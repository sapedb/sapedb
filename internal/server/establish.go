package server

import (
	"encoding/json"
	"fmt"

	"github.com/sapedb/sapedb/internal/store"
)

// Declaring a collection on a server that is already running.
//
// The Declare frame next door did this for an operation, and stopped there.
// So a running database could be given new vocabulary and could not be given
// anywhere to put anything: adding a collection still meant stopping the
// server and running `sapedb apply`, which opens the database file directly
// and takes the exclusive lock.
//
// Half a hole is worse than a whole one here, because of what the missing half
// blocks. An External Operation that solves a whole problem through the shapes
// it publishes is not one operation — it is a module, and a module arrives
// with somewhere to keep its data (a collection, its indexes, its rollups),
// the vocabulary to reach it (operations) and the shapes it publishes. With
// only the operation half declarable over the wire, such a module could not be
// installed into an empty database at all. It could only be a handful of
// operations pointed at somebody else's collections.
//
// This is the same declaration by the same function, sent down a connection
// that already exists, and it is not a second, looser way in:
//
//   - Only an operator may send it, proved the same way Explore and Declare
//     are — by answering the welcome's challenge with the server's own secret.
//     A connection string says which database to reach, never what may be
//     declared once there.
//   - It calls store.Declare, which is the function `sapedb apply` calls,
//     which begins with spec.validate(). There is no second copy of the rules
//     here and nothing is normalised on the way in. That property is measured
//     rather than asserted — internal/cli's
//     TestEstablishingOverTheWireRefusesExactlyWhatApplyRefuses runs one table
//     down both paths against one database and demands the same sentence.
//
// What it does NOT do is version anything. Declaring an operation name that
// already exists writes a new version and leaves every older one runnable,
// because callers are built against a version. A collection has no version to
// give: it is where the documents physically are, and there is one of those.
// So establishing a name that already exists brings that collection up to
// date in place — new indexes are built over the documents already stored,
// indexes no longer listed take their entries with them — and the changes that
// cannot be made in place (the primary key, how the collection is divided, an
// index that keeps its name and changes its shape) are refused in the store's
// own words. That is store.Declare's behaviour, measured before it was chosen
// rather than after; see TestEstablishingAnExistingCollectionUpgradesItInPlace.

// establishing is one collection declaration, and which database to put it in.
type establishing struct {
	Spec store.Spec `json:"spec"`

	// DBName and Signature are how an account-wide connection says which
	// database, exactly as a call does. Proving you can operate the server
	// does not say which database you meant.
	DBName    string `json:"dbname,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// established is the collection as it now stands.
//
// It is read back off the store rather than echoed from the request, and that
// is the whole reason it is worth sending: store.Declare assigns the
// collection its id, and assigns an id to every index and rollup it adds,
// keeping the ones an earlier declaration already gave. A caller that
// establishes over an existing collection gets back what actually took effect,
// which is not always what it asked for — an index it did not mention is gone
// from this answer, and one it re-sent keeps the id it already had.
type established struct {
	Spec store.Spec `json:"spec"`
}

// establish stores a collection, for a connection that has proved it may.
func (s *Server) establish(live *session, payload []byte) ([]byte, error) {
	if !live.operator {
		return nil, ErrNotOperator
	}

	asked := establishing{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return nil, fmt.Errorf("sapedb/server: the declaration does not read as one: %w", err)
	}

	// Reached exactly as a call reaches a database: being an operator says
	// what you may do, never which database you may do it to.
	db, err := s.reach(live, call{DBName: asked.DBName, Signature: asked.Signature})
	if err != nil {
		return nil, err
	}

	// The write lock, not the read one — and here it is not a formality. A
	// declaration that adds an index walks every document already stored and
	// writes an entry for each, which is the heaviest write this server
	// serves. Running it under store.SharedRead would be handing a reader a
	// collection whose index is half built.
	db.mutex.Lock()
	defer db.mutex.Unlock()

	// Named by the account, exactly as declare and explore do, and for the
	// same reason: that is the only identity this connection has proved. It
	// is written here rather than read out of the payload — an `establishing`
	// has no field for it, and if it had one it would be a name the caller
	// chose for itself.
	caller := store.Caller{Actor: live.opening.Account + " (operator)"}

	collection, err := db.store.Declare(caller, asked.Spec)
	if err != nil {
		// Declare validates before it writes, so the ordinary refusal has
		// left nothing behind. The ones that have not are real: a
		// redeclaration that drops a rollup and then trips over an index has
		// already deleted the rollup's rows, and a first declaration that
		// fails inside install has already taken a collection id. Rolling
		// back costs nothing on the common path — there is no transaction
		// open to undo — and is the difference between a refusal and a
		// refusal that leaves damage.
		if back := db.store.Rollback(); back != nil {
			return nil, fmt.Errorf("%w (and rolling it back failed: %v)", err, back)
		}
		return nil, err
	}

	// Declare writes the catalogue key, the index entries and the change-log
	// entry; committing is the caller's, exactly as it is for `sapedb apply`.
	// Without this the collection is real for this connection and gone at the
	// next restart.
	if err := db.store.Commit(); err != nil {
		return nil, err
	}

	// A declaration is a change to the database like any other, so whoever is
	// following it hears about it.
	db.notify()

	return json.Marshal(established{Spec: collection.Spec()})
}
