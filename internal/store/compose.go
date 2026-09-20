package store

import (
	"errors"
	"fmt"
)

// A composed operation is a batch whose steps name operations rather than
// collections.
//
// It exists to answer one objection without answering it with a query
// language: "you are missing a scan-with-total, when do I get one?" — never;
// you compose it, out of two declarations that already exist, in one more
// declaration, and nobody writes an expression. The page and its true total
// come back from one call, which means one transaction and therefore ONE
// state: today they are two calls, the write lock spans one of them, and a
// write landing in between is how a user is shown "51 results" over a page
// of 50 that just lost one.
//
// What it is not sold as is faster. Task 0069 measured the two places a saving
// could come from: fsync, which a batch already saves and composing saves
// nothing further of, and round trips, which on loopback came to 0.099-0.131
// ms — the same size as the difference between two runs of the same
// measurement. Nobody has measured it across a real network. Until somebody
// has, the honest words are atomic, composable, and fewer round trips, which
// is a count read off a declaration and not a duration.
//
// The whole of what makes it safe is that a step runs EXACTLY ONCE, and that
// the number of rows it may hand back is readable from the declarations rather
// than from the data. Five conditions hold that up, all of them refusals made
// when the operation is declared, none of them checks made while it runs:
//
//	N1  the reference pins a version, and that version already exists
//	N2  the callee is itself a declaration this store validated when it was
//	    made, so its own ceiling is already true of it — see the note on depth
//	    in ceiling below
//	N3  the callee's ceiling is readable from its declaration at all
//	N4  a term that names another step may only name one whose ceiling is 1
//	N5  a batch with a composed step declares a limit, and the ceilings of its
//	    steps add up to no more than that limit
//
// N5 is what makes the whole thing readable by a person. It is not that the
// ceiling can be worked out; it is that it does not have to be. By induction
// over depth: a flat operation's ceiling is the limit written in it, and a
// composed one's is the sum of its steps' ceilings, which N5 holds at or below
// the limit written in IT. So the ceiling of a composed operation, at any
// depth, is the number written in that operation — not a product, not a sum
// the reader has to work out, and not a number that depends on opening the
// files it refers to. Measured, not argued: see compose_test.go's
// TestTheCeilingOfAComposedOperationAtAnyDepthIsTheLimitItDeclares.

// costs is one declaration's walk over the reference graph: what it has
// already worked out, and what it is in the middle of working out.
//
// known is memoisation, and it is what makes the walk cost one visit per edge
// rather than one per path — a diamond (two steps calling two operations that
// both call a third) is the ordinary shape, not a pathological one.
//
// visiting is not a cycle detector standing in for the DAG argument above; a
// cycle cannot be declared, so this can only fire on a store whose operation
// records have been damaged or hand-edited underneath us. It is here so that
// such a store is refused rather than walked until the stack runs out, which
// is the difference between an error and a process that dies.
type costs struct {
	known    map[string]int
	visiting map[string]bool
}

func newCosts() *costs {
	return &costs{known: map[string]int{}, visiting: map[string]bool{}}
}

// ceiling is the most rows an operation may return, read from its declaration
// and from the declarations it names — never from the data.
//
// The per-action numbers, and why each one is what it is:
//
//   - get, insert, put, update, delete touch exactly one document, so one.
//     A write hands back no rows at all, and counting it as one rather than
//     zero is deliberate: it keeps the sum an upper bound on the number of
//     STEPS as well as on the number of rows, so the limit a composed
//     operation declares also bounds how many times it touches the database.
//   - scan and totals return their declared limit, which both are already
//     required to declare.
//   - count hands back a number rather than rows. Its ceiling is one, but only
//     once it declares a limit: a count without one is the single action in
//     this store that can be declared without declaring what it costs, and
//     how far it walks is then decided by the data. As a step it is refused
//     outright, for a second reason as well — see validateComposedStep.
//   - batch is the sum over its steps. A step that touches a collection is
//     one; a step that calls an operation is that operation's ceiling.
//
// Depth is unbounded on purpose. N5 is checked when each operation is
// declared, so every operation this walk reaches has ALREADY had the sum of
// its own steps held at or below its own limit — the induction is not
// re-derived here, it is carried by the declarations themselves. A maximum
// depth would buy nothing that is not already bought, and a constant nobody
// can justify is worse than no constant.
func (s *Store) ceiling(operation Operation, within *costs) (int, error) {
	switch operation.Action {
	case ActionGet, ActionInsert, ActionPut, ActionUpdate, ActionDelete:
		return 1, nil

	case ActionScan, ActionTotals:
		if operation.Limit <= 0 {
			return 0, fmt.Errorf("%w: %q is a %s with no declared limit, so how many rows it may return is not written down",
				ErrDeclaration, operation.Name, operation.Action)
		}
		return operation.Limit, nil

	case ActionCount:
		if operation.Limit <= 0 {
			return 0, fmt.Errorf("%w: %q is a count with no declared limit, so how far it walks is decided by the data rather than by the declaration",
				ErrDeclaration, operation.Name)
		}
		return 1, nil

	case ActionBatch:
		at := fmt.Sprintf("%s@%d", operation.Name, operation.Version)
		if total, done := within.known[at]; done {
			return total, nil
		}
		if within.visiting[at] {
			return 0, fmt.Errorf("%w: %q refers to itself, which a pinned version cannot be declared to do — this store's operation records disagree with the rule that wrote them",
				ErrDamaged, at)
		}
		within.visiting[at] = true
		defer delete(within.visiting, at)

		total := 0
		for i := range operation.Steps {
			step := &operation.Steps[i]
			if step.Operation == "" {
				total++
				continue
			}
			callee, found, err := s.Operation(step.Operation, step.Version)
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, fmt.Errorf("%w: %q version %d", ErrNoOperation, step.Operation, step.Version)
			}
			reach, err := s.ceiling(callee, within)
			if err != nil {
				return 0, err
			}
			total += reach
		}
		if operation.Version > 0 {
			within.known[at] = total
		}
		return total, nil
	}

	return 0, fmt.Errorf("%w: %q is not something an operation can do", ErrDeclaration, operation.Action)
}

// keyTypeOf is the declared type of the key an operation leaves in Result.Key,
// or the empty string when it leaves none.
//
// This is not the same question as "what does it read". A get reads one
// document and sets no key; a scan reads many and sets no key; the four writes
// set the key they wrote. A batch sets whatever its last step set, because
// that is what runSteps leaves behind, and saying anything else here would be
// this package describing a mechanism it does not have.
func (s *Store) keyTypeOf(operation Operation) (string, error) {
	switch operation.Action {
	case ActionInsert, ActionPut, ActionUpdate, ActionDelete:
		collection, err := s.Collection(operation.Collection)
		if err != nil {
			return "", err
		}
		return collection.spec.Key.Type, nil

	case ActionBatch:
		if len(operation.Steps) == 0 {
			return "", nil
		}
		last := operation.Steps[len(operation.Steps)-1]
		if last.Operation == "" {
			collection, err := s.Collection(last.Collection)
			if err != nil {
				return "", err
			}
			return collection.spec.Key.Type, nil
		}
		callee, found, err := s.Operation(last.Operation, last.Version)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("%w: %q version %d", ErrNoOperation, last.Operation, last.Version)
		}
		return s.keyTypeOf(callee)
	}

	return "", nil
}

// validateComposedStep checks a step that calls an operation, and says what it
// offers the steps after it.
//
// Everything here is a refusal made when the operation is declared. Nothing
// below is re-checked while it runs, and that is the point: a cost that is
// only discovered at call time is not a declared cost.
func (s *Store) validateComposedStep(step *Step, where string, parent *Operation,
	within *costs, check func(Term, string, string) error) (stepOffer, error) {

	// One step, one job. A step that wrote both shapes would leave the reader
	// — and this function — guessing which one it meant, and the guess would
	// be made once here and differently somewhere else later.
	switch {
	case step.Action != "":
		return stepOffer{}, fmt.Errorf("%w: %s calls %q and also does %q — a step either touches a collection or calls an operation",
			ErrDeclaration, where, step.Operation, step.Action)
	case step.Collection != "":
		return stepOffer{}, fmt.Errorf("%w: %s calls %q and also names collection %q — a step either touches a collection or calls an operation",
			ErrDeclaration, where, step.Operation, step.Collection)
	case step.Key != nil, len(step.Document) > 0, len(step.Set) > 0, step.Exists != nil, len(step.Require) > 0:
		return stepOffer{}, fmt.Errorf("%w: %s calls %q, so it carries its arguments in \"with\" and nothing else — a key, a document, a set, an exists or a require belongs to a step that touches a collection",
			ErrDeclaration, where, step.Operation)
	}

	// N1. Version zero means "the latest" everywhere else in this package, so
	// it is spelled out rather than defaulted: a reference that followed the
	// latest would make the limit this operation declares stop being true the
	// moment somebody redeclared the callee, and nobody would be told.
	if step.Version <= 0 {
		return stepOffer{}, fmt.Errorf("%w: %s calls %q without pinning a version — a reference to whichever version is newest is a cost that changes when a different declaration changes",
			ErrDeclaration, where, step.Operation)
	}
	callee, found, err := s.Operation(step.Operation, step.Version)
	if err != nil && !errors.Is(err, ErrNoOperation) {
		return stepOffer{}, err
	}
	if !found {
		return stepOffer{}, fmt.Errorf("%w: %s calls %q version %d, which is not declared",
			ErrDeclaration, where, step.Operation, step.Version)
	}

	// A count is refused for two independent reasons, and either alone would
	// be enough. Its answer is a number, and a composed result has exactly one
	// Count field for the rows it returns — so the number would be computed,
	// paid for, and then dropped on the floor with nothing saying so. And a
	// count is the one action that can be declared without declaring its cost.
	// A totals reads the same kind of answer into a row, where it survives.
	if callee.Action == ActionCount {
		return stepOffer{}, fmt.Errorf("%w: %s calls %q, which is a count — a count hands back a number and a composed answer has nowhere to put it, so it would be paid for and dropped; read a totals instead",
			ErrDeclaration, where, step.Operation)
	}

	// N3. Refused here rather than at the first call that walks further than
	// anybody expected.
	reach, err := s.ceiling(callee, within)
	if err != nil {
		return stepOffer{}, fmt.Errorf("%s calls %q: %w", where, step.Operation, err)
	}

	// Scopes, fail-closed and at declare time. allowed() checks the operation
	// a caller NAMED, and a caller names only the outermost one — so without
	// this, composing would be a way to run an operation whose scope you do
	// not hold by naming one that calls it. The rule is the union: whatever
	// the callee asks for, the caller of this operation must present too.
	held := map[string]bool{}
	for _, scope := range parent.Scopes {
		held[scope] = true
	}
	for _, scope := range callee.Scopes {
		if !held[scope] {
			return stepOffer{}, fmt.Errorf("%w: %s calls %q, which needs scope %q, and %q does not ask for it — a composed operation asks for every scope the operations it calls ask for",
				ErrDeclaration, where, step.Operation, scope, parent.Name)
		}
	}

	// The arguments. The callee's own declaration says what they are called
	// and what type each one is, so the terms are checked against that — the
	// same check an operation's own terms get, which is what keeps a composed
	// call from being a second, looser way to pass a value in.
	declared := map[string]Parameter{}
	for _, parameter := range callee.Input {
		declared[parameter.Name] = parameter
	}
	for name := range step.With {
		if _, known := declared[name]; !known {
			return stepOffer{}, fmt.Errorf("%w: %s passes %q to %q, which does not take it",
				ErrDeclaration, where, name, step.Operation)
		}
	}
	for _, parameter := range callee.Input {
		term, passed := step.With[parameter.Name]
		if !passed {
			if parameter.Required {
				return stepOffer{}, fmt.Errorf("%w: %s calls %q, which needs %q, and nothing is passed for it",
					ErrDeclaration, where, step.Operation, parameter.Name)
			}
			continue
		}
		if err := check(term, parameter.Type, fmt.Sprintf("%s passing %q to %q", where, parameter.Name, step.Operation)); err != nil {
			return stepOffer{}, err
		}
	}

	keyType, err := s.keyTypeOf(callee)
	if err != nil {
		return stepOffer{}, err
	}
	return stepOffer{keyType: keyType, ceiling: reach}, nil
}
