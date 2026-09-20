package store

import (
	"fmt"
	"sort"
)

// Envelope is what a caller needs to read before running an operation it did
// not write, without running it first — SAPE-8's "cost envelope".
//
// It is derived from the declaration, never written by hand alongside it, so
// it cannot disagree with the operation it describes: everything here is
// read off the stored Operation (and, for a batch, the operations it names),
// the same declaration validateOperation already held to N1-N5 when it was
// declared.
//
// Four facts, plus one that needed no new plumbing:
//
//   - Collections is every collection this operation, or anything it calls,
//     touches — the set a flat operation's own Collection field already
//     names on its own, and a composed one's steps and their callees name
//     between them.
//
//   - Indexes is every index (or ClusteredIndex, for a scan or count that
//     walks primary-key order) any of that touches.
//
//   - Limit is the cost a caller of this exact operation should weigh: for
//     a get, a write, a scan, a totals, or a batch, that is compose.go's
//     ceiling() — the most rows this operation may hand back — reached from
//     outside this package for the first time. It is deliberately not read
//     off Operation.Limit directly: that field is zero, and thanks to its
//     `omitempty` tag absent from the wire, for every action whose ceiling
//     is not a number it writes down itself (a get, a write, or a plain
//     batch of them, whose ceiling is one document per action rather than a
//     number written anywhere), so a reader going straight to the JSON has
//     no way to tell "unbounded" from "the number is one (or the step
//     count) and nobody had to write it" — the ambiguity SAPE-8 asks to have
//     closed. Limit here is never that ambiguous: for those actions it is
//     always the resolved ceiling, so "absent" cannot mean anything because
//     Limit is never absent.
//
//     A count is the one action where Limit is deliberately NOT ceiling():
//     ceiling() reports 1 for a count, because that is what a count
//     contributes to an ENCLOSING batch's declared sum (N5) — a single
//     answer, not a row. A count can never actually be composed that way
//     (validateComposedStep refuses one as a callee outright), so that "1"
//     never describes a real run; the real cost of running a count by
//     itself is how far it walks, which is Operation.Limit, required
//     positive by validateOperation for exactly this reason. Reusing
//     ceiling() here would print "1" for a count that may examine a
//     million documents, which is the misleading number, not the honest
//     one — so Limit is Operation.Limit for a count and ceiling() for
//     everything else.
//
//   - Projection / WholeDocument together say which fields escape. Exactly
//     one of two shapes: WholeDocument true (a flat get or scan with no
//     declared projection, or a batch step that reads a collection directly
//     — a step has no projection field of its own; runStep's own project
//     call always passes it nil, so a get step in a batch always hands back
//     the whole document regardless of what the enclosing operation
//     declares), or Projection holding the narrower field list a read
//     declared. Neither is set for an action that returns no document
//     fields at all: a count (a number, not rows), a write (no rows), or a
//     totals (see below).
//
// Scopes rides along for free: validateComposedStep already requires a
// composed operation's own Scopes to be the union of whatever anything it
// calls asks for — a caller of a composed operation names only the
// outermost one, so the callee's own scope would otherwise go unchecked.
// That means Operation.Scopes already reads correctly with no walk at all;
// it is copied here only so the envelope is the one place all five facts are
// read from, not four from here and one from the declaration itself.
//
// A totals is worth naming on its own rather than folding into the other
// four: it hands back a synthetic {count, group} row built from the
// rollup's own already-declared group fields (public through the
// collection's Spec already), never the underlying document, and it
// ignores any Projection written on it — validateOperation only checks that
// a declared projection's paths are non-empty, for every action, whether or
// not that action ever reads one. So a totals sets neither Projection nor
// WholeDocument: nothing beyond the rollup's own declared shape escapes,
// and that shape is not this ticket's "escaping fields" in the first place
// since it was never document fields to begin with.
//
// If two paths through a composed operation disagree about whether the
// whole document escapes — one step projects, another does not — the
// answer is the safe one: WholeDocument true. A caller deciding "may I run
// this" cares about the worst a run can do, not an average of its steps,
// and a partial field list next to a WholeDocument flag would read as a
// promise this envelope does not make.
type Envelope struct {
	Operation string `json:"operation"`
	Version   int    `json:"version"`

	// Collections is never nil, even when empty, for the same reason
	// Catalogue.Collections is not: a caller reaching straight for .map()
	// should not have to special-case the one operation that touches
	// nothing yet.
	Collections []string `json:"collections"`
	Indexes     []string `json:"indexes"`

	Limit int `json:"limit"`

	Projection    []string `json:"projection,omitempty"`
	WholeDocument bool     `json:"wholeDocument"`

	Scopes []string `json:"scopes,omitempty"`
}

// Envelope reads the cost envelope of one declared operation, by name and
// version (0 for the newest), without running it.
func (s *Store) Envelope(name string, version int) (Envelope, error) {
	operation, found, err := s.Operation(name, version)
	if err != nil {
		return Envelope{}, err
	}
	if !found {
		return Envelope{}, fmt.Errorf("%w: %q version %d", ErrNoOperation, name, version)
	}
	return s.envelopeOf(operation)
}

// envelopeOf builds the envelope of an operation this store already knows —
// either the one Envelope looked up, or one WhatIsHere is about to list.
func (s *Store) envelopeOf(operation Operation) (Envelope, error) {
	// The same number N5 already checked at declare time, read from outside
	// this package for the first time. Reused rather than re-derived: a
	// second implementation of "how far does this reach" is a second place
	// for the two to quietly disagree. Except for a top-level count, whose
	// real cost ceiling() does not describe — see the Envelope doc above.
	limit := operation.Limit
	if operation.Action != ActionCount {
		var err error
		limit, err = s.ceiling(operation, newCosts())
		if err != nil {
			return Envelope{}, err
		}
	}

	envelope := Envelope{
		Operation: operation.Name,
		Version:   operation.Version,
		Limit:     limit,
		Scopes:    operation.Scopes,
	}

	collections := map[string]bool{}
	indexes := map[string]bool{}
	projection := map[string]bool{}
	visited := map[string]bool{}

	var walk func(op Operation) error
	walk = func(op Operation) error {
		// A pinned reference cannot cycle (Step.Operation's own doc: version
		// only ever goes up, so the reference graph is a DAG by
		// construction), so this is memoisation rather than a cycle guard —
		// the same shape as costs.visiting in ceiling, kept separate because
		// this walk answers a different question and does not need to sum
		// anything.
		if op.Version > 0 {
			at := fmt.Sprintf("%s@%d", op.Name, op.Version)
			if visited[at] {
				return nil
			}
			visited[at] = true
		}

		if op.Action == ActionBatch {
			for i := range op.Steps {
				step := &op.Steps[i]
				if step.Operation != "" {
					callee, found, err := s.Operation(step.Operation, step.Version)
					if err != nil {
						return err
					}
					if !found {
						return fmt.Errorf("%w: %q version %d", ErrNoOperation, step.Operation, step.Version)
					}
					if err := walk(callee); err != nil {
						return err
					}
					continue
				}

				// A step that touches a collection directly: always the
				// whole document for a get (runStep's project(document,
				// nil)), and touched but nothing escaping for a write.
				collections[step.Collection] = true
				if step.Action == ActionGet {
					envelope.WholeDocument = true
				}
			}
			return nil
		}

		if op.Collection != "" {
			collections[op.Collection] = true
		}

		switch op.Action {
		case ActionScan, ActionGet:
			if len(op.Projection) == 0 {
				envelope.WholeDocument = true
			} else {
				for _, path := range op.Projection {
					projection[path] = true
				}
			}
			if op.Action == ActionScan {
				indexes[indexName(op.Index)] = true
			}

		case ActionCount:
			indexes[indexName(op.Index)] = true
			// Hands back a number, not a document: nothing escapes.

		case ActionTotals:
			// A synthetic {count, group} row, not the document — see the
			// Envelope doc above.

		case ActionInsert, ActionPut, ActionUpdate, ActionDelete:
			// No rows returned.
		}
		return nil
	}

	if err := walk(operation); err != nil {
		return Envelope{}, err
	}

	envelope.Collections = sortedSet(collections)
	envelope.Indexes = sortedSet(indexes)
	// Only reported when nothing already said "the whole document escapes" —
	// see the Envelope doc's note on disagreeing paths.
	if !envelope.WholeDocument && len(projection) > 0 {
		envelope.Projection = sortedSet(projection)
	}

	return envelope, nil
}

// indexName is the index an access reads, spelled out: an empty Index means
// the clustered one everywhere else in this package, and leaving it blank
// here would make a set of index names one entry that is not a name.
func indexName(index string) string {
	if index == "" {
		return ClusteredIndex
	}
	return index
}

// sortedSet turns a membership set into the sorted, never-nil slice the wire
// carries — sorted so two reads of the same declaration produce the same
// bytes, and never nil for the reason Catalogue.Collections is not.
func sortedSet(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
