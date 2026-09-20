package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// composed declares the world every test below works against: one collection,
// one index, one rollup, and the flat operations a composed one is built out
// of. Nothing here is composed; this is the vocabulary, not the measurement.
func composed(t *testing.T) *Store {
	t.Helper()
	_, s := fresh(t, 7)

	if _, err := s.Declare(Spec{
		Name: "items",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{
			Name:   "by_shelf",
			Fields: []Field{{Path: "shelf", Type: TypeString, Missing: MissingSkip}},
		}},
		Rollups: []Rollup{{
			Name:  "per_shelf",
			Group: []Field{{Path: "shelf", Type: TypeString, Missing: MissingSkip}},
			Count: true,
		}},
	}); err != nil {
		t.Fatalf("declare items: %v", err)
	}

	flat := []Operation{
		{
			Name: "items.add", Collection: "items", Action: ActionInsert,
			Input: []Parameter{
				{Name: "shelf", Type: TypeString, Required: true},
				{Name: "title", Type: TypeString, Required: true},
			},
			Document: map[string]Term{"shelf": {Arg: "shelf"}, "title": {Arg: "title"}},
		},
		{
			Name: "items.get", Collection: "items", Action: ActionGet,
			Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
			Key:   &Term{Arg: "id"},
		},
		{
			Name: "items.on_shelf", Collection: "items", Action: ActionScan,
			Index: "by_shelf",
			Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
			From:  &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			To:    &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			Limit: 50,
		},
		{
			Name: "items.total_on_shelf", Collection: "items", Action: ActionTotals,
			Rollup: "per_shelf",
			Input:  []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
			From:   &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			To:     &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			Limit:  1,
		},
		{
			Name: "items.count_on_shelf", Collection: "items", Action: ActionCount,
			Index: "by_shelf",
			Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
			From:  &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			To:    &Endpoint{Terms: []Term{{Arg: "shelf"}}},
			Limit: 1000,
		},
	}
	for _, operation := range flat {
		if _, err := s.DeclareOperation(Caller{}, operation); err != nil {
			t.Fatalf("declare %s: %v", operation.Name, err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	return s
}

// stock writes n items onto one shelf.
func stock(t *testing.T, s *Store, shelf string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Invoke(Caller{}, "items.add", 0, map[string]any{
			"shelf": shelf, "title": fmt.Sprintf("title %02d", i),
		}); err != nil {
			t.Fatalf("stocking %s: %v", shelf, err)
		}
	}
}

// page is the composed operation of task 0070 §3.2 — a page of a shelf and
// that shelf's true total, together.
func page() Operation {
	return Operation{
		Name: "catalog.page", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Limit: 51,
		Steps: []Step{
			{Name: "items", Operation: "items.on_shelf", Version: 1, With: map[string]Term{"shelf": {Arg: "shelf"}}},
			{Name: "total", Operation: "items.total_on_shelf", Version: 1, With: map[string]Term{"shelf": {Arg: "shelf"}}},
		},
	}
}

// ---------------------------------------------------------------------------
// Boundary case 1 of task 0070 §3: a composed operation that is worth having.
// It is accepted, and it runs, and what it hands back is right.
// ---------------------------------------------------------------------------

func TestAComposedOperationReturnsAPageAndItsTrueTotalFromOneCall(t *testing.T) {
	s := composed(t)
	stock(t, s, "a", 4)
	stock(t, s, "b", 2)

	if _, err := s.DeclareOperation(Caller{}, page()); err != nil {
		t.Fatalf("declaring catalog.page: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	result, err := s.Invoke(Caller{}, "catalog.page", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatalf("running catalog.page: %v", err)
	}

	// Four item rows, then one totals row, in step order.
	if len(result.Rows) != 5 {
		t.Fatalf("got %d rows, want 4 items and 1 total: %+v", len(result.Rows), result.Rows)
	}
	for i, row := range result.Rows[:4] {
		if row["shelf"] != "a" {
			t.Fatalf("row %d is not from shelf a: %+v", i, row)
		}
	}
	total := result.Rows[4]
	if got := total["count"]; got != float64(4) {
		t.Fatalf("the total says %v, want 4: %+v", got, total)
	}

	// Fewer round trips is a count, not a duration — and this is the count.
	// Everything above came back from ONE Invoke; the same answer built out of
	// the flat operations takes two, and nothing holds the database still
	// between them.
	items, err := s.Invoke(Caller{}, "items.on_shelf", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatal(err)
	}
	totals, err := s.Invoke(Caller{}, "items.total_on_shelf", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items.Rows)+len(totals.Rows) != len(result.Rows) {
		t.Fatalf("the composed answer (%d rows) is not what the two flat calls return (%d + %d)",
			len(result.Rows), len(items.Rows), len(totals.Rows))
	}
}

// ---------------------------------------------------------------------------
// Boundary case 2 of task 0070 §3: the shapes that would turn this into an
// expression language. Three of them, each refused by something different, so
// that no single rule going missing lets all three in.
// ---------------------------------------------------------------------------

// Iteration. This is §3.3's orders.underpaid, in the vocabulary above: a leg
// that may return many rows, and a second leg asked to take a value from it.
// That is "run this once for each row the other one returned" — a for loop
// with a JSON syntax — and it is refused at the second leg, not at the third
// one where the sum and the comparison would be.
func TestAStepThatTakesAValueFromAManyRowStepIsRefused(t *testing.T) {
	s := composed(t)

	_, err := s.DeclareOperation(Caller{}, Operation{
		Name: "catalog.walk", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Limit: 60,
		Steps: []Step{
			{Name: "found", Operation: "items.on_shelf", Version: 1, With: map[string]Term{"shelf": {Arg: "shelf"}}},
			{Name: "each", Operation: "items.get", Version: 1, With: map[string]Term{"id": {Step: "found", Field: "key"}}},
		},
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a step taking a value from a 50-row step was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), `step "found"`) {
		t.Fatalf("the refusal does not name the step at fault: %v", err)
	}
	if !strings.Contains(err.Error(), "50 rows") {
		t.Fatalf("the refusal does not say how many rows that step may return, which is the number the schema author needs: %v", err)
	}
}

// The same rule, on the one shape where nothing else would catch it — and the
// reason this test exists next to the one above rather than instead of it.
//
// A leg that may return many rows is, today, always a leg that hands back no
// key: a scan and a count and a totals all read without leaving one behind. So
// the test above is caught twice over, and it would stay red even if N4 were
// deleted — which makes it useless for saying whether N4 is still doing
// anything. A batch is the exception that separates the two: its ceiling is
// the sum over its steps, and it leaves behind the key its last step made. So
// this shape — a leg with a ceiling of two and a key to give — is refused by
// N4 and by nothing else, which is what makes it the measurement of N4 rather
// than of the rule next to it.
//
// This asymmetry is worth saying out loud rather than leaving in a test name:
// what stops fan-out today is mostly that Field is limited to "key" and that
// the many-row actions produce none. N4 is the rule that will still be
// standing on the day somebody widens Field, which is the obvious next
// request. It is written against the ceiling for exactly that reason.
func TestAStepThatTakesAKeyFromAManyRowBatchStepIsRefused(t *testing.T) {
	s := composed(t)

	// A batch of two gets: ceiling 2, and it leaves its last get's key behind.
	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "items.two_of", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "a", Type: TypeString, Required: true},
			{Name: "b", Type: TypeString, Required: true},
		},
		Steps: []Step{
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "a"}},
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "b"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	// The control, on the same path and before the refusal: a one-row leg with
	// a key IS allowed to hand it on. If this stops being true the refusal
	// below stops meaning anything.
	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "catalog.one_then_read", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "a", Type: TypeString, Required: true}},
		Limit: 10,
		Steps: []Step{
			{Name: "one", Action: ActionGet, Collection: "items", Key: &Term{Arg: "a"}},
			{Name: "read", Operation: "items.get", Version: 1,
				With: map[string]Term{"id": {Step: "one", Field: "key"}}},
		},
	}); err != nil {
		t.Fatalf("a one-row leg handing its key on was refused, so nothing below is a measurement of the ceiling rule: %v", err)
	}

	_, err := s.DeclareOperation(Caller{}, Operation{
		Name: "catalog.two_then_read", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "a", Type: TypeString, Required: true},
			{Name: "b", Type: TypeString, Required: true},
		},
		Limit: 10,
		Steps: []Step{
			{Name: "pair", Operation: "items.two_of", Version: 1,
				With: map[string]Term{"a": {Arg: "a"}, "b": {Arg: "b"}}},
			{Name: "read", Operation: "items.get", Version: 1,
				With: map[string]Term{"id": {Step: "pair", Field: "key"}}},
		},
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a step took a key from a two-row leg and the declaration was accepted — the ceiling rule is not doing anything: %v", err)
	}
	if !strings.Contains(err.Error(), `step "pair"`) || !strings.Contains(err.Error(), "2 rows") {
		t.Fatalf("the refusal must name the leg and how many rows it may return: %v", err)
	}
}

// Branching. "Skip this leg if the condition is false" has no spelling here,
// and this is the measurement of why: a condition that fails takes the whole
// transaction with it, including what an earlier step already wrote. It is an
// assertion, not an if.
func TestAFailedConditionTakesTheWholeTransactionRatherThanSkippingAStep(t *testing.T) {
	s := composed(t)
	stock(t, s, "a", 1)

	first, err := s.Invoke(Caller{}, "items.on_shelf", 0, map[string]any{"shelf": "a"})
	if err != nil || len(first.Rows) != 1 {
		t.Fatalf("seeding: %v %+v", err, first.Rows)
	}
	id := first.Rows[0]["id"]

	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "items.touch_then_check", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Steps: []Step{
			{Action: ActionUpdate, Collection: "items", Key: &Term{Arg: "id"},
				Set: map[string]Term{"touched": {Value: true}}},
			{Action: ActionUpdate, Collection: "items", Key: &Term{Arg: "id"},
				Require: []Condition{{Path: "shelf", Equals: &Term{Value: "z"}}},
				Set:     map[string]Term{"never": {Value: true}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Invoke(Caller{}, "items.touch_then_check", 0, map[string]any{"id": id}); !errors.Is(err, ErrCondition) {
		t.Fatalf("want ErrCondition, got %v", err)
	}

	items, err := s.Collection("items")
	if err != nil {
		t.Fatal(err)
	}
	document, found, err := items.Get(id)
	if err != nil || !found {
		t.Fatalf("the document went missing: %v", err)
	}
	if _, wrote := document["touched"]; wrote {
		t.Fatalf("the first step's write survived a later step's failed condition, so a failed condition is a branch after all: %+v", document)
	}
}

// Computation. There is nowhere to write sum, or <, or +. at() (collection.go)
// splits a path on dots and walks maps, and that is the whole of it — so a
// condition path that asks for arithmetic asks for a field nobody stored.
func TestAConditionPathIsAFieldNameAndNotACalculation(t *testing.T) {
	s := composed(t)
	stock(t, s, "a", 1)

	first, _ := s.Invoke(Caller{}, "items.on_shelf", 0, map[string]any{"shelf": "a"})
	id := first.Rows[0]["id"]

	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "items.arith", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "items", Key: &Term{Arg: "id"},
			Require: []Condition{{Path: "price + 1", Equals: &Term{Value: float64(2)}}},
			Set:     map[string]Term{"ok": {Value: true}},
		}},
	}); err != nil {
		t.Fatalf("a path is a string, so this declaration is accepted — the refusal is at call time: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	_, err := s.Invoke(Caller{}, "items.arith", 0, map[string]any{"id": id})
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("want the path to be read as a field name nobody wrote, got %v", err)
	}
	if !strings.Contains(err.Error(), "price + 1") {
		t.Fatalf("the refusal should name the path as written, so the author sees it was taken literally: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Boundary case 3 of task 0070 §3.4: the one the task itself says is hard —
// may a step whose ceiling is exactly one hand its key on to the next step?
//
// This implementation ALLOWS it, and the reasons are:
//
//  1. The arithmetic does not move. The leg still runs once; one times n is n;
//     the sum stays a sum and N5 still holds it at the declared limit.
//  2. It already exists. {"step": ..., "field": "key"} between two plain steps
//     of one batch is this exact thing, and it is the flow the comment on Term
//     names as the reason that field exists at all — an order giving its key
//     to its payment. Refusing it across a call boundary while allowing it
//     inside one is an asymmetry with nothing behind it but nerves.
//  3. The concrete objection §3.4 raised against it — that a key travelling
//     between steps was the one value in a declaration nobody type-checked —
//     was a real hole, and it is closed (steptype_test.go). The argument for
//     refusing lost its evidence.
//
// What is NOT allowed is the same move from a leg that may return more than
// one row, which is the test above. The line is the ceiling, not the call
// boundary.
// ---------------------------------------------------------------------------

func TestAKeyFromAOneRowStepMayBePassedToTheNextComposedStep(t *testing.T) {
	s := composed(t)

	// A batch of one write: ceiling 1, and it leaves a key behind.
	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "items.add_one", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "shelf", Type: TypeString, Required: true},
			{Name: "title", Type: TypeString, Required: true},
		},
		Steps: []Step{{
			Action: ActionInsert, Collection: "items",
			Document: map[string]Term{"shelf": {Arg: "shelf"}, "title": {Arg: "title"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "catalog.add_and_read", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "shelf", Type: TypeString, Required: true},
			{Name: "title", Type: TypeString, Required: true},
		},
		Limit: 10,
		Steps: []Step{
			{Name: "made", Operation: "items.add_one", Version: 1,
				With: map[string]Term{"shelf": {Arg: "shelf"}, "title": {Arg: "title"}}},
			{Name: "read", Operation: "items.get", Version: 1,
				With: map[string]Term{"id": {Step: "made", Field: "key"}}},
		},
	}); err != nil {
		t.Fatalf("a key from a one-row step was refused across a call boundary: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	result, err := s.Invoke(Caller{}, "catalog.add_and_read", 0, map[string]any{
		"shelf": "a", "title": "the one",
	})
	if err != nil {
		t.Fatalf("running it: %v", err)
	}
	if result.Changed != 1 {
		t.Fatalf("changed %d, want 1", result.Changed)
	}
	if len(result.Rows) != 1 || result.Rows[0]["title"] != "the one" {
		t.Fatalf("the second leg did not read back what the first leg wrote: %+v", result.Rows)
	}
}

// ---------------------------------------------------------------------------
// The claim task 0070 §2.3 calls the strongest thing this design has, measured
// rather than believed: the ceiling of a composed operation, at any depth, is
// the number that operation itself declares. Not a product of the numbers
// underneath it, and not a sum anybody has to work out.
// ---------------------------------------------------------------------------

func TestTheCeilingOfAComposedOperationAtAnyDepthIsTheLimitItDeclares(t *testing.T) {
	s := composed(t)

	// A tower. Every level has two legs, each leg the level below it, and
	// every level declares exactly the sum of its legs.
	//
	//	level 0  items.on_shelf   scan, limit 50
	//	level 1  two of level 0    limit 100
	//	level 2  two of level 1    limit 200
	//	level 3  two of level 2    limit 400
	//
	// A reading where a leg runs once per row of another leg — the fan-out
	// this design refuses — would put 50*50 = 2500 at level 1 alone, and
	// 2500*2500 at level 2. The numbers below are what say which reading the
	// code implements.
	leg := func(name string, version int) Step {
		return Step{Operation: name, Version: version, With: map[string]Term{"shelf": {Arg: "shelf"}}}
	}
	tower := []struct {
		name  string
		below string
		limit int
	}{
		{"tower.1", "items.on_shelf", 100},
		{"tower.2", "tower.1", 200},
		{"tower.3", "tower.2", 400},
	}
	for _, level := range tower {
		if _, err := s.DeclareOperation(Caller{}, Operation{
			Name: level.name, Collection: "items", Action: ActionBatch,
			Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
			Limit: level.limit,
			Steps: []Step{leg(level.below, 1), leg(level.below, 1)},
		}); err != nil {
			t.Fatalf("declaring %s: %v", level.name, err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	// The positive count this measurement needs before any of it is believed:
	// four operations exist to ask about, and they are composed, not flat.
	checked := 0
	for _, level := range append([]string{}, "tower.1", "tower.2", "tower.3") {
		operation, found, err := s.Operation(level, 0)
		if err != nil || !found {
			t.Fatalf("%s is not declared, so nothing below measures anything: %v", level, err)
		}
		reach, err := s.ceiling(operation, newCosts())
		if err != nil {
			t.Fatalf("working out the ceiling of %s: %v", level, err)
		}
		if reach != operation.Limit {
			t.Fatalf("%s declares a limit of %d and its ceiling is %d — the number written in the declaration is not the ceiling",
				level, operation.Limit, reach)
		}
		checked++
	}
	if checked != 3 {
		t.Fatalf("only %d levels were measured; a measurement that ran on nothing is not a measurement", checked)
	}

	// And the product reading, named so the difference is on the record: at
	// level 3 a fan-out would be 50 to the eighth power. It is 400.
	deepest, _, err := s.Operation("tower.3", 0)
	if err != nil {
		t.Fatal(err)
	}
	reach, err := s.ceiling(deepest, newCosts())
	if err != nil {
		t.Fatal(err)
	}
	product := 1
	for i := 0; i < 8; i++ {
		product *= 50
	}
	if reach >= product {
		t.Fatalf("the ceiling at depth 3 is %d, which is the product reading (%d), not the sum reading", reach, product)
	}
	if reach != 400 {
		t.Fatalf("the ceiling at depth 3 is %d, want the 400 it declares", reach)
	}
}

// M2's target: the ceilings of the legs must add up to no more than the limit
// the composed operation declares. Without this the number printed in the
// declaration stops being the ceiling and the reader has to add it up
// themselves — which is the thing §2.3 says must not be true.
func TestAComposedOperationWhoseLegsOutgrowItsLimitIsRefused(t *testing.T) {
	s := composed(t)

	over := page()
	over.Name = "catalog.page_too_small"
	over.Limit = 50 // the scan alone may return 50, and the totals is one more

	_, err := s.DeclareOperation(Caller{}, over)
	if !errors.Is(err, ErrDeclaration) {
		// Say what was lost, not only that a refusal did not arrive: the
		// number the declaration prints has stopped being the ceiling, which
		// is the whole of what this rule is for.
		stored, found, lookup := s.Operation(over.Name, 0)
		reach := -1
		if lookup == nil && found {
			reach, _ = s.ceiling(stored, newCosts())
		}
		t.Fatalf("legs adding up to 51 under a limit of 50 were not refused (%v) — %q is now stored declaring a limit of %d with a real ceiling of %d, so the number a reader sees is a lie",
			err, over.Name, over.Limit, reach)
	}
	if !strings.Contains(err.Error(), "51") || !strings.Contains(err.Error(), "50") {
		t.Fatalf("the refusal must print both numbers — the sum it worked out and the limit that was declared: %v", err)
	}

	// The control on the same path: one more row of headroom and it is taken.
	// A table of nothing but refusals measures nothing.
	fits := page()
	fits.Name = "catalog.page_just_right"
	fits.Limit = 51
	if _, err := s.DeclareOperation(Caller{}, fits); err != nil {
		t.Fatalf("legs adding up to exactly the declared limit were refused: %v", err)
	}
}

func TestAComposedOperationThatDeclaresNoLimitIsRefused(t *testing.T) {
	s := composed(t)

	silent := page()
	silent.Name = "catalog.page_silent"
	silent.Limit = 0

	_, err := s.DeclareOperation(Caller{}, silent)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a composed operation with no declared limit was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "how many rows it may return") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// M3's target, and the one that matters most because nothing breaks when it
// goes: a reference pins a version, so a later declaration of the callee
// cannot change what an earlier caller costs.
//
// There is no error to look for here. If pinning is swapped for following the
// newest name, every call still works and every answer still looks like an
// answer — the only thing that changes is that the number written in
// catalog.page stops being true, silently, from the moment somebody else
// redeclares something. So this is measured twice: the ceiling the declaration
// promises, and the rows a call actually returns.
// ---------------------------------------------------------------------------

func TestRedeclaringACalleeDoesNotMoveTheCeilingOfWhatAlreadyCallsIt(t *testing.T) {
	s := composed(t)
	stock(t, s, "a", 60)

	if _, err := s.DeclareOperation(Caller{}, page()); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	before, _, err := s.Operation("catalog.page", 0)
	if err != nil {
		t.Fatal(err)
	}
	was, err := s.ceiling(before, newCosts())
	if err != nil {
		t.Fatal(err)
	}
	if was != 51 {
		t.Fatalf("catalog.page starts at a ceiling of %d, want the 51 it declares", was)
	}

	// The control: it really is reading the shelf, and really is stopping at
	// the limit its callee declares. 60 items on the shelf, 50 in the answer.
	first, err := s.Invoke(Caller{}, "catalog.page", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 51 {
		t.Fatalf("got %d rows, want 50 items and 1 total", len(first.Rows))
	}

	// Now somebody redeclares the callee, a hundred times wider. The new
	// version is real and callable; what must not happen is catalog.page
	// following it.
	wider := Operation{
		Name: "items.on_shelf", Collection: "items", Action: ActionScan,
		Index: "by_shelf",
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "shelf"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "shelf"}}},
		Limit: 5000,
	}
	stored, err := s.DeclareOperation(Caller{}, wider)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 {
		t.Fatalf("the redeclaration is version %d, want 2 — this test's own fixture is wrong", stored.Version)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	// The control that the redeclaration took effect at all, so that the
	// assertions after it are about pinning and not about a write that never
	// landed.
	wide, err := s.Invoke(Caller{}, "items.on_shelf", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wide.Rows) != 60 {
		t.Fatalf("the new version returned %d rows, want all 60 — it did not take effect", len(wide.Rows))
	}

	after, _, err := s.Operation("catalog.page", 0)
	if err != nil {
		t.Fatal(err)
	}
	now, err := s.ceiling(after, newCosts())
	if err != nil {
		t.Fatal(err)
	}
	if now != was {
		t.Fatalf("catalog.page declares a limit of %d and its ceiling moved from %d to %d when somebody else redeclared items.on_shelf — the number in the declaration is no longer the ceiling",
			after.Limit, was, now)
	}

	second, err := s.Invoke(Caller{}, "catalog.page", 0, map[string]any{"shelf": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != len(first.Rows) {
		t.Fatalf("catalog.page returned %d rows after the redeclaration and %d before it, without being redeclared itself",
			len(second.Rows), len(first.Rows))
	}
}

func TestAStepMustPinTheVersionOfTheOperationItCalls(t *testing.T) {
	s := composed(t)

	loose := page()
	loose.Name = "catalog.page_loose"
	loose.Steps[0].Version = 0

	_, err := s.DeclareOperation(Caller{}, loose)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("an unpinned reference was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "without pinning a version") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}

	missing := page()
	missing.Name = "catalog.page_missing"
	missing.Steps[0].Version = 9

	_, err = s.DeclareOperation(Caller{}, missing)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a reference to a version that does not exist was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "version 9") {
		t.Fatalf("the refusal does not name the version asked for: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The rest of the declare-time rules, one case each.
// ---------------------------------------------------------------------------

func TestACountMayNotBeAStepOfAComposedOperation(t *testing.T) {
	s := composed(t)

	counting := page()
	counting.Name = "catalog.page_counting"
	counting.Steps[1] = Step{
		Name: "total", Operation: "items.count_on_shelf", Version: 1,
		With: map[string]Term{"shelf": {Arg: "shelf"}},
	}

	_, err := s.DeclareOperation(Caller{}, counting)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a count as a leg was accepted, and its number has nowhere to go: %v", err)
	}
	if !strings.Contains(err.Error(), "count") || !strings.Contains(err.Error(), "totals") {
		t.Fatalf("the refusal should say what is wrong and what to use instead: %v", err)
	}
}

func TestAStepIsEitherACollectionOrAnOperationAndNeverBoth(t *testing.T) {
	s := composed(t)

	both := page()
	both.Name = "catalog.page_both"
	both.Steps[0].Action = ActionGet
	both.Steps[0].Collection = "items"

	_, err := s.DeclareOperation(Caller{}, both)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a step that both calls and touches was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "either touches a collection or calls an operation") {
		t.Fatalf("the refusal does not say which two things are in conflict: %v", err)
	}
}

func TestTheArgumentsOfACalledOperationAreCheckedAgainstItsOwnDeclaration(t *testing.T) {
	s := composed(t)

	for _, bad := range []struct {
		name  string
		with  map[string]Term
		says  string
		shelf string
	}{
		{name: "unknown argument", with: map[string]Term{"shelf": {Arg: "shelf"}, "nope": {Value: "x"}}, says: "does not take"},
		{name: "nothing for a required one", with: map[string]Term{}, says: "and nothing is passed for it"},
		{name: "the wrong type", with: map[string]Term{"shelf": {Value: float64(3)}}, says: "is a string"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			wrong := page()
			wrong.Name = "catalog.page_" + strings.ReplaceAll(bad.name, " ", "_")
			wrong.Steps[0].With = bad.with

			_, err := s.DeclareOperation(Caller{}, wrong)
			if !errors.Is(err, ErrDeclaration) {
				t.Fatalf("accepted: %v", err)
			}
			if !strings.Contains(err.Error(), bad.says) {
				t.Fatalf("the refusal does not say %q: %v", bad.says, err)
			}
		})
	}
}

func TestAComposedOperationAsksForEveryScopeTheOperationsItCallsAskFor(t *testing.T) {
	s := composed(t)

	guarded := Operation{
		Name: "items.on_shelf_guarded", Collection: "items", Action: ActionScan,
		Index: "by_shelf",
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "shelf"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "shelf"}}},
		Limit: 5, Scopes: []string{"catalog:read"},
	}
	if _, err := s.DeclareOperation(Caller{}, guarded); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	calling := Operation{
		Name: "catalog.sneak", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Limit: 10,
		Steps: []Step{{
			Name: "rows", Operation: "items.on_shelf_guarded", Version: 1,
			With: map[string]Term{"shelf": {Arg: "shelf"}},
		}},
	}

	_, err := s.DeclareOperation(Caller{}, calling)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("an operation that calls a scoped one without asking for the scope was accepted — that is a way round allowed(): %v", err)
	}
	if !strings.Contains(err.Error(), "catalog:read") {
		t.Fatalf("the refusal does not name the scope: %v", err)
	}

	// The control: ask for it, and it is accepted — and then a caller that
	// does not hold it is refused at call time by the check that already
	// existed.
	calling.Scopes = []string{"catalog:read"}
	if _, err := s.DeclareOperation(Caller{}, calling); err != nil {
		t.Fatalf("asking for the scope its leg asks for was still refused: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Invoke(Caller{}, "catalog.sneak", 0, map[string]any{"shelf": "a"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a caller holding no scope ran it anyway: %v", err)
	}
	if _, err := s.Invoke(Caller{Scopes: []string{"catalog:read"}}, "catalog.sneak", 0, map[string]any{"shelf": "a"}); err != nil {
		t.Fatalf("a caller holding the scope was refused: %v", err)
	}
}

// A composed write is one transaction, which is the thing composing actually
// buys: every leg lands or none of them does.
func TestAComposedWriteIsOneTransaction(t *testing.T) {
	s := composed(t)

	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "items.add_strict", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "shelf", Type: TypeString, Required: true},
		},
		Steps: []Step{{
			Action: ActionInsert, Collection: "items",
			Document: map[string]Term{"id": {Arg: "id"}, "shelf": {Arg: "shelf"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeclareOperation(Caller{}, Operation{
		Name: "catalog.add_two", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "one", Type: TypeString, Required: true},
			{Name: "two", Type: TypeString, Required: true},
		},
		Limit: 10,
		Steps: []Step{
			{Name: "a", Operation: "items.add_strict", Version: 1,
				With: map[string]Term{"id": {Arg: "one"}, "shelf": {Value: "a"}}},
			{Name: "b", Operation: "items.add_strict", Version: 1,
				With: map[string]Term{"id": {Arg: "two"}, "shelf": {Value: "a"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Invoke(Caller{}, "catalog.add_two", 0, map[string]any{"one": "k1", "two": "k2"}); err != nil {
		t.Fatalf("both legs should have landed: %v", err)
	}

	// Now the second leg cannot: k2 is taken. The first leg of THIS call
	// writes k3, and it must not survive.
	_, err := s.Invoke(Caller{}, "catalog.add_two", 0, map[string]any{"one": "k3", "two": "k2"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists from the second leg, got %v", err)
	}

	items, err := s.Collection("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := items.Get("k3"); err != nil || found {
		t.Fatalf("the first leg's write survived the second leg's failure: found=%v err=%v", found, err)
	}
	if _, found, err := items.Get("k1"); err != nil || !found {
		t.Fatalf("the earlier successful call was rolled back too: found=%v err=%v", found, err)
	}
}
