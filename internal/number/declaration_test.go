package number

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// declaring builds the declaration ISS-35's second half was measured against:
// one insert whose document carries a constant, which is the shape a schema
// file writes as `"n": {"value": 9007199254740993}`.
func declaring(constant json.Number) *store.Operation {
	return &store.Operation{
		Name: "ids.fixed", Collection: "ids", Action: store.ActionInsert,
		Document: map[string]store.Term{"n": {Value: constant, Constant: true}},
	}
}

// TestTheBoundaryOnADeclarationIsRepresentabilityNotMagnitude is the shape
// TestTheNumberBoundaryIsRepresentabilityNotMagnitude pinned for the invoke
// door, pinned again for a declaration — and pinned through ExactIn rather
// than through Carries, so that what it measures is the path and not the
// predicate a second time.
//
// Both sides, for the reason the first half gives: a fix that refused every
// integer above 2^53 would pass a table holding only the refusals, and it
// would be wrong. 9007199254740994 and 9223372036854775808 are both larger
// than the refused 9007199254740993 and both come back exactly as written, so
// refusing them would break a working author to fix a bug they never had.
//
// The two decimals are the other half of the same claim. 1.5e300 is
// integer-valued and 0.1 is not a float64 exactly, and both have always gone
// in and come back as written; a rule that caught either would be a rule about
// values rather than about literals, and would refuse most of the numbers this
// database has ever been sent.
func TestTheBoundaryOnADeclarationIsRepresentabilityNotMagnitude(t *testing.T) {
	for _, one := range []struct {
		sent    string
		refused bool
		why     string
	}{
		{"9007199254740992", false, "2^53, exactly a float64"},
		{"9007199254740993", true, "2^53+1, the nearest float64 is 2^53"},
		{"9007199254740994", false, "2^53+2 — larger than the refused one, and exact"},
		{"18014398509481984", false, "2^54, exact"},
		{"9223372036854775807", true, "2^63-1, not a float64"},
		{"9223372036854775808", false, "2^63 — vastly larger than the refused one, and exact"},
		{"-9007199254740993", true, "the same hole below zero"},
		{"-9007199254740992", false, "-2^53, exact"},
		{"1234567890123456789", true, "the Snowflake-shaped id ISS-35 names"},
		{"0", false, "zero"},
		{"42", false, "an ordinary integer"},
		{"1.5", false, "a fraction float64 holds exactly"},
		{"0.1", false, "a fraction it does not — and never has, for anybody"},
		{"1.5e300", false, "integer-valued, written as a float, and printed back as written"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			operation := declaring(json.Number(one.sent))
			err := ExactIn("operation \"ids.fixed\"", operation)

			if !one.refused {
				if err != nil {
					t.Fatalf("%s (%s) was refused: %v", one.sent, one.why, err)
				}
				// And it is a float64 afterwards, not a json.Number: see
				// TestASurvivingConstantIsHandedOnAsFloat64.
				if _, ok := operation.Document["n"].Value.(float64); !ok {
					t.Fatalf("%s survived as %T, want float64", one.sent, operation.Document["n"].Value)
				}
				return
			}

			if err == nil {
				t.Fatalf("%s (%s) was declared rather than refused, and would be stored as a different number", one.sent, one.why)
			}
			if !errors.Is(err, ErrPrecision) {
				t.Fatalf("the refusal is not ErrPrecision: %v", err)
			}
			// The value by name. "a number in your declaration" is not
			// something an author can act on when the file carries two
			// hundred of them.
			if !strings.Contains(err.Error(), one.sent) {
				t.Errorf("the refusal does not name the value sent: %v", err)
			}
			// And where it was: which constant of which declaration.
			if !strings.Contains(err.Error(), `operation "ids.fixed".document.n.value`) {
				t.Errorf("the refusal does not say which constant it was: %v", err)
			}
		})
	}
}

// TestARefusedDeclarationSaysWhatWouldHaveBeenStored is the half of the
// refusal that makes it readable. The author can see the neighbouring number
// they would otherwise have found in a dump, which is what tells them the
// difference between a typo and a limit of the type.
func TestARefusedDeclarationSaysWhatWouldHaveBeenStored(t *testing.T) {
	for sent, instead := range map[string]string{
		"9007199254740993":    "9007199254740992",
		"9223372036854775807": "9223372036854776000",
		"1234567890123456789": "1234567890123456800",
	} {
		err := ExactIn("operation \"ids.fixed\"", declaring(json.Number(sent)))
		if err == nil {
			t.Fatalf("%s was accepted", sent)
		}
		if !strings.Contains(err.Error(), instead) {
			t.Errorf("the refusal of %s does not say it would store %s: %v", sent, instead, err)
		}
	}
}

// TestEveryConstantADeclarationCanCarryIsWalked is the map of the hole.
//
// A constant is not only the document an insert writes. It is the key a `get`
// names, either end of a scan's range, what an omitted argument stands for,
// and a step of a batch — every one of them an `any` field, every one of them
// rounded at the decode, and every one of them written into the stored
// declaration where it is used again by every call that runs it.
//
// The walk finds them because it is over the shape and not over a list; this
// test is that claim measured against the shapes that exist today, so that a
// field added tomorrow which this walk does NOT reach shows up as a red test
// here rather than as a rounded id in somebody's database.
func TestEveryConstantADeclarationCanCarryIsWalked(t *testing.T) {
	bad := json.Number("9007199254740993")

	for _, one := range []struct {
		what  string
		where string
		build func() any
	}{
		{
			what: "the document an insert writes", where: `.document.n.value`,
			build: func() any { return declaring(bad) },
		},
		{
			what: "the key a get names", where: `.key.value`,
			build: func() any {
				return &store.Operation{Name: "ids.one", Collection: "ids", Action: store.ActionGet,
					Key: &store.Term{Value: bad, Constant: true}}
			},
		},
		{
			what: "the low end of a scan", where: `.from.terms[0].value`,
			build: func() any {
				return &store.Operation{Name: "ids.range", Collection: "ids", Action: store.ActionScan,
					From: &store.Endpoint{Terms: []store.Term{{Value: bad, Constant: true}}}}
			},
		},
		{
			what: "the high end of a scan", where: `.to.terms[0].value`,
			build: func() any {
				return &store.Operation{Name: "ids.range", Collection: "ids", Action: store.ActionScan,
					To: &store.Endpoint{Terms: []store.Term{{Value: bad, Constant: true}}}}
			},
		},
		{
			what: "what an omitted argument stands for", where: `.input[0].default`,
			build: func() any {
				return &store.Operation{Name: "ids.put", Collection: "ids", Action: store.ActionInsert,
					Input: []store.Parameter{{Name: "n", Type: store.TypeNumber, Default: bad}}}
			},
		},
		{
			what: "a step of a batch", where: `.steps[0].document.n.value`,
			build: func() any {
				return &store.Operation{Name: "ids.both", Collection: "ids", Action: store.ActionBatch,
					Steps: []store.Step{{Collection: "ids", Action: store.ActionInsert,
						Document: map[string]store.Term{"n": {Value: bad, Constant: true}}}}}
			},
		},
		{
			what: "the key a typed get names at the shell", where: `.key`,
			build: func() any { return &store.Access{Kind: "get", Collection: "ids", Key: bad} },
		},
		{
			what: "a value in a typed scan's bound", where: `.from.values[0]`,
			build: func() any {
				return &store.Access{Kind: "scan", Collection: "ids",
					From: &store.Bound{Values: []any{bad}}}
			},
		},
		{
			// The one that is easiest to forget and hardest to see: a
			// constant that is a whole document, with the number three levels
			// down inside it.
			what: "a number buried inside a constant document", where: `.document.body.value.order.lines[1].id`,
			build: func() any {
				return &store.Operation{Name: "ids.body", Collection: "ids", Action: store.ActionInsert,
					Document: map[string]store.Term{"body": {Constant: true, Value: map[string]any{
						"order": map[string]any{"lines": []any{
							map[string]any{"sku": "a", "qty": json.Number("2")},
							map[string]any{"sku": "b", "id": bad},
						}},
					}}}}
			},
		},
	} {
		t.Run(one.what, func(t *testing.T) {
			err := ExactIn("declaration", one.build())
			if err == nil {
				t.Fatalf("%s was not walked: 9007199254740993 would be stored as 9007199254740992", one.what)
			}
			if !errors.Is(err, ErrPrecision) {
				t.Fatalf("the refusal is not ErrPrecision: %v", err)
			}
			if !strings.Contains(err.Error(), "declaration"+one.where) {
				t.Errorf("the refusal points at %q, and should point at %q", err, "declaration"+one.where)
			}
		})
	}
}

// TestASurvivingConstantIsHandedOnAsFloat64 is the quiet half of the fix, and
// the one a green test suite would otherwise hide.
//
// Reading a declaration with UseNumber means every constant in it arrives as a
// json.Number. Everything downstream — store's `matches`, internal/keys, the
// change log, and the marshal that writes the declaration into the catalogue —
// expects float64 and says so. A json.Number left in place would be a value of
// a type none of them know: a second bug wearing this fix as a disguise, and
// one that only shows up on the values that were fine all along.
func TestASurvivingConstantIsHandedOnAsFloat64(t *testing.T) {
	operation := &store.Operation{
		Name: "ids.put", Collection: "ids", Action: store.ActionInsert,
		Input: []store.Parameter{{Name: "n", Type: store.TypeNumber, Default: json.Number("7")}},
		Key:   &store.Term{Value: json.Number("42"), Constant: true},
		Document: map[string]store.Term{
			"n":    {Value: json.Number("1.5"), Constant: true},
			"body": {Constant: true, Value: map[string]any{"deep": []any{json.Number("2"), "text", nil, true}}},
		},
	}
	if err := ExactIn("operation \"ids.put\"", operation); err != nil {
		t.Fatalf("an ordinary declaration was refused: %v", err)
	}

	if _, ok := operation.Key.Value.(float64); !ok {
		t.Errorf("the key constant is %T, want float64", operation.Key.Value)
	}
	if _, ok := operation.Input[0].Default.(float64); !ok {
		t.Errorf("the default is %T, want float64", operation.Input[0].Default)
	}
	if _, ok := operation.Document["n"].Value.(float64); !ok {
		t.Errorf("the document constant is %T, want float64", operation.Document["n"].Value)
	}
	deep := operation.Document["body"].Value.(map[string]any)["deep"].([]any)
	if _, ok := deep[0].(float64); !ok {
		t.Errorf("the nested number is %T, want float64", deep[0])
	}
	if deep[1] != "text" || deep[2] != nil || deep[3] != true {
		t.Errorf("the walk disturbed a non-number: %#v", deep)
	}
}

// TestTheWalkLeavesADeclarationThatCarriesNoConstantAlone is the control.
//
// A walk that rewrote something here would be a walk that could rewrite
// anything, and the two paths that call it — `establish` over the wire, and
// every collection in a schema file — hand it declarations with no `any` field
// in them at all. Measured by comparing the whole value before and after,
// rather than by the absence of an error.
func TestTheWalkLeavesADeclarationThatCarriesNoConstantAlone(t *testing.T) {
	spec := store.Spec{
		Name: "ids",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_n",
			Fields: []store.Field{{Path: "n", Type: store.TypeNumber, Missing: store.MissingSkip}},
		}},
		Rollups: []store.Rollup{{Name: "all", Count: true}},
	}
	before := spec

	if err := ExactIn("collection \"ids\"", &spec); err != nil {
		t.Fatalf("a collection with no constant in it was refused: %v", err)
	}
	if !reflect.DeepEqual(spec, before) {
		t.Errorf("the walk changed a declaration it had nothing to do to:\n  %+v\n  %+v", spec, before)
	}
}

// TestExactInRefusesToWalkSomethingItCannotWriteBack.
//
// ExactIn replaces every constant it accepts, so a value passed by copy would
// be checked and then thrown away — a check that reads as working and stores
// a json.Number anyway, which is a worse outcome than not checking at all.
// It refuses rather than silently doing half the job.
func TestExactInRefusesToWalkSomethingItCannotWriteBack(t *testing.T) {
	for _, one := range []struct {
		what  string
		given any
	}{
		{"a declaration by value", *declaring(json.Number("42"))},
		{"a nil pointer", (*store.Operation)(nil)},
		{"nothing at all", nil},
	} {
		t.Run(one.what, func(t *testing.T) {
			if err := ExactIn("declaration", one.given); err == nil {
				t.Fatal("it was walked, and anything it found could not have been written back")
			}
		})
	}
}
