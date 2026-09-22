package store

import "testing"

// TestRunBatchRollsBackAndRepanicsWhenAStepPanics proves the defer added to
// runBatch (batch.go): a panic partway through a step must still leave the
// store's own invariant intact — s.pages.Pending() false again, whatever the
// batch had written thrown away — before the same panic value continues on
// its way.
//
// Every input-reachable panic this package's own history found (task 0042's
// sameValue, task 0049's satisfies/describe, task 0054's backward bool
// range) is already closed by the time this task starts — a targeted search
// of batch.go, ops.go, collection.go and keys.go for an unguarded type
// assertion, an uncomparable map key, an explicit panic(), or a division
// found none reachable through DeclareOperation + Invoke, the two public
// doors. So this cannot be measured by feeding a crafted argument through
// the public API the way cycle_test.go measures a stack overflow: there is
// currently nothing left in the write path that panics on its own.
//
// What can still be measured, and is measured here, is the defer itself:
// runBatch is called directly, the way cycle_test.go's argument-door test
// calls Invoke directly, except one step further in — a *Collection is
// poked into s.collections as a bare nil, a shape Declare() itself can
// never produce (every real declaration always populates a concrete
// *Collection), and then named in a step whose action dereferences it. That
// manufactures a real Go panic (nil pointer dereference), not a stand-in for
// one, and is only reachable this way: from inside this package, calling
// the unexported runBatch, past validation DeclareOperation would refuse to
// let this same Operation through in the first place. The defer under test
// does not know or care that this is how its panic arrived — it recovers
// whatever runStep panics with, regardless of cause.
func TestRunBatchRollsBackAndRepanicsWhenAStepPanics(t *testing.T) {
	_, s := fresh(t, 8301)
	_, err := s.Declare(Caller{}, Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if s.pages.Pending() {
		t.Fatal("setup is wrong: expected nothing pending before the batch runs")
	}

	// A *Collection Declare() can never produce: a bare nil under a real
	// name, reachable only by poking the map directly from inside this
	// package.
	s.collections["broken"] = nil

	// Step 1 succeeds and writes a real document — this is what makes
	// s.pages.Pending() become true partway through the batch, the state a
	// rollback has to undo. Step 2 panics before the batch ever reaches
	// Commit, so if the defer under test did nothing, this document would
	// stay behind as a half-finished batch's leftovers — exactly what
	// runBatch's own godoc says a batch exists to prevent.
	op := Operation{
		Name: "boom", Collection: "widgets", Action: ActionBatch,
		Steps: []Step{
			{Action: ActionPut, Collection: "widgets", Document: map[string]Term{"id": {Value: "in-the-batch"}}},
			{Action: ActionPut, Collection: "broken", Document: map[string]Term{}},
		},
	}

	var result Result
	panicked := func() (r any) {
		defer func() { r = recover() }()
		_ = s.runBatch(Attribution{Operation: op.Name}, op, nil, &result, &budget{most: s.budget})
		return nil
	}()

	if panicked == nil {
		t.Fatal("expected runBatch to panic (a nil *Collection was dereferenced), it returned instead")
	}
	// Not asserting the exact runtime error text here — only that the panic
	// happened at all and (below) that runBatch's own defer put the store
	// back into a usable state before letting it continue.
	if _, ok := panicked.(error); !ok {
		t.Fatalf("expected the panic value to be an error (a Go nil-pointer panic already is one); got %T: %v", panicked, panicked)
	}

	if s.pages.Pending() {
		t.Fatal("s.pages.Pending() is still true after the panic — runBatch did not roll back before re-panicking, so every future call on this Store is now refused with ErrUncommitted forever")
	}

	// Rollback (store.go) refuses every handle taken before it, by design
	// ("a caller that rolls back and carries on asks for what it needs
	// again") — so widgets, captured before the panic, is stale now, and
	// the checks below take a fresh handle rather than reusing it.
	after, err := s.Collection("widgets")
	if err != nil {
		t.Fatalf("widgets should still be declared after the rollback (it was committed before the batch ran): %v", err)
	}
	if _, found, err := after.Get("in-the-batch"); err != nil {
		t.Fatalf("checking whether step 1's write survived: %v", err)
	} else if found {
		t.Fatal("step 1's write survived the panic in step 2 — the batch did not roll back")
	}
	// The "broken" nil *Collection this test poked in directly does not
	// survive the rollback either — Rollback rebuilds s.collections from
	// what is actually committed on disk (store.go's load()), which never
	// contained it — confirming the rollback was real, not this test
	// quietly cleaning up after itself.
	if _, err := s.Collection("broken"); err == nil {
		t.Fatal("the manufactured nil *Collection survived the rollback")
	}

	// The store must still be usable afterwards: Pending() being false is
	// necessary but not sufficient, an ordinary write proves the recovery
	// left a store that works.
	if _, err := after.Put(map[string]any{"id": "after-the-panic"}); err != nil {
		t.Fatalf("the store is unusable after recovering from the panic: %v", err)
	}
}
