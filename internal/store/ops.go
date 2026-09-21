package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/sapedb/sapedb/internal/keys"
)

// Declared operations are the only way into a database.
//
// There is no query on the wire. A caller names an operation that is already
// stored here and passes arguments to it, and the operation says exactly which
// collection it touches, which index it walks, how far, what it returns and
// who may run it. Everything a query would decide at runtime is decided when
// the operation is declared, which has two consequences worth the whole
// design:
//
//   - Injection has nowhere to happen. An argument is a value in a bound or a
//     field of a document; it can never become part of the access path,
//     because the access path was fixed before the argument existed.
//   - The cost is known before anything runs. A scan without a declared limit
//     is a scan whose cost nobody has worked out, so declaring one is refused
//     rather than defaulted — a default there is a promise made on behalf of
//     whoever has to serve it.
//
// Operations are versioned and old versions are kept. Changing one writes a
// new version rather than replacing the old, so an audit record that says
// which operation ran can still be read back years later.
const spaceOps byte = 0x04 // 0x04 | name | 0x00 | version -> the declaration

// What an operation does. There is no general expression language on purpose:
// each of these has a cost that can be described in a sentence.
const (
	ActionGet    = "get"    // one document by its primary key
	ActionScan   = "scan"   // a stretch of one index
	ActionCount  = "count"  // the same stretch, counted rather than returned
	ActionTotals = "totals" // a declared rollup, read
	ActionInsert = "insert" // a new document; refused if the key is taken
	ActionPut    = "put"    // a document, replacing one under the same key
	ActionUpdate = "update" // named fields of an existing document
	ActionDelete = "delete" // one document by its primary key
	ActionBatch  = "batch"  // several of the above, as one transaction

	// ActionHashRange is the tenth, and the first thing in this store that
	// WALKS a range at run time to produce something other than the rows it
	// walked: the values of a stretch of keys, in key order, joined with
	// nothing between them, answered as the SHA-256 of the join.
	//
	// Its output is thirty-two bytes whatever the input was, which is the
	// whole reason it can exist before a size ceiling does. What it costs is
	// therefore invisible in what it answers, and that is why its declared
	// limit is counted while it walks rather than taken on trust: see
	// hashRange in digest.go.
	ActionHashRange = "hashRange"

	// ActionDeleteRange is the eleventh, and the second thing in this store
	// that walks a range at run time — the first that WRITES while it walks:
	// a stretch of keys removed in key order, up to the number the
	// declaration says.
	//
	// The name is the one thing about it most likely to mislead, so the cost
	// is written here rather than left to be discovered. This is not a
	// truncation of a keyspace and it is not cheap. Removing one document
	// reads it (its index entries are derived from its contents), deletes
	// every one of those entries, adjusts every rollup it contributed to,
	// removes the document, and records a change. So one run costs
	//
	//	limit × (one document read + its index entries + its rollup
	//	         contribution + one log entry)
	//
	// which is linear in limit with every unit bounded — which is exactly
	// what makes it declarable, and exactly why the log grows by one entry
	// per row rather than by one per call. See deleteRange in
	// deleterange.go.
	ActionDeleteRange = "deleteRange"
)

// How a value is read before it reaches the digest, as an operation writes it
// down. A declared enumeration, in the same family as an index field's
// Missing — a choice written into the declaration, never an expression a
// caller composes.
//
// It exists because binary is stored as base64 text today: a string cannot
// carry arbitrary bytes, and the byte 0xFF put through one comes back as
// U+FFFD. Hashing the values as stored would answer the SHA-256 of a base64
// transcript, which can never equal the hash a client computed from the
// original file — two correct systems disagreeing forever, and the
// disagreement looking exactly like corruption. A real bytes type will make
// this unnecessary for new data and it stays useful for the data that is
// already base64.
const (
	DecodeNone   = ""       // the value as stored, hashed as its UTF-8 bytes
	DecodeBase64 = "base64" // standard base64 text, decoded before it is hashed
)

// ClusteredIndex is the name for walking documents in primary-key order, which
// is the order they are stored in.
const ClusteredIndex = "_key"

// Which way a scan runs along its index, as an operation writes it down.
const (
	DirectionForward = "forward" // the order the index declared
	DirectionReverse = "reverse" // that order, read back to front
)

var (
	ErrNoOperation = errors.New("sapedb/store: no such operation")
	ErrArgument    = errors.New("sapedb/store: the arguments do not match what the operation declares")
	ErrNotAllowed  = errors.New("sapedb/store: the caller may not run this operation")
	ErrExists      = errors.New("sapedb/store: a document already has that primary key")
	ErrMissing     = errors.New("sapedb/store: the document this step needs is not there")
	ErrCondition   = errors.New("sapedb/store: the document is not in the state this operation requires")
	ErrUncommitted = errors.New("sapedb/store: a batch needs a database with nothing half-written in it")

	// ErrDigest is a document in the range that cannot go into the digest —
	// the field is missing, it is not text, or it does not read as what the
	// declaration says it is encoded in.
	//
	// It refuses rather than skips, and the precedent is the rollup: a total
	// that silently skips what it could not add is a total nobody can trust.
	// A digest with a hole in it is worse, because it is thirty-two bytes
	// that look exactly as authoritative as a correct thirty-two bytes.
	ErrDigest = errors.New("sapedb/store: a document in this range cannot go into the digest")

	// ErrCeiling is a walk that reached the row limit its operation declares.
	//
	// Separate from every other refusal here because it is the one that says
	// the answer would have been RIGHT and is not being given: at the ceiling
	// a hashRange stops and refuses rather than hand back the digest of a
	// prefix, which is indistinguishable from the digest of the whole range.
	ErrCeiling = errors.New("sapedb/store: the walk reached the row limit this operation declares")
)

// Operation is a declaration: everything about a call except its arguments.
type Operation struct {
	Name       string      `json:"name"`
	Collection string      `json:"collection"`
	Action     string      `json:"action"`
	Input      []Parameter `json:"input,omitempty"`

	// Key is which document, for the actions that work on exactly one.
	Key *Term `json:"key,omitempty"`

	// Rollup is which declared total to read, for an operation that reads one.
	Rollup string `json:"rollup,omitempty"`

	// Index, From and To are the access path of a scan: which index, and where
	// along it to start and stop. Nothing else may narrow a scan, so nothing
	// else can be smuggled into one.
	Index string    `json:"index,omitempty"`
	From  *Endpoint `json:"from,omitempty"`
	To    *Endpoint `json:"to,omitempty"`

	// Direction is which way along that index the scan runs: "forward", the
	// index's own order, or "reverse", that order read back to front. Absent
	// is forward.
	//
	// It is a Term rather than a plain string so that one mechanism covers
	// both ways of deciding it — a constant term fixes the direction in the
	// declaration, an argument term lets the caller choose per call — and both
	// are already the vocabulary everything else here is written in.
	//
	// From is the low end of the stretch and To is the high end, in BOTH
	// directions — Direction changes only the order rows come back in, never
	// which rows they are. One declaration read both ways is therefore one
	// stretch, read from either end: a caller that flips the direction and
	// leaves From and To alone gets the same rows back, backwards.
	//
	// This is a change from an older convention some callers may still carry
	// in their head, where a reversed read meant writing From as the high
	// value and To as the low one. Doing that today is refused, in EITHER
	// direction, because From is unconditionally the low end regardless of
	// what value is written there: a From that sorts after To is not a
	// stretch that happens to be empty, it is a declaration that could never
	// return a row with any argument, and task 0045 changed this from a
	// silent empty answer — indistinguishable on the wire from a stretch that
	// is legitimately empty — into an error instead. It is caught as early as
	// the shape allows: at DeclareOperation, if both ends are constants
	// (ErrDeclaration, validateOperation below); at Invoke, if either end is
	// an argument or a step's key (ErrArgument, stretch() in scan.go), since
	// only then does a value for it exist to compare. Pinning both ends to
	// the same point, with Exclusive making one of them not quite equal to
	// the other, is a different shape — a caller-intended empty range — and
	// that one still runs; see stretch()'s own doc for exactly where the line
	// between the two is.
	//
	// It is still the one thing about a scan an argument may decide, and it is
	// safe for a reason that does not generalise to anything else: it widens
	// nothing. The index is the declared one, the limit is the declared one,
	// From and To are the declared ones — Direction is read nowhere while the
	// two bounds are being worked out, so there is no declaration shaped in a
	// way that lets a caller-chosen direction reach a row a forward call of
	// the same declaration could not. Earlier rounds tried to get there by
	// refusing every shape where that could go wrong instead — a constant at
	// one end only, the two ends disagreeing about Exclusive, the two ends
	// written at different widths — and each round's rule caught the leak it
	// was aimed at and missed the next one. There is no such rule now, because
	// there is nothing left for one to catch.
	//
	// Limit is the one place a caller-chosen direction still matters, and it is
	// meant to: with a limit smaller than the stretch, the two directions hand
	// back the two different ends of it — the ten most recent rows and the ten
	// oldest are different sets on purpose. That is what this feature exists to
	// do, not a widening of the declaration.
	//
	// "Reverse" reverses the order the index declared, as a whole. An index on
	// (a ascending, b descending) read in reverse gives (a descending, b
	// ascending), not (a descending, b descending) — that last one is a third
	// order and still needs an index of its own. The word is deliberately not
	// "descending", which is what a single field is.
	Direction *Term `json:"direction,omitempty"`

	// Document is what an insert or a put writes; Set is what an update
	// changes. Both are built from arguments and constants and nothing else.
	Document map[string]Term `json:"document,omitempty"`
	Set      map[string]Term `json:"set,omitempty"`

	// Steps are what a batch does, in order and in one transaction.
	Steps []Step `json:"steps,omitempty"`

	// Field is which field of each document a hashRange digests, as a path
	// the same shape a projection uses. Read by nothing else: an action that
	// writes one down and is not a hashRange is refused, because a word
	// nothing reads is how a caller ends up believing a promise nobody made.
	Field string `json:"field,omitempty"`

	// Decode is how the value at Field is read before it reaches the digest:
	// DecodeBase64, or absent for the bytes of the string as stored. Also
	// read only by a hashRange, and refused elsewhere for the same reason as
	// Field.
	Decode string `json:"decode,omitempty"`

	// Projection is the fields a read returns. Empty returns the document.
	Projection []string `json:"projection,omitempty"`

	// Limit is the most rows a scan may return. Required for a scan: it is the
	// declaration of what this operation costs.
	Limit int `json:"limit,omitempty"`

	// Scopes are what a caller must hold. Checked fail-closed: an operation
	// that declares a scope is refused to a caller that does not present it.
	Scopes []string `json:"scopes,omitempty"`

	// Version is assigned when the operation is declared, so a declaration
	// being written — a schema file, or the draft the shell prints — does not
	// carry one. Omitted rather than zero, because a file saying version 0
	// claims something nobody gave it.
	Version int `json:"version,omitempty"`

	// DeclaredBy is the identity that declared THIS version, assigned by the
	// store from the Caller in the same breath as Version and for the same
	// reason: it is a fact about the act of declaring, not a field of the
	// declaration somebody wrote. Whatever a caller sends here is discarded —
	// a field a client can set is a field a client can lie in.
	//
	// Optional, and a pointer so that absent is a state the wire can hold. It
	// has to be: every operation declared before this field existed is stored
	// without it, and reading one back must not invent a declarer it never
	// had. An absent value stays legal for the life of 1.x.
	//
	// Redeclaring records the identity that made the new version. Older
	// versions keep theirs, because old versions are kept rather than
	// replaced — which is the whole reason an audit record naming an operation
	// version is still readable years later, and this field is what makes that
	// record say who as well as what.
	//
	// It survives a dump and a restore, unlike the change log's Attribution,
	// because a dump carries the Operation itself. That is the point of
	// putting it here. See declarer.go.
	DeclaredBy *Declarer `json:"declaredBy,omitempty"`
}

// Parameter is one declared argument.
type Parameter struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Default  any    `json:"default,omitempty"`
}

// Term is where a value comes from: an argument of the call, a constant
// written into the declaration, or what an earlier step of a batch produced.
// Exactly one of the three.
type Term struct {
	Arg   string `json:"arg,omitempty"`
	Value any    `json:"value,omitempty"`
	// Constant distinguishes a declared value of null from no value at all,
	// which JSON alone cannot.
	Constant bool `json:"constant,omitempty"`

	// Step names an earlier step of the same batch, and Field says what of it
	// — only "key" for now, which is what an order needs to give its payment.
	//
	// This is the whole of the dataflow between steps, and it is deliberately
	// this small: anything richer is an expression language, and an expression
	// is the one thing a caller is promised never to have to write. The engine
	// underneath could carry more; this field is where that promise is kept.
	Step  string `json:"step,omitempty"`
	Field string `json:"field,omitempty"`
}

// Step is one part of a batch.
//
// A step either touches a collection — Action and Collection, with the fields
// under them — or calls an operation that is already declared — Operation,
// Version and With. Never both, and the refusal for writing both is at declare
// time. The two shapes are one struct because they are one list: the steps of
// a batch run in order, in one transaction, and which of the two a step is
// changes nothing about that.
type Step struct {
	// Name lets a later step refer to what this one produced.
	Name       string `json:"name,omitempty"`
	Action     string `json:"action,omitempty"`
	Collection string `json:"collection,omitempty"`

	// Operation is the name of an already-declared operation this step runs,
	// and Version is which declaration of it — pinned, never the latest.
	//
	// Pinning is not caution, it is what makes two things true that nothing
	// else here makes true:
	//
	//   - The cost this operation declares stays what it says. A reference to
	//     a bare name would follow the callee to its next version, so the day
	//     somebody redeclares the callee with a bigger limit, the number
	//     written in THIS declaration quietly stops being the ceiling. The
	//     promise this store is built on is that the cost is known before
	//     anything runs, and a number that changes when a different file
	//     changes is not that.
	//   - Recursion cannot be written down. A version is handed out by
	//     DeclareOperation and only ever goes up, and a pinned reference only
	//     resolves to a version that already exists — so a@1 referring to b@2
	//     needs b@2 declared first, and a cycle would need the opposite as
	//     well. The reference graph is a DAG by construction, the way the
	//     steps of one batch are already ordered by construction (see earlier,
	//     in validateOperation). Nothing detects cycles here because nothing
	//     can build one, and for the same reason there is no maximum depth:
	//     depth buys no risk that the ceiling rule does not already cover.
	//
	// Version is required and must be greater than zero. Zero means "the
	// latest" everywhere else in this package, and that is exactly the meaning
	// this field may not have.
	Operation string `json:"operation,omitempty"`
	Version   int    `json:"version,omitempty"`

	// With is the arguments handed to that operation, by the names the callee
	// declares them under. The terms are the ordinary ones — an argument of
	// the caller, a constant, or an earlier step's key — and the last of those
	// is the one that is limited: see validateComposedStep.
	With map[string]Term `json:"with,omitempty"`

	Key      *Term           `json:"key,omitempty"`
	Document map[string]Term `json:"document,omitempty"`
	Set      map[string]Term `json:"set,omitempty"`

	// Exists says the document this step names must, or must not, already be
	// there. Absent means it is not checked.
	Exists *bool `json:"exists,omitempty"`

	// Require are conditions the document must already satisfy. They are how a
	// caller that read something a moment ago over the network says "only if
	// it is still that way" — the whole of optimistic locking, and the reason
	// this store needs no interactive transaction to do the classic
	// order-payment-ledger flow safely.
	Require []Condition `json:"require,omitempty"`
}

// Condition is one thing that must already be true of a document.
type Condition struct {
	Path string `json:"path"`
	// Equals is the value the field must have. Absent says it must not be
	// there at all. Exactly one of the two.
	Equals *Term `json:"equals,omitempty"`
	Absent bool  `json:"absent,omitempty"`
}

// Endpoint is one end of a scan: values for the first fields of the index, and
// whether that point is included.
//
// From is always the low end and To is always the high end, whichever way
// Direction reads the stretch, so Exclusive means the same thing at both ends
// in both directions: this point is not part of the stretch.
//
// A From that sorts after To is refused rather than read as an empty stretch
// — see Operation.Direction and Range (scan.go) for where and why. Pinning
// From and To to the same values, with Exclusive telling them apart, is not
// that: it is how an empty stretch is written on purpose, and it keeps
// running.
type Endpoint struct {
	Terms     []Term `json:"terms"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// normalizeEndpoint keeps Terms a wire-safe []Term rather than the nil Go
// hands back when a hand-authored schema.json's "from"/"to" omits "terms"
// entirely (e.g. `{"exclusive": true}`) — a legal way to declare a bound end
// with no constraint, reached only through DeclareOperation, since Explore's
// own asOperation always builds an Endpoint with make() (see endpoint() in
// explore.go). Terms carries no `omitempty`, so an unnormalized nil marshals
// as JSON `null`: an Operation declared that way and later read back through
// WhatIsHere or Explored.Draft would hand a caller `"terms":null` where the
// TypeScript mirror's own comment promises "never absent" and reaches
// straight for `.map()`. Called from validateOperation so both stores
// (DeclareOperation) and runs (Explore) see the same normalized shape before
// anything is marshaled.
func normalizeEndpoint(e *Endpoint) {
	if e != nil && e.Terms == nil {
		e.Terms = []Term{}
	}
}

// Caller is the context of one call: who is running the operation, what they
// hold, and their own id for the write if it is one.
type Caller struct {
	Scopes []string
	// Actor is recorded with any change this call makes, so the log says who
	// as well as what.
	Actor string
	// WriteID is the caller's own id for this write. The same id arriving twice
	// is answered from what the first one did rather than applied again, which
	// is what makes a retry after a lost connection safe.
	WriteID string
	// Signer is the ed25519 public key, in hex, of the bundle this call
	// arrived inside — set by `sapedb install` and by nothing else. It is the
	// identity a namespace claim is recorded against when it is present, and
	// it is a separate field from Actor rather than a differently-spelled
	// Actor because the two are different kinds of fact: this one was checked,
	// by a signature over exactly the declarations it carried, and Actor is a
	// name somebody was called at the time. See namespace.go.
	Signer string
}

// Result is what running an operation produced.
type Result struct {
	Operation string           `json:"operation"`
	Version   int              `json:"version"`
	Rows      []map[string]any `json:"rows,omitempty"`
	Count     int              `json:"count,omitempty"`
	Key       any              `json:"key,omitempty"`
	Changed   int              `json:"changed,omitempty"`
	// Truncated says the scan stopped at its declared limit and there was
	// more. A caller that does not look at this is reading a partial answer as
	// a whole one, so it is a field rather than a silence.
	//
	// A deleteRange sets it for the same reason and with the same meaning:
	// it removed the number it was allowed to remove and there were more
	// keys in the stretch. Together with Key — the last key it removed — it
	// is the cursor the next call starts after, which is what lets a
	// thousand rows go in pages. This is the field that makes a stopped
	// deleteRange distinguishable from a finished one, and it is why that
	// action stops and says so where a hashRange stops and refuses: a digest
	// has nowhere to carry "there was more", and this does.
	Truncated bool `json:"truncated,omitempty"`
	// Repeated is the log entry a write id had already produced, when this call
	// was a retry of a write that had already landed. Nothing was written
	// again.
	Repeated uint64 `json:"repeated,omitempty"`

	// Digest is what a hashRange answered: the SHA-256 of the values it
	// walked, joined with nothing between them, written as lower-case hex.
	//
	// Hex rather than base64 so that it can be compared by eye, and by
	// paste, with what `sha256sum` and every client's own SHA-256 prints —
	// which is the entire point of the action. Count rides alongside it and
	// says how many rows were read to get there: the digest is thirty-two
	// bytes however far the walk went, so the cost is not readable from the
	// answer unless something says it.
	//
	// Never partial. A hashRange that reached its declared ceiling returns an
	// error and no Digest at all, because a digest of part of a range is
	// indistinguishable from a digest of all of it.
	Digest string `json:"digest,omitempty"`
}

// DeclareOperation stores an operation, as a new version if the name is
// already declared.
//
// The caller is here for one reason: the change log says who. Every other
// entry in that log has carried an Attribution since there was a log — a
// write names the operation that made it and the actor it ran for, and even a
// read through the shell names "explore" and the operator. A declaration
// carried none. That was tolerable while declaring meant `sapedb apply`,
// which means holding the file's exclusive lock, which means the server is
// stopped and whoever did it was standing at the machine. It stopped being
// tolerable with the Declare frame: an operation can now be declared from
// another host, over a live connection, against a running server — and the
// one entry in the log that records the most consequential kind of change,
// the one that decides what everybody else may do, was the one entry with
// nobody's name on it.
//
// What the actor is worth differs by path and neither path pretends
// otherwise. Over the wire it is the account whose connection string reached
// this database, and it is written by the server rather than sent by the
// client. Through `sapedb apply` it is the account the command was pointed
// at, which is a label rather than a proof: running apply means holding the
// database file, and anybody holding the file could have written the bytes by
// hand. In both cases it is the best name available at that point, which is
// what an audit trail is made of.
func (s *Store) DeclareOperation(caller Caller, operation Operation) (Operation, error) {
	if err := s.validateOperation(&operation); err != nil {
		return Operation{}, err
	}

	// Who may declare into this namespace, before anything is written. A name
	// in the unnamed namespace — every flat name there has ever been — passes
	// straight through here and keeps the behaviour the collision tests
	// measure: the newest version wins and nobody is warned. A named namespace
	// belongs to whoever declared into it first. See namespace.go.
	if err := s.claimFor(caller, operation.Name); err != nil {
		return Operation{}, err
	}

	latest, found, err := s.Operation(operation.Name, 0)
	if err != nil && !errors.Is(err, ErrNoOperation) {
		return Operation{}, err
	}
	operation.Version = 1
	if found {
		operation.Version = latest.Version + 1
	}
	// Assigned here rather than taken from what arrived, and assigned
	// unconditionally — including to nil, when the caller names nobody — so
	// that a declaration which turns up with the field already filled cannot
	// keep it. Same rule as Version on the line above: both are the store's
	// account of the act, and a caller writing either one would be writing
	// their own audit record.
	operation.DeclaredBy = declarerOf(caller)

	encoded, err := json.Marshal(operation)
	if err != nil {
		return Operation{}, err
	}
	if err := s.tree.Put(operationKey(operation.Name, operation.Version), encoded); err != nil {
		return Operation{}, err
	}
	// "declare" rather than the operation's own name: the Attribution says
	// which operation DID this, and what did this was the act of declaring.
	// The operation being declared is already on the entry, in Operation.
	if _, err := s.record(Change{
		Kind:      ChangeOperation,
		Operation: &operation,
		By:        Attribution{Operation: "declare", Actor: caller.Actor},
	}); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// Operation reads a declaration back. Version zero is the latest.
func (s *Store) Operation(name string, version int) (Operation, bool, error) {
	if version > 0 {
		value, found, err := s.tree.Get(operationKey(name, version))
		if err != nil || !found {
			if err == nil {
				err = fmt.Errorf("%w: %q version %d", ErrNoOperation, name, version)
			}
			return Operation{}, false, err
		}
		operation := Operation{}
		if err := json.Unmarshal(value, &operation); err != nil {
			return Operation{}, false, fmt.Errorf("%w: operation %q: %v", ErrDamaged, name, err)
		}
		return operation, true, nil
	}

	// The versions of one name sit together and their numbers are written big
	// end first, so the last one along is the newest.
	prefix := operationPrefix(name)
	var newest []byte
	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		newest = append(newest[:0], value...)
		return true
	})
	if err != nil {
		return Operation{}, false, err
	}
	if newest == nil {
		return Operation{}, false, fmt.Errorf("%w: %q", ErrNoOperation, name)
	}

	operation := Operation{}
	if err := json.Unmarshal(newest, &operation); err != nil {
		return Operation{}, false, fmt.Errorf("%w: operation %q: %v", ErrDamaged, name, err)
	}
	return operation, true, nil
}

// Operations is every declared name, with the newest version of each.
func (s *Store) Operations() ([]Operation, error) {
	prefix := []byte{spaceOps}
	byName := map[string]Operation{}
	var order []string

	err := s.tree.Ascend(prefix, func(key, value []byte) bool {
		if !bytes.HasPrefix(key, prefix) {
			return false
		}
		operation := Operation{}
		if err := json.Unmarshal(value, &operation); err != nil {
			return false
		}
		if _, seen := byName[operation.Name]; !seen {
			order = append(order, operation.Name)
		}
		byName[operation.Name] = operation
		return true
	})
	if err != nil {
		return nil, err
	}

	all := make([]Operation, 0, len(order))
	for _, name := range order {
		all = append(all, byName[name])
	}
	return all, nil
}

func (s *Store) validateOperation(operation *Operation) error {
	// Every declared scan/rollup Endpoint gets here before it is stored
	// (DeclareOperation) or run (Explore), so normalizing here reaches both
	// paths and reaches it before json.Marshal ever sees the operation. See
	// normalizeEndpoint.
	normalizeEndpoint(operation.From)
	normalizeEndpoint(operation.To)

	if err := usableName(operation.Name); err != nil {
		return fmt.Errorf("operation name: %w", err)
	}
	// The namespace half of the name, checked here rather than in
	// DeclareOperation so that Explore cannot hand back a draft declaration
	// that could never be declared. usableName runs first and keeps its rules
	// exactly: the 128-byte ceiling is on the WHOLE name, separator and
	// namespace included, because raising a published limit quietly is a
	// different promise from the one 1.0 makes. See namespace.go.
	if _, _, err := SplitOperationName(operation.Name); err != nil {
		return fmt.Errorf("operation name: %w", err)
	}

	collection, err := s.Collection(operation.Collection)
	if err != nil {
		return err
	}

	parameters := map[string]Parameter{}
	for _, parameter := range operation.Input {
		if err := usableName(parameter.Name); err != nil {
			return fmt.Errorf("argument name: %w", err)
		}
		if _, seen := parameters[parameter.Name]; seen {
			return fmt.Errorf("%w: two arguments are called %q", ErrDeclaration, parameter.Name)
		}
		switch parameter.Type {
		case TypeString, TypeNumber, TypeBool, TypeAny:
		default:
			return fmt.Errorf("%w: argument %q is declared %q", ErrDeclaration, parameter.Name, parameter.Type)
		}
		if parameter.Default != nil && !matches(parameter.Type, parameter.Default) {
			return fmt.Errorf("%w: the default for %q is not a %s", ErrDeclaration, parameter.Name, parameter.Type)
		}
		if parameter.Required && parameter.Default != nil {
			return fmt.Errorf("%w: %q is required and also has a default", ErrDeclaration, parameter.Name)
		}
		parameters[parameter.Name] = parameter
	}

	// earlier is the steps already declared, so a reference forward or to
	// itself is refused where it is written rather than found at call time.
	// It carries what each of those steps offers the ones after it rather
	// than a bare "it exists", because a reference to a step has to be
	// checked against something: see stepOffer.
	earlier := map[string]stepOffer{}

	check := func(term Term, wanted string, where string) error {
		sources := 0
		if term.Arg != "" {
			sources++
		}
		if term.Value != nil || term.Constant {
			sources++
		}
		if term.Step != "" {
			sources++
		}
		if sources != 1 {
			return fmt.Errorf("%w: %s must be exactly one of an argument, a value, or an earlier step", ErrDeclaration, where)
		}

		if term.Step != "" {
			offer, comes := earlier[term.Step]
			if !comes {
				return fmt.Errorf("%w: %s uses step %q, which does not come before it", ErrDeclaration, where, term.Step)
			}
			if term.Field != "key" {
				return fmt.Errorf("%w: %s asks a step for %q, and a step gives only its key", ErrDeclaration, where, term.Field)
			}
			// N4, and it is the rule that keeps the promise rather than
			// tidying the code.
			//
			// A step runs exactly once. Taking a value from a step that may
			// hand back many rows is how that stops being true: "run this once
			// for each row that one returned" is a for loop, written in JSON,
			// and a for loop is the first half of an expression language. What
			// breaks is not the engine's dignity but a promise somebody relies
			// on: the ceiling would stop being a sum and
			// become a product, and a product of three fifties is a hundred
			// and twenty five thousand rows behind three numbers none of which
			// makes a reader look twice.
			//
			// Said plainly, because the refusal has to be defensible to
			// somebody who just hit it: today a step offers one key, so a leg
			// with a ceiling above one has at best the LAST of its keys to
			// give, and naming it is ambiguous even before it is dangerous.
			// The refusal is written against the ceiling rather than against
			// that ambiguity on purpose. Field is limited to "key" today and
			// widening it is the obvious next request; a rule keyed to the
			// ambiguity would fall away the day that happens, silently, and
			// the loop would arrive with it. This one does not.
			if offer.ceiling > 1 {
				return fmt.Errorf("%w: %s names step %q, which may return %d rows — a step runs once, and taking a value from a step that returns more than one row is asking to run once for each of them",
					ErrDeclaration, where, term.Step, offer.ceiling)
			}
			if offer.keyType == "" {
				return fmt.Errorf("%w: %s names step %q, which hands back no key — a get and a scan both read documents without leaving a key behind, so there is nothing there to take",
					ErrDeclaration, where, term.Step)
			}
			// The same check the argument branch below makes, for the same
			// reason, and it used to be missing here: this branch returned as
			// soon as it had found the step, so a key taken from a step was
			// the one value in a declaration whose type nobody compared with
			// the place it was being used. A step reading a collection keyed
			// by string, handing that key to a step on a collection keyed by
			// number, was accepted at declare time and then sorted somewhere
			// else in the index at call time — an empty answer with no error,
			// which is exactly the outcome the comment below says this kind of
			// check exists to prevent.
			if wanted != TypeAny && offer.keyType != TypeAny && offer.keyType != wanted {
				return fmt.Errorf("%w: %s is a %s, and step %q gives a %s",
					ErrDeclaration, where, wanted, term.Step, offer.keyType)
			}
			return nil
		}

		if term.Arg == "" {
			if !matches(wanted, term.Value) {
				return fmt.Errorf("%w: %s is a %s, and the value is %T", ErrDeclaration, where, wanted, term.Value)
			}
			return nil
		}
		parameter, found := parameters[term.Arg]
		if !found {
			return fmt.Errorf("%w: %s uses %q, which is not an argument of this operation", ErrDeclaration, where, term.Arg)
		}
		// Checked here so that it cannot fail at call time: an argument whose
		// type does not fit where it is used would sort somewhere else in the
		// index, and the caller would get an empty answer with no error.
		if wanted != TypeAny && parameter.Type != TypeAny && parameter.Type != wanted {
			return fmt.Errorf("%w: %s is a %s, and %q is declared %s", ErrDeclaration, where, wanted, term.Arg, parameter.Type)
		}
		return nil
	}

	switch operation.Action {
	case ActionGet, ActionDelete, ActionUpdate:
		if operation.Key == nil {
			return fmt.Errorf("%w: a %s says which document by its key", ErrDeclaration, operation.Action)
		}
		if err := check(*operation.Key, collection.spec.Key.Type, "the key"); err != nil {
			return err
		}
		if operation.Action == ActionUpdate && len(operation.Set) == 0 {
			return fmt.Errorf("%w: an update changes nothing", ErrDeclaration)
		}
		for field, term := range operation.Set {
			if err := check(term, TypeAny, "the field "+field); err != nil {
				return err
			}
		}

	case ActionTotals:
		rollup, found := collection.rollup(operation.Rollup)
		if !found {
			return fmt.Errorf("%w: %q of %q", ErrNoRollup, operation.Rollup, operation.Collection)
		}
		fields := rollup.Group
		for _, endpoint := range []*Endpoint{operation.From, operation.To} {
			if endpoint == nil {
				continue
			}
			if len(endpoint.Terms) > len(fields) {
				return fmt.Errorf("%w: %d bound values for a rollup grouped by %d fields",
					ErrDeclaration, len(endpoint.Terms), len(fields))
			}
			for i, term := range endpoint.Terms {
				if err := check(term, fields[i].Type, fmt.Sprintf("the bound on %q", fields[i].Path)); err != nil {
					return err
				}
				// A rollup's Group fields cannot be declared "any" —
				// Rollup.validate() only accepts TypeString, TypeNumber and
				// TypeBool for them — so matches() already refuses a slice
				// or a map here before this line is reached. Kept anyway,
				// deliberately, for the same reason sameTerm replaced ==
				// in totalsAcross below rather than only in scanAcross:
				// symmetry with the scan branch, so a future field type
				// that does allow "any" on a rollup group does not silently
				// reopen the panic this closed on the scan side.
				//
				// Correction (task 0054, caught by QA — the paragraph above
				// used to end "there is no declaration in this repo that
				// makes this call return a non-nil error today," and that
				// claim was too wide, kept here rather than deleted so the
				// narrower one below does not drift back open the same way:
				// the TypeAny ban only closes the SLICE/MAP path.
				// matches(TypeNumber, ...) (store.go) accepts int and int64
				// as well as float64, and keys.Encode refuses an int64
				// magnitude beyond 2^53 (ErrTooLarge) or a float64 NaN
				// (ErrNotANumber) — neither is a slice or a map, so a
				// TypeNumber group field with one of those two boundary
				// values DOES reach this line and DOES return a non-nil
				// error today. Measured:
				// TestARollupBoundaryNumberNamesItsOwnField
				// (lockstep_test.go).
				if err := constantIsEncodable(term, fields[i]); err != nil {
					return err
				}
			}
		}
		if err := refusedBackwardsRange(operation.From, operation.To, fields); err != nil {
			return err
		}
		if operation.Limit <= 0 {
			return fmt.Errorf("%w: reading a rollup must declare how many rows it may return", ErrDeclaration)
		}
		if err := totalsAcross(collection, rollup, operation); err != nil {
			return err
		}

	case ActionScan, ActionCount, ActionHashRange, ActionDeleteRange:
		// Key order, and only key order, for a deleteRange, for reasons that
		// are not the hashRange's below and land in the same place.
		//
		// An index is a second order over the same rows, and removing a
		// document removes every index entry it has — so "delete this
		// stretch of by_author" is not a stretch of documents at all: an
		// array index spreads one document over several entries, so a
		// stretch of it can name half a document, and a tie in an index is
		// no order within itself, so which rows a limit stops at would
		// depend on nothing the declaration wrote down. Key order is the one
		// order in which "up to N of them, and here is the last one" is a
		// sentence with one meaning — which is also what makes the answer a
		// cursor somebody can page with.
		if operation.Action == ActionDeleteRange &&
			operation.Index != "" && operation.Index != ClusteredIndex {
			return fmt.Errorf("%w: a deleteRange removes a stretch of %q in key order, and %q is an index — removing a document removes all of its entries, an array index spreads one document over several of them, and a tie in an index has no order of its own, so which rows a limit stopped at would not be decided by this declaration",
				ErrDeclaration, operation.Collection, operation.Index)
		}
		// Key order, and only key order, for a hashRange. The answer is the
		// SHA-256 of the values joined with nothing between them, so the
		// order they are joined in IS the answer — and an index is a second
		// order over the same rows, chosen per declaration. Two of the
		// store's own index shapes make it worse than merely different: an
		// array index spreads one document into several entries, so the same
		// value would be fed to the digest more than once, and an index over
		// a field several documents share gives no order at all within a tie.
		// "The hash of this stretch of keys" is a sentence with one answer;
		// "the hash of this stretch of some index" is not.
		if operation.Action == ActionHashRange &&
			operation.Index != "" && operation.Index != ClusteredIndex {
			return fmt.Errorf("%w: a hashRange walks %q in key order, and %q is an index — an index is a second order over the same rows, and an array index visits one document more than once, so the digest would depend on which order was picked rather than on the bytes",
				ErrDeclaration, operation.Collection, operation.Index)
		}
		fields, err := scanFields(collection, operation.Index)
		if err != nil {
			return err
		}
		for _, endpoint := range []*Endpoint{operation.From, operation.To} {
			if endpoint == nil {
				continue
			}
			if len(endpoint.Terms) > len(fields) {
				return fmt.Errorf("%w: %d bound values for an index of %d fields", ErrDeclaration, len(endpoint.Terms), len(fields))
			}
			for i, term := range endpoint.Terms {
				if err := check(term, fields[i].Type, fmt.Sprintf("the bound on %q", fields[i].Path)); err != nil {
					return err
				}
				if err := constantIsEncodable(term, fields[i]); err != nil {
					return err
				}
			}
		}
		if err := refusedBackwardsRange(operation.From, operation.To, fields); err != nil {
			return err
		}
		// Only a scan may carry one at all — see the refusal below — so a count
		// that wrote a direction down should hear about that rather than about
		// whether the word it chose was spelled right.
		if operation.Action == ActionScan {
			if err := checkDirection(operation, parameters, check); err != nil {
				return err
			}
		}
		// Both of them, now. A scan declares how many rows it may return; a
		// count returns none, so what it has to declare is how far it walks.
		//
		// Until this rule covered the count, a count was the only action that
		// could be declared without declaring what it costs — the limit was
		// asked for on a scan, on a rollup read and on a composed batch, and
		// a count slipped between them. That made "every declaration in this
		// store says what it costs" a sentence with one exception in it, and
		// an exception reachable by writing `"action": "count"` and leaving
		// one field out. The walk is the whole cost of a count: a count over
		// forty million documents returns the number 40000000 and the caller
		// finds out how big the collection was by waiting, which is exactly
		// what a declared limit exists to stop.
		//
		// compose.go's ceiling() has asked a count for a limit since composed
		// operations landed. It is kept rather than folded into this one,
		// because it reads declarations already on disk — including ones
		// written before this line existed.
		//
		// All three of them, now. A hashRange is the one where the limit does
		// the most work and the least of it is visible: a scan's cost is
		// roughly the rows it hands you and a count's answer at least grows
		// with the walk, while a hashRange answers thirty-two bytes whether
		// it read four rows or forty million. Nothing about what comes back
		// says what it cost, so the declaration is the only place it can be
		// said.
		//
		// All four of them, now. A deleteRange is the one where the number
		// is not a ceiling on what comes back but a ceiling on what is
		// destroyed, and where the cost is not only this call's: each row
		// removed writes a change-log entry that every follower replays and
		// that no operator can cap today (ISS-23). "How many rows may this
		// remove" is therefore the single most consequential number in this
		// store, and there is no honest default for it — a defaulted one
		// would be this store deciding on somebody's behalf how much of
		// their data goes.
		if operation.Limit <= 0 {
			if operation.Action == ActionCount {
				return fmt.Errorf("%w: a count must declare how far it walks — it hands back a number rather than rows, so the walk is the whole of what it costs", ErrDeclaration)
			}
			if operation.Action == ActionHashRange {
				return fmt.Errorf("%w: a hashRange must declare how far it walks — it hands back thirty-two bytes whatever it read, so the walk is the whole of what it costs and the answer never reveals it", ErrDeclaration)
			}
			if operation.Action == ActionDeleteRange {
				return fmt.Errorf("%w: a deleteRange must declare how many rows it may remove — the number bounds what is destroyed and how many log entries every follower replays, and there is no default this store may pick on an operator's behalf", ErrDeclaration)
			}
			return fmt.Errorf("%w: a scan must declare how many rows it may return", ErrDeclaration)
		}
		// The same refusal, in the same words, that a scan, a count and a
		// hashRange already get — deliberately matched rather than decided
		// again. The ticket flagged "may one call cross a partition
		// boundary" as a decision rather than an implementation detail, and
		// it is a decision this store already made: a walk that crosses
		// partitions is several indexes one after another, which is key
		// order for a time-partitioned collection and hash order — no order
		// — for a hash-partitioned one. A deleteRange needs that order for a
		// stronger reason than a scan does, not a weaker one: without it,
		// "up to N in key order, and here is the last key" is not a cursor
		// and a second page would remove rows from wherever the hash
		// happened to put them.
		if err := scanAcross(collection, operation); err != nil {
			return err
		}
		// A `Limit < 0` refusal used to sit here, after scanAcross, and it was
		// the only thing that caught a negative limit on a count. The rule
		// above now covers every limit that is not positive, for both actions,
		// so that branch had become unreachable — and an unreachable refusal
		// is a rule a reader believes is doing work.

		if operation.Action == ActionHashRange {
			if operation.Field == "" {
				return fmt.Errorf("%w: a hashRange must say which field of each document it digests", ErrDeclaration)
			}
			switch operation.Decode {
			case DecodeNone, DecodeBase64:
			default:
				return fmt.Errorf("%w: a hashRange decodes %q or nothing, not %q — it is a word written into the declaration, and this store has no other encoding to offer",
					ErrDeclaration, DecodeBase64, operation.Decode)
			}
			// A projection is fields a read hands back, and a hashRange hands
			// back a digest. Refused rather than ignored: a totals ignores one
			// and that is a wart this action does not have to inherit, because
			// nothing has declared a hashRange yet and so nothing is being
			// tightened on anybody.
			if len(operation.Projection) > 0 {
				return fmt.Errorf("%w: a hashRange hands back a digest rather than rows, so a projection on one names fields nobody will ever be shown", ErrDeclaration)
			}
		}

		// The same rule, for the same reason, for the action that hands back
		// a count and a cursor. Refused rather than ignored while nothing
		// has declared one yet.
		if operation.Action == ActionDeleteRange && len(operation.Projection) > 0 {
			return fmt.Errorf("%w: a deleteRange removes rows rather than returning them, so a projection on one names fields nobody will ever be shown", ErrDeclaration)
		}

	case ActionBatch:
		if len(operation.Steps) == 0 {
			return fmt.Errorf("%w: a batch does nothing", ErrDeclaration)
		}
		within := newCosts()
		total, composed := 0, false
		for i := range operation.Steps {
			step := &operation.Steps[i]
			offer, err := s.validateStep(step, i, operation, within, check)
			if err != nil {
				return err
			}
			if step.Operation != "" {
				composed = true
			}
			total += offer.ceiling
			if step.Name != "" {
				if _, taken := earlier[step.Name]; taken {
					return fmt.Errorf("%w: two steps are called %q", ErrDeclaration, step.Name)
				}
				earlier[step.Name] = offer
			}
		}

		// N5, and it is what makes this readable by a person rather than
		// merely computable by this function.
		//
		// A batch of plain steps has always cost one document per step, and a
		// reader counts the steps. A batch that calls operations does not: the
		// cost is somewhere else, in files this one only names. So an
		// operation that calls one declares its own limit and that limit must
		// cover the sum — which, by induction over the declarations it names
		// (each of which was held to the same rule when IT was declared),
		// makes the ceiling of a composed operation at any depth the number
		// written in it. Not a product. Not a sum anybody has to work out. The
		// same one number a flat scan declares, meaning the same thing.
		//
		// Only required when a step calls an operation: a plain batch has
		// never declared a limit and making it start now would refuse every
		// declaration already written down, to buy a number the reader can
		// already get by counting.
		if composed {
			if operation.Limit <= 0 {
				return fmt.Errorf("%w: %q calls other operations, so it must declare how many rows it may return — the cost is in declarations this one only names",
					ErrDeclaration, operation.Name)
			}
			if total > operation.Limit {
				return fmt.Errorf("%w: the steps of %q may return %d rows between them, and it declares a limit of %d",
					ErrDeclaration, operation.Name, total, operation.Limit)
			}
		}

	case ActionInsert, ActionPut:
		if len(operation.Document) == 0 {
			return fmt.Errorf("%w: an %s writes nothing", ErrDeclaration, operation.Action)
		}
		for field, term := range operation.Document {
			wanted := TypeAny
			if field == collection.spec.Key.Path {
				wanted = collection.spec.Key.Type
			}
			if err := check(term, wanted, "the field "+field); err != nil {
				return err
			}
		}
		if _, writesKey := operation.Document[collection.spec.Key.Path]; !writesKey && collection.spec.Key.Auto == "" {
			return fmt.Errorf("%w: %q does not generate keys, so the operation must write %q",
				ErrDeclaration, collection.spec.Name, collection.spec.Key.Path)
		}

	default:
		return fmt.Errorf("%w: %q is not something an operation can do", ErrDeclaration, operation.Action)
	}

	// A direction is a word in a declaration, and a word nothing reads is how a
	// caller ends up believing a promise nobody made. Only a scan reads one.
	//
	// A count walks an index, so it looked at first like it should be allowed
	// one — but a count hands back a number, and min(rows, limit) is the same
	// number from either end, Truncated with it. That is the same reason
	// totals is refused, so it gets the same answer.
	if operation.Direction != nil && operation.Action != ActionScan {
		why := "does not walk an index, so it has no direction"
		switch operation.Action {
		case ActionCount:
			why = "hands back a number, and that number is the same from either end"
		case ActionHashRange:
			// Not the same reason as a count's, and the difference is the
			// whole point of the action: reversing a hashRange would change
			// the answer, because the order the values are joined in IS the
			// answer. A direction here would be a caller deciding which of
			// two digests of the same bytes they wanted, which is exactly
			// what "how a file was split does not change its hash" is for.
			why = "joins its values in key order, and reading them back to front is a different digest of the same bytes"
		case ActionDeleteRange:
			// The sharpest version of this rule, because here the word does
			// not change what comes back — it changes which rows still
			// exist. With a limit smaller than the stretch, the two
			// directions remove the two opposite ends of it, so a
			// caller-chosen direction would be a caller deciding which half
			// of somebody's data goes. That is precisely the decision a
			// declaration exists to have already made.
			why = "removes rows in key order, and reading the stretch back to front under a limit would let the caller choose which end of it is destroyed"
		}
		return fmt.Errorf("%w: a %s %s", ErrDeclaration, operation.Action, why)
	}

	// Field and Decode are read by a hashRange and by nothing else, so an
	// action that writes one down has written a word this store will not
	// read. The same rule as the direction above, for the same reason: a
	// declaration whose words are quietly ignored is a promise nobody made.
	if operation.Action != ActionHashRange {
		if operation.Field != "" {
			return fmt.Errorf("%w: a %s does not digest a field, so %q names something nothing here reads", ErrDeclaration, operation.Action, operation.Field)
		}
		if operation.Decode != "" {
			return fmt.Errorf("%w: a %s decodes nothing, so %q names something nothing here reads", ErrDeclaration, operation.Action, operation.Decode)
		}
	}

	for _, path := range operation.Projection {
		if path == "" {
			return fmt.Errorf("%w: the projection names a field with no path", ErrDeclaration)
		}
	}
	return nil
}

// checkDirection refuses a direction that could not be worked out, at the point
// where it is written rather than at the point where somebody reads rows in an
// order they did not expect.
func checkDirection(operation *Operation, parameters map[string]Parameter,
	check func(Term, string, string) error) error {

	term := operation.Direction
	if term == nil {
		return nil
	}
	if err := check(*term, TypeString, "the direction"); err != nil {
		return err
	}

	if term.Arg == "" {
		if _, err := directionOf(term.Value); err != nil {
			return fmt.Errorf("%w: the direction: %v", ErrDeclaration, err)
		}
		return nil
	}

	// check has already said the argument is declared and is a string.
	parameter := parameters[term.Arg]

	// A direction that depends on whether an argument turned up is two orders
	// wearing one name, and the caller who leaves it out cannot tell which one
	// it got. Refused here rather than defaulted at call time, for the same
	// reason a scan must declare a limit.
	if !parameter.Required && parameter.Default == nil {
		return fmt.Errorf("%w: %q chooses the direction, so it must be required or carry a default — a scan whose order is optional has two orders",
			ErrDeclaration, term.Arg)
	}
	if parameter.Default != nil {
		if _, err := directionOf(parameter.Default); err != nil {
			return fmt.Errorf("%w: the default for %q: %v", ErrDeclaration, term.Arg, err)
		}
	}
	return nil
}

// stepOffer is what one step of a batch offers the steps that come after it.
//
// keyType is the declared type of the key a later step may name with
// {"step": ..., "field": "key"}. It is the whole reason this is a struct
// rather than the bool it used to be: a step reference is a value like any
// other, and a value has to be checked against the place it is used.
type stepOffer struct {
	keyType string
	// ceiling is the most rows this step may hand back, read from what it
	// declares — one for a step that touches a collection, and the called
	// operation's own ceiling for a step that calls one. It is what N4 is
	// asked about (see check, in validateOperation) and what N5 adds up.
	ceiling int
}

// validateStep checks one step of a batch against the collection it names, and
// says what it offers the steps after it.
func (s *Store) validateStep(step *Step, at int, parent *Operation, within *costs,
	check func(Term, string, string) error) (stepOffer, error) {

	where := fmt.Sprintf("step %d", at+1)
	if step.Name != "" {
		where = fmt.Sprintf("step %q", step.Name)
	}

	if step.Operation != "" {
		return s.validateComposedStep(step, where, parent, within, check)
	}

	collection, err := s.Collection(step.Collection)
	if err != nil {
		return stepOffer{}, fmt.Errorf("%s: %w", where, err)
	}

	// Every action a step may take touches exactly one document, which is why
	// a step has never needed a limit of its own and why the cost of a plain
	// batch is read by counting its steps.
	offer := stepOffer{keyType: collection.spec.Key.Type, ceiling: 1}

	switch step.Action {
	case ActionGet, ActionUpdate, ActionDelete:
		if step.Key == nil {
			return stepOffer{}, fmt.Errorf("%w: %s says which document by its key", ErrDeclaration, where)
		}
		if err := check(*step.Key, collection.spec.Key.Type, where+" key"); err != nil {
			return stepOffer{}, err
		}
		if step.Action == ActionUpdate && len(step.Set) == 0 {
			return stepOffer{}, fmt.Errorf("%w: %s changes nothing", ErrDeclaration, where)
		}
		for field, term := range step.Set {
			if err := check(term, TypeAny, where+" field "+field); err != nil {
				return stepOffer{}, err
			}
		}

	case ActionInsert, ActionPut:
		if len(step.Document) == 0 {
			return stepOffer{}, fmt.Errorf("%w: %s writes nothing", ErrDeclaration, where)
		}
		for field, term := range step.Document {
			wanted := TypeAny
			if field == collection.spec.Key.Path {
				wanted = collection.spec.Key.Type
			}
			if err := check(term, wanted, where+" field "+field); err != nil {
				return stepOffer{}, err
			}
		}
		if _, writes := step.Document[collection.spec.Key.Path]; !writes && collection.spec.Key.Auto == "" {
			return stepOffer{}, fmt.Errorf("%w: %q does not generate keys, so %s must write %q",
				ErrDeclaration, collection.spec.Name, where, collection.spec.Key.Path)
		}

	default:
		return stepOffer{}, fmt.Errorf("%w: %s does %q, which a step cannot do", ErrDeclaration, where, step.Action)
	}

	for i, condition := range step.Require {
		if condition.Path == "" {
			return stepOffer{}, fmt.Errorf("%w: %s condition %d names no field", ErrDeclaration, where, i+1)
		}
		if (condition.Equals == nil) == !condition.Absent {
			return stepOffer{}, fmt.Errorf("%w: %s condition %d must be either a value it equals or absent", ErrDeclaration, where, i+1)
		}
		if condition.Equals != nil {
			if err := check(*condition.Equals, TypeAny, fmt.Sprintf("%s condition on %q", where, condition.Path)); err != nil {
				return stepOffer{}, err
			}
		}
	}

	return offer, nil
}

// scanFields is the fields of the index an operation walks, primary key
// included as the clustered one.
func scanFields(collection *Collection, name string) ([]Field, error) {
	if name == ClusteredIndex || name == "" {
		// Same fake field walkRange (scan.go) builds for the same reason, and
		// the same non-choice: Missing only matters for keys.Absent, and a
		// document's primary key is never absent. MissingSkip here is the
		// zero value, not a decision that changes what this declares.
		return []Field{{
			Path:    collection.spec.Key.Path,
			Type:    collection.spec.Key.Type,
			Missing: MissingSkip,
		}}, nil
	}

	index, found := collection.index(name)
	if !found {
		return nil, fmt.Errorf("%w: %q of %q", ErrNoIndex, name, collection.spec.Name)
	}
	return index.Fields, nil
}

// constantIsEncodable refuses a constant bound that keys cannot turn into
// bytes, at the point it is written rather than at the point every call to
// this operation panics trying to.
//
// An index field, or a rollup group field, may be declared "any", and matches
// lets a constant of any type satisfy that — including a slice or a map read
// off a JSON body, which keys.Encode refuses with ErrNotIndexable rather than
// encoding. A constant is the one kind of term this can be checked for ahead
// of time: an argument or a step reference has no value yet, so this function
// never sees it. Its value at call time is whatever the caller or the earlier
// step produced, and bound() (scan.go) is where that value first meets
// keys.Encode. A value that fails there is refused with ErrArgument rather
// than left as the bare error keys.Encode returns — it is the caller's
// mistake, discovered at the one place that can see it, not a fault of the
// server's that the constant path here was already closed against.
func constantIsEncodable(term Term, field Field) error {
	if term.Arg != "" || term.Step != "" {
		return nil
	}
	if _, err := keys.Encode(nil, term.Value, field.encoding()); err != nil {
		return fmt.Errorf("%w: the bound on %q holds a constant keys cannot encode: %v", ErrDeclaration, field.Path, err)
	}
	return nil
}

// refusedBackwardsRange is task 0045's other half of the same refusal
// stretch() (scan.go) makes at call time: a From that sorts after To, caught
// here instead when there is nothing left that could still change it — both
// ends are written as constants, not as an argument or a step's key, so the
// values ARE the whole of what this declaration will ever compare. A
// half-hearted variant that also fired on an argument end would be checking
// against whatever placeholder is in Term.Value at declare time, which
// nobody wrote there and which has no relationship to what a caller passes —
// that is exactly the "nothing to check yet" case only stretch() can catch,
// once an argument's actual value exists. This function and stretch() are
// deliberately two call sites of one calculation (boundAt, scan.go) rather
// than two rules that happen to agree today: sameTerm, above, is what a
// second hand-kept rule costs when the thing it is standing in for grows a
// field nobody remembered to add.
//
// Only runs when both ends are present at all — an operation that leaves
// From or To unbounded has nothing here for a caller-shaped mistake to widen
// against, the same reason stretch() guards on upper != nil.
func refusedBackwardsRange(from, to *Endpoint, fields []Field) error {
	if from == nil || to == nil {
		return nil
	}
	if !allConstant(from.Terms) || !allConstant(to.Terms) {
		return nil
	}

	// The values here already passed constantIsEncodable above, at every
	// call site of this function — so an error out of boundAt would mean
	// this ran before that check rather than after it, and that is a bug in
	// the caller, not a shape this declaration should be refused for. Nothing
	// converts it to ErrDeclaration; it is returned as-is so a mistake in the
	// calling order surfaces as the wrong error rather than a silently
	// swallowed one.
	lower, err := boundAt(nil, fields, endpointBound(from), false)
	if err != nil {
		return err
	}
	upper, err := boundAt(nil, fields, endpointBound(to), true)
	if err != nil {
		return err
	}
	if bytes.Compare(lower, upper) > 0 || backwardsBoolRange(fields, endpointBound(from), endpointBound(to)) {
		return fmt.Errorf("%w: the from bound sorts after the to bound, so this operation would never return a row with any argument; From is the low end and To is the high end, in both directions",
			ErrDeclaration)
	}
	return nil
}

// allConstant is true when every term of an endpoint is a constant — the one
// case refusedBackwardsRange has enough information to judge at declare
// time. A single argument or step term among the rest means at least one
// value does not exist yet.
func allConstant(terms []Term) bool {
	for _, term := range terms {
		if term.Arg != "" || term.Step != "" {
			return false
		}
	}
	return true
}

// endpointBound turns a declared Endpoint's constant terms into the *Bound
// boundAt expects — the same shape bounds() (invoke.go) builds from an
// endpoint's terms at call time, written a second time here because that one
// resolves arguments this function has none of yet.
func endpointBound(endpoint *Endpoint) *Bound {
	bound := &Bound{Exclusive: endpoint.Exclusive}
	for _, term := range endpoint.Terms {
		bound.Values = append(bound.Values, term.Value)
	}
	return bound
}

// sameTerm compares two terms without the panic == risks. Term.Value is any,
// and a term may hold a constant of a type an index field declared "any"
// accepts — a slice or a map, which Go cannot compare with ==. reflect.
// DeepEqual has no such limit.
//
// It compares the whole struct rather than listing Arg, Constant, Step and
// Field by name next to a DeepEqual of Value alone, which is what this used
// to do and which is a list somebody has to remember to extend. Term has five
// fields today; the day a sixth is added, a hand-kept list stays silent about
// it and quietly starts calling two different terms "the same" — and the
// place that costs is scanAcross/totalsAcross below, which is what a
// same-term test is standing in for a half-pinned scan or rollup across a
// partitioned collection: exactly the shape those two functions exist to
// refuse. DeepEqual on the struct covers a new field the moment it exists
// rather than the moment somebody remembers this function — with the one
// caveat that is true of DeepEqual generally, not particular to this change:
// per its own documentation, two non-nil func values are never deeply equal
// to each other, only to nil. Term.Value could in principle hold a func; if
// it ever does, this reports two such terms as different rather than
// comparing them by identity, which is the safe side of the bug this
// replaced (silently calling two different terms "the same"), not the unsafe
// one.
func sameTerm(a, b Term) bool {
	return reflect.DeepEqual(a, b)
}

func operationPrefix(name string) []byte {
	key := append([]byte{spaceOps}, name...)
	return append(key, 0)
}

func operationKey(name string, version int) []byte {
	key := operationPrefix(name)
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(version))
	return append(key, raw[:]...)
}

// scanAcross refuses a scan of a partitioned collection that would come back
// in an order nobody asked for.
//
// An index on a partitioned collection is local to its partition — it has to
// be, or dropping a partition would stop being an unlink — so a scan that
// crosses partitions is the results of several indexes one after another.
// Whether that is the right order depends on the collection, and it is
// knowable here, which is the only place it should ever be decided.
//
// Time: partition names sort in the order the periods happened, and the key is
// a ulid, so partition order is key order. Every index entry ends with the key.
// So the concatenation is correctly ordered as long as the declared fields of
// the index are all fixed and the only thing varying is the key. A scan that
// leaves a declared field free would return, say, every account of January
// before every account of February.
//
// Hash: partition order is hash order, which is no order at all. Nothing that
// crosses partitions can be ordered, so nothing may.
//
// A direction changes none of this. Reversing a sequence that is in key order
// leaves it in key order — that part holds unconditionally, and it is why a
// scan that may not run forward may not run backward either, and why there is
// nothing extra to refuse here and nothing extra to allow.
//
// What is conditional is whether the sequence is in key order in the first
// place, and that is this function's business rather than the walk's. It
// compares Terms as they are written, and a bound whose argument is optional
// falls away entirely at call time — so a scan this function passed as "one
// account, every partition" can still run as "every account, one partition
// after another". That hole is the same in both directions and predates the
// direction; see task 0016.
func scanAcross(collection *Collection, operation *Operation) error {
	divided := collection.spec.Partition
	if divided == nil {
		return nil
	}

	where := fmt.Sprintf("%q is divided %s", collection.spec.Name, describePartition(divided))

	// Four actions reach this now — scan, count, hashRange and deleteRange —
	// so the sentence says "an ordered walk" rather than "a scan". The rule
	// is unchanged and deliberately so: whether one call may cross a
	// partition boundary was decided here once, and each action that walks
	// inherits that answer rather than arguing it again. It binds hardest on
	// the deleteRange, where the order is what makes the last key removed a
	// cursor rather than an arbitrary row.
	if divided.By == ByHash {
		return fmt.Errorf("%w: %s, so an ordered walk of it would run over partitions in hash order, which is no order; reach it by key",
			ErrDeclaration, where)
	}

	// The clustered index is the key itself, and partition order is key order,
	// so walking every partition in turn is exactly key order.
	if operation.Index == "" || operation.Index == ClusteredIndex {
		return nil
	}

	index, found := collection.index(operation.Index)
	if !found {
		return fmt.Errorf("%w: %q of %q", ErrNoIndex, operation.Index, collection.spec.Name)
	}

	if operation.From == nil || operation.To == nil ||
		len(operation.From.Terms) != len(index.Fields) || len(operation.To.Terms) != len(index.Fields) {
		return fmt.Errorf("%w: %s, so a scan on %q must fix all %d of its fields and let only the key vary — otherwise the partitions come back one after another rather than in order",
			ErrDeclaration, where, index.Name, len(index.Fields))
	}
	for i := range index.Fields {
		// Compared with sameTerm rather than ==: Term.Value is any, and an
		// index field declared "any" lets a constant here be a slice or a map,
		// which == panics on rather than compares.
		if !sameTerm(operation.From.Terms[i], operation.To.Terms[i]) {
			return fmt.Errorf("%w: %s, so a scan on %q must fix %q rather than range over it",
				ErrDeclaration, where, index.Name, index.Fields[i].Path)
		}
	}
	return nil
}

// totalsAcross refuses a rollup read of a partitioned collection that would
// have to add up groups it has not finished finding.
//
// A rollup row is a total, and a group's total is the sum of what each
// partition holds for it. Adding them up while walking is fine when the read
// names one group, because there is one number to arrive at. Over a range it
// is not: the read would have to hold every group in the range until the last
// partition had been walked, which is memory nobody declared, bounded by the
// data rather than by the operation.
func totalsAcross(collection *Collection, rollup *Rollup, operation *Operation) error {
	if collection.spec.Partition == nil {
		return nil
	}

	where := fmt.Sprintf("%q is divided %s", collection.spec.Name, describePartition(collection.spec.Partition))

	if operation.From == nil || operation.To == nil ||
		len(operation.From.Terms) != len(rollup.Group) || len(operation.To.Terms) != len(rollup.Group) {
		return fmt.Errorf("%w: %s, so reading %q must name one group — every partition holds part of each total, and finding them all over a range is work nobody declared",
			ErrDeclaration, where, rollup.Name)
	}
	for i := range rollup.Group {
		// Same reason as scanAcross: Term.Value is any and a group field
		// declared "any" can carry a slice or a map as a constant, which ==
		// panics on.
		if !sameTerm(operation.From.Terms[i], operation.To.Terms[i]) {
			return fmt.Errorf("%w: %s, so reading %q must fix %q rather than range over it",
				ErrDeclaration, where, rollup.Name, rollup.Group[i].Path)
		}
	}
	return nil
}
