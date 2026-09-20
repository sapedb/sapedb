package server

import (
	"encoding/json"
	"fmt"

	"github.com/sapedb/sapedb/internal/store"
)

// Declaring an operation on a server that is already running.
//
// Until this frame existed there was no way to. store.DeclareOperation could
// only be reached through `sapedb apply`, and that command opens the database
// file directly and takes the exclusive lock — its own package doc says a run
// while the server is up is refused. So adding an operation meant stopping the
// server, applying, and starting it again: every live connection dropped, for
// a change that writes one key.
//
// This is the same declaration by the same function, sent down a connection
// that already exists. What it is not is a second, looser way in:
//
//   - Only an operator may send it, proved the same way Explore is — by
//     answering the welcome's challenge with the server's own secret. A
//     connection string says which database to reach, never what may be
//     declared once there.
//   - It calls store.DeclareOperation, which is the function `sapedb apply`
//     calls, which begins by calling validateOperation. There is no second
//     copy of the rules here and nothing is normalised on the way in. That is
//     the one property this file exists to keep, and it is measured rather
//     than asserted — see TestDeclaringOverTheWireRefusesExactlyWhatApplyRefuses,
//     TestAScanDeclaredOverTheWireMustSayHowManyRowsItMayReturn and
//     TestACountDeclaredOverTheWireMustSayHowFarItWalks.
//
// The warning is next door. internal/store's Explore says it checks a typed
// access "with the same validation a declaration gets", and for the limit rule
// that has never been true: asOperation forces Limit to MostRows when it is
// missing or too large, BEFORE validateOperation runs, so neither "a scan must
// declare how many rows it may return" nor "a count must declare how far it
// walks" can ever fire through Explore. That is the right answer for a shell —
// an operator who did not say is not asking for everything — and the wrong
// answer for a declaration, which is a promise about cost that somebody has to
// keep. Nothing here caps anything.

// declaring is one operation to store, and which database to store it in.
type declaring struct {
	Operation store.Operation `json:"operation"`

	// DBName and Signature are how an account-wide connection says which
	// database, exactly as a call does. Proving you can operate the server
	// does not say which database you meant.
	DBName    string `json:"dbname,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// declared is what comes back: the operation as it was stored, carrying the
// version it was given.
//
// Declaring a name that is already declared writes a new version and leaves
// every older one readable, which is store.DeclareOperation's own behaviour
// and not something this path changes. The version is the whole answer a
// caller needs: it is what an Invoke passes to run this exact declaration
// later, after somebody has declared over the top of it.
type declared struct {
	Operation store.Operation `json:"operation"`
}

// declare stores an operation, for a connection that has proved it may.
func (s *Server) declare(live *session, payload []byte) ([]byte, error) {
	if !live.operator {
		return nil, ErrNotOperator
	}
	// Before the payload is even read: a declaration is an entry in the log,
	// and a follower mints none. Refused for an operator too — holding the
	// server's secret says what you may do, not what this database is.
	if err := s.readOnly("declaring an operation writes to the change log"); err != nil {
		return nil, err
	}

	asked := declaring{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return nil, fmt.Errorf("sapedb/server: the declaration does not read as one: %w", err)
	}

	// Reached exactly as a call reaches a database: being an operator says
	// what you may do, never which database you may do it to.
	db, err := s.reach(live, call{DBName: asked.DBName, Signature: asked.Signature})
	if err != nil {
		return nil, err
	}

	// The write lock, not the read one. This writes a key and commits, so it
	// is a writer by every measure store.SharedRead uses to decide the
	// question for an Invoke — and unlike an Invoke there is nothing to look
	// up first that would make a read lock worth taking on the way.
	db.mutex.Lock()
	defer db.mutex.Unlock()

	// Named by the account, exactly as explore does, and for the same reason:
	// that is the only identity this connection has proved. It is written here
	// rather than read out of the payload — a `declaring` has no field for it,
	// and if it had one it would be a name the caller chose for itself.
	caller := store.Caller{Actor: live.opening.Account + " (operator)"}

	stored, err := db.store.DeclareOperation(caller, asked.Operation)
	if err != nil {
		// DeclareOperation validates before it writes, so the ordinary
		// refusal has left nothing behind. The one that has is the narrow
		// window after tree.Put and before record returns: half a
		// declaration, uncommitted, on a database the next caller would
		// commit for us. Rolling back here costs nothing on the common path —
		// there is no transaction open to undo — and is the difference
		// between a refusal and a refusal that leaves damage.
		if back := db.store.Rollback(); back != nil {
			return nil, fmt.Errorf("%w (and rolling it back failed: %v)", err, back)
		}
		return nil, err
	}

	// DeclareOperation writes the key and the change-log entry; committing is
	// the caller's, exactly as it is for `sapedb apply`. Without this the
	// declaration is real for this connection and gone at the next restart.
	if err := db.store.Commit(); err != nil {
		return nil, err
	}

	// A declaration is a change to the database like any other, so whoever is
	// following it hears about it.
	db.notify()

	return json.Marshal(declared{Operation: stored})
}
