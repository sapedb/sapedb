package server

import (
	"errors"
	"fmt"

	"github.com/sapedb/sapedb/internal/store"
)

// A follower is a server whose databases are copies of somebody else's.
//
// Two things make that true, and both live here. One is Apply: an entry from
// another database's log is put into this one keeping its own number, so the
// copy's log IS the log it followed rather than a second log that happens to
// describe the same documents. The other is that a follower takes no writes of
// its own — every request that would put a new entry in the log is refused by
// name.
//
// Refusing is not politeness. A follower that accepted one write would mint an
// entry number of its own, and from that moment its numbers and the leader's
// mean different things: the next entry the leader sends is refused as out of
// order (store.ErrOutOfOrder) and the copy stops following, or — worse, if
// anything ever papered over that — the two logs quietly disagree about what
// entry 91 was. Replication here is asynchronous and one-directional, so there
// is nowhere for a write made on this side to go.
//
// What counts as a write is wider than it looks, and the surprise is worth
// saying out loud rather than leaving to be discovered: reading the catalogue
// and running an operator's typed access both record a ChangeRead entry (see
// store.WhatIsHere and store.Explore — an audit trail that stops at the
// primary is one nobody can read), and an entry is an entry. So both are
// refused on a follower too. Invoking a declared operation that only reads is
// not refused, because it records nothing.

// ErrReadOnly is a write sent to a follower.
//
// It is a sentinel with a code of its own, not a generic refusal, because the
// caller's answer to it is specific: go and write to the leader. A client that
// had to tell this apart from "that operation does not exist" by reading prose
// would be a client that stops telling them apart the day the prose improves.
var ErrReadOnly = errors.New("sapedb/server: this database follows another one and takes no writes; write to the leader it follows")

// IsReadOnly says whether this server refuses writes.
//
// It exists so that a follower can check the server it is about to apply into
// really is one — the single configuration where the two sides' entry numbers
// can come to mean different things is a follower applying into a server that
// also takes writes of its own, and it looks healthy until the first one.
func (s *Server) IsReadOnly() bool { return s.options.ReadOnly }

// readOnly refuses when this server is a follower, naming what was refused.
func (s *Server) readOnly(what string) error {
	if !s.options.ReadOnly {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrReadOnly, what)
}

// Apply puts one entry of another database's log into this one, and commits.
//
// The position and the change are one write, which is the whole reason this
// method exists rather than a follower doing the two steps itself. store.Apply
// puts the entry, whatever it did, the caller's write id and the log counter
// into the same tree, and the Commit here is what makes all of it durable
// together. There is no cursor kept beside the data that could be saved after
// a crash lost the change, or before one lost the position: the position IS
// the log counter, and asking where to resume is store.LatestLSN.
//
// An entry already applied is ignored by store.Apply and one out of order is
// refused, so a follower that reconnects and is sent entries it has already
// seen — the ordinary case, because it resumes from what it has, not from what
// the leader thinks it has — applies each exactly once.
//
// A failure rolls back rather than leaving half of an entry in the pages: a
// database left with pending writes refuses everything afterwards (see
// runBatch), which would turn one bad entry into a dead server.
func (s *Server) Apply(account, name string, change store.Change) error {
	db, err := s.database(account, name)
	if err != nil {
		return err
	}

	db.mutex.Lock()
	defer db.mutex.Unlock()

	at, err := db.store.LatestLSN()
	if err != nil {
		return err
	}
	// An entry already held is not applied and so has nothing to commit, and
	// the commit is the expensive part — a sync. store.Apply would ignore it
	// too; what this avoids is the sync of an empty transaction, once per
	// entry, every time a follower reconnects and is sent what it already has.
	if change.LSN <= at {
		return nil
	}
	if err := db.store.Apply(change); err != nil {
		_ = db.store.Rollback()
		return err
	}
	if err := db.store.Commit(); err != nil {
		_ = db.store.Rollback()
		return err
	}

	// Whoever is following this copy hears about it like anything else: a
	// follower serves the same log it followed, so a subscription to one is
	// worth the same as a subscription to the leader.
	db.notify()
	return nil
}

// FollowingAt is where a followed database has got to, which is where a
// subscription to the leader has to resume from — this, plus one.
//
// It is a method on the server rather than something a follower works out from
// store.LatestLSN itself so that opening the database, locking it and reading
// the counter happen the one way they happen everywhere else here.
func (s *Server) FollowingAt(account, name string) (uint64, error) {
	db, err := s.database(account, name)
	if err != nil {
		return 0, err
	}

	db.mutex.Lock()
	defer db.mutex.Unlock()
	return db.store.LatestLSN()
}
