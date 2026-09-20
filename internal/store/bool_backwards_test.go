package store

import (
	"errors"
	"testing"
)

// TestABackwardsBoolRangeIsNowRefused replaces TestABackwardsBoolRangeIsAccep
// tedRatherThanRefused, which pinned a real, measured gap in task 0045's
// promise found while building task 0054's case table (section 3.4 of the
// task doc): a bool field breaks the "lower == upper is left alone" rule
// completely rather than at one edge, because it has exactly two values —
// keys.Encode(false) and keys.Encode(true) are one step apart, so the ONE
// backwards pair a bool field can be written with (From: true, To: false,
// both inclusive) always lands on lower == upper, the same bytes an
// intentionally self-pinned range produces.
//
// This task closes that gap with backwardsBoolRange (scan.go), a value-level
// check — true and false are never ambiguous the way two floats one ULP
// apart are, so this can be judged before either side reaches keys.Encode,
// unlike the general lower == upper case stretch()'s doc explains is left
// alone on purpose. Both places task 0045 already made this refusal for
// every other field width now make it for bool too: refusedBackwardsRange
// (ops.go) at declare time, when both ends are constants, and stretch()
// (scan.go) at call time, when either end is an argument.
//
// Scoped narrow, matching the fix: a single-field bool index. A composite
// index with a bool field alongside others is a different, unmeasured shape
// this does not claim to cover — left as open debt in this task's report.
func TestABackwardsBoolRangeIsNowRefused(t *testing.T) {
	_, store := fresh(t, 7701)
	_, err := store.Declare(Spec{
		Name: "flags",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{Name: "by_flag", Fields: []Field{
			{Path: "b", Type: TypeBool, Missing: MissingFirst},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The constant-endpoint path: both ends are written into the declaration
	// itself, so refusedBackwardsRange (ops.go) must catch this at declare
	// time now.
	op := Operation{
		Name: "flags.backwards", Collection: "flags", Action: ActionScan,
		Index: "by_flag", Limit: 10,
		From: &Endpoint{Terms: []Term{{Value: true}}},
		To:   &Endpoint{Terms: []Term{{Value: false}}},
	}
	if _, err := store.DeclareOperation(Caller{}, op); !errors.Is(err, ErrDeclaration) {
		t.Fatalf("From=true To=false on a bool field: want ErrDeclaration at declare time, got %v", err)
	}

	// The argument path: only stretch() (scan.go) ever sees the values, at
	// call time, since neither end is a constant the declaration could have
	// judged in advance.
	argOp := Operation{
		Name: "flags.arg", Collection: "flags", Action: ActionScan,
		Index: "by_flag", Limit: 10,
		Input: []Parameter{{Name: "lo", Type: TypeBool, Required: true}, {Name: "hi", Type: TypeBool, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "lo"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "hi"}}},
	}
	if _, err := store.DeclareOperation(Caller{}, argOp); err != nil {
		t.Fatalf("the argument-shaped operation was refused at declare: %v", err)
	}
	_, err = store.Invoke(Caller{}, "flags.arg", 0, map[string]any{"lo": true, "hi": false})
	if !errors.Is(err, ErrArgument) {
		t.Fatalf("lo=true hi=false on a bool field: want ErrArgument at call time, got %v", err)
	}

	// The forward pair, and the pinned-empty pair, must still read as before:
	// this fix must not turn an honest empty stretch into a refusal.
	if _, err := store.Invoke(Caller{}, "flags.arg", 0, map[string]any{"lo": false, "hi": true}); err != nil {
		t.Fatalf("the forward pair lo=false hi=true was refused: %v", err)
	}
	result, err := store.Invoke(Caller{}, "flags.arg", 0, map[string]any{"lo": true, "hi": true})
	if err != nil {
		t.Fatalf("lo=true hi=true (pinned to a single point) was refused: %v", err)
	}
	_ = result
}
