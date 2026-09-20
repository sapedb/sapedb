package store

import (
	"errors"
	"strings"
	"testing"
)

// The type of a key taken from an earlier step, which until now was the one
// value in a declaration nobody compared with the place it was used.
//
// validateOperation's check() has always compared an {"arg": ...} term's
// declared type against what the surrounding position wants, and its own
// comment says why: "an argument whose type does not fit where it is used
// would sort somewhere else in the index, and the caller would get an empty
// answer with no error." The {"step": ..., "field": "key"} branch returned
// before reaching that line, so the identical mistake made through the
// identical mechanism was accepted at declare time and produced exactly the
// outcome that comment describes at call time.
//
// Three tests, in the order that makes the third one legible:
//
//  1. the door that was already shut (an argument of the wrong type), so a
//     failure below is about the step door and not about check() as a whole;
//  2. the positive control on the step door — a key of the right type flows
//     from one step to the next, is accepted, AND runs, so "refused" below
//     cannot be a declaration that was never going to be accepted anyway;
//  3. the door this closes.
//
// keyedTwoWays declares two collections whose primary keys are declared
// different types, which is the whole fixture: a key produced by a step on one
// is not a key on the other.
func keyedTwoWays(t *testing.T) *Store {
	t.Helper()
	_, s := fresh(t, 11)

	if _, err := s.Declare(Spec{
		Name: "orders", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
	}); err != nil {
		t.Fatalf("declare orders: %v", err)
	}
	if _, err := s.Declare(Spec{
		Name: "receipts", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"},
	}); err != nil {
		t.Fatalf("declare receipts: %v", err)
	}
	if _, err := s.Declare(Spec{
		Name: "lines", Key: Key{Path: "id", Type: TypeNumber},
	}); err != nil {
		t.Fatalf("declare lines: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("commit declarations: %v", err)
	}
	return s
}

func TestAnArgumentOfTheWrongTypeForAKeyIsRefusedAtDeclareTime(t *testing.T) {
	s := keyedTwoWays(t)

	_, err := s.DeclareOperation(Caller{}, Operation{
		Name: "lines.get", Collection: "lines", Action: ActionGet,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a string argument used as a number key was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "is a number") || !strings.Contains(err.Error(), "declared string") {
		t.Fatalf("the refusal does not name both types: %v", err)
	}
}

func TestAKeyOfTheRightTypeFlowsFromOneStepToTheNextAndRuns(t *testing.T) {
	s := keyedTwoWays(t)

	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "orders.place", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "who", Type: TypeString, Required: true}},
		Steps: []Step{
			{
				Name: "order", Action: ActionInsert, Collection: "orders",
				Document: map[string]Term{"who": {Arg: "who"}},
			},
			{
				Name: "receipt", Action: ActionInsert, Collection: "receipts",
				Document: map[string]Term{"id": {Step: "order", Field: "key"}},
			},
		},
	}); err != nil {
		t.Fatalf("a string key handed to a string-keyed collection was refused: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	result, err := s.Invoke(Caller{}, "orders.place", 0, map[string]any{"who": "ann"})
	if err != nil {
		t.Fatalf("running it: %v", err)
	}
	if result.Changed != 2 {
		t.Fatalf("the batch changed %d documents, want 2", result.Changed)
	}

	// And the second document really is keyed by the first one's key — which
	// is what makes this a control for the refusal below rather than a
	// declaration that merely parsed.
	receipts, err := s.Collection("receipts")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := receipts.Get(result.Key); err != nil || !found {
		t.Fatalf("the receipt is not stored under the order's key: found=%v err=%v", found, err)
	}
}

func TestAKeyOfTheWrongTypeTakenFromAnEarlierStepIsRefusedAtDeclareTime(t *testing.T) {
	s := keyedTwoWays(t)

	// "lines" is keyed by number; "order" produces a string. Before this was
	// closed, this declaration was accepted and every call to it wrote a line
	// under a key that sorts nowhere near where a number would.
	_, err := s.DeclareOperation(Caller{}, Operation{
		Name: "orders.place_line", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "who", Type: TypeString, Required: true}},
		Steps: []Step{
			{
				Name: "order", Action: ActionInsert, Collection: "orders",
				Document: map[string]Term{"who": {Arg: "who"}},
			},
			{
				Name: "line", Action: ActionInsert, Collection: "lines",
				Document: map[string]Term{"id": {Step: "order", Field: "key"}},
			},
		},
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a string key handed to a number-keyed collection was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "is a number") {
		t.Fatalf("the refusal does not say what the position wanted: %v", err)
	}
	if !strings.Contains(err.Error(), `step "order"`) || !strings.Contains(err.Error(), "gives a string") {
		t.Fatalf("the refusal does not name the step or what it gives: %v", err)
	}
}

// And the same door on the other side: a step's key used as a scan bound of
// the wrong type. Same branch of check(), reached from a different caller, so
// that the fix is a property of the branch rather than of one call site.
func TestAStepsKeyUsedAsAConditionValueIsStillAcceptedWhateverItsType(t *testing.T) {
	s := keyedTwoWays(t)

	// A Require condition wants TypeAny, so any key fits it — this is the
	// case the new check must NOT refuse, and it is here so that the rule is
	// "compare the types" rather than "refuse step keys".
	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "orders.place_checked", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "who", Type: TypeString, Required: true}},
		Steps: []Step{
			{
				Name: "order", Action: ActionInsert, Collection: "orders",
				Document: map[string]Term{"who": {Arg: "who"}},
			},
			{
				Name: "receipt", Action: ActionInsert, Collection: "receipts",
				Document: map[string]Term{"of": {Step: "order", Field: "key"}},
			},
		},
	}); err != nil {
		t.Fatalf("a step key in an any-typed position was refused: %v", err)
	}
}
