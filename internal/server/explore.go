package server

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sapedb/sapedb/internal/number"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
)

// Operating a database over the wire.
//
// The shell needs something no ordinary connection may do: run an access
// nobody declared. A connection string cannot say so — it signs what to reach,
// not what you may do once there — and a client that claims it about itself
// has claimed nothing.
//
// So the server challenges, and an operator answers with proof that it holds
// the server's own secret. Whoever holds that can already read the files and
// mint any connection string they like, so this hands out no power that was
// not already there. What it adds is that everything done this way is in the
// change log with a name against it, which reading the files is not.

// ErrNotOperator is an explore or a declare on a connection that has not
// proved itself. One sentinel for both, so codeFor keeps answering
// "not_operator" whichever frame was refused — a client switches on the code,
// and two codes for one reason would be two things to handle.
var ErrNotOperator = errors.New("sapedb/server: this connection may not explore or declare; prove the server secret first")

// elevating is the answer to the challenge in the welcome.
type elevating struct {
	Proof string `json:"proof"`
}

// exploring is one typed access, and which database to run it against.
type exploring struct {
	Access store.Access `json:"access"`

	// Catalogue asks what the database holds instead of reading any of it.
	// The shell needs it first: an operator who does not know the collection
	// names cannot type an access at all.
	Catalogue bool `json:"catalogue,omitempty"`

	// DBName and Signature are how an account-wide connection says which
	// database, exactly as a call does. Proving you can operate the server
	// does not say which database you meant.
	DBName    string `json:"dbname,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// explored is what comes back: the rows, and the operation this would have to
// be declared as.
type explored struct {
	Result store.Result    `json:"result"`
	Draft  store.Operation `json:"draft"`

	Here *store.Catalogue `json:"here,omitempty"`
}

// elevate checks the proof and marks the connection.
func (s *Server) elevate(live *session, payload []byte) error {
	asked := elevating{}
	if err := json.Unmarshal(payload, &asked); err != nil {
		return fmt.Errorf("sapedb/server: the proof does not read as one: %w", err)
	}

	// Checked against the challenge this connection was given, so a proof
	// recorded from another connection is not one.
	if !signing.Operates(asked.Proof, s.options.Secret, live.challenge) {
		return fmt.Errorf("%w: the proof does not answer this connection's challenge", ErrNotOperator)
	}

	live.operator = true
	return nil
}

// explore runs a typed access, for a connection that has proved it may.
func (s *Server) explore(live *session, payload []byte) ([]byte, error) {
	if !live.operator {
		return nil, ErrNotOperator
	}
	// Both halves of this handler record a ChangeRead entry — store.Explore
	// after the rows, store.WhatIsHere after the catalogue — so on a follower
	// both are writes, however much they read like reads. Refusing them costs
	// a follower its operator shell, which is a real loss said out loud in
	// CHANGELOG rather than hidden behind a handler that looks harmless.
	if err := s.readOnly("looking around is recorded in the change log, and a follower writes no entries of its own"); err != nil {
		return nil, err
	}

	// Read without rounding and checked, for the same reason a declaration is
	// (ISS-35). A typed access carries constants in two places — the key a
	// `get` names, and the values a scan's bounds are built from — and both
	// are `any`, so both used to arrive already rounded. A `get` on
	// 9007199254740993 silently fetched the document under 9007199254740992,
	// which is the worst shape this bug has: the shell answers with a row, and
	// the row is somebody else's.
	//
	// The draft that comes back is the operation this access would have to be
	// declared as, so an access that rounds also hands the operator a
	// declaration that rounds, to paste into a schema file. Refusing here is
	// what stops the bug being copied forward.
	asked := exploring{}
	if err := unrounded(payload, &asked); err != nil {
		return nil, fmt.Errorf("sapedb/server: the access does not read as one: %w", err)
	}
	if err := number.ExactIn(fmt.Sprintf("access %q", asked.Access.Kind), &asked.Access); err != nil {
		return nil, err
	}

	// Reached exactly as a call reaches a database: being an operator says
	// what you may do, never which database you may do it to.
	db, err := s.reach(live, call{DBName: asked.DBName, Signature: asked.Signature})
	if err != nil {
		return nil, err
	}

	db.mutex.Lock()
	defer db.mutex.Unlock()

	// Named by the account, because that is the only identity this connection
	// has proved. It is what goes in the log against everything the shell does.
	caller := store.Caller{Actor: live.opening.Account + " (operator)"}

	if asked.Catalogue {
		here, err := db.store.WhatIsHere(caller)
		if err != nil {
			return nil, err
		}
		db.notify()
		return json.Marshal(explored{Here: &here})
	}

	draft, result, err := db.store.Explore(caller, asked.Access)
	if err != nil {
		return nil, err
	}

	// Exploring writes a log entry, so whoever is following this database
	// hears about it like anything else.
	db.notify()

	return json.Marshal(explored{Result: result, Draft: draft})
}
