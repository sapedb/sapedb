package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// These are characterisation tests. They do not assert that today's behaviour
// is right — SAPE-9 is the ticket that decides that, and docs/namespaces.md is
// where the decision goes. They assert that today's behaviour is what
// docs/namespaces.md says it is, so that document cannot quietly stop being
// true, and so a change to name resolution arrives as a red suite rather than
// as a surprise in somebody's database.
//
// Every one of them names a claim in that document. If the product decides to
// make collisions refusable, these go red, and that is the intended way to
// find out which claims the decision overturned.

// collidingRead is what the first vendor declares: a read of one document by
// its primary key.
func collidingRead(name string) Operation {
	return Operation{
		Name: name, Collection: "articles", Action: ActionGet,
		Key:        &Term{Arg: "id"},
		Input:      []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Projection: []string{"id", "title"},
	}
}

// collidingDelete is what the second vendor declares under the same name: the
// same argument, the same call site, a destructive action.
func collidingDelete(name string) Operation {
	return Operation{
		Name: name, Collection: "articles", Action: ActionDelete,
		Key:   &Term{Arg: "id"},
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
	}
}

// TestASecondDeclarationOfANameTakesOverEveryUnversionedCall is the whole of
// SAPE-9 in one function: two parties declare "orders.recent", neither is told
// about the other, and the caller who was built against the first one is
// running the second one's declaration on its next call.
//
// The second declaration is a delete rather than another read, because the
// interesting question is not whether the newest version wins — it is what the
// newest version is allowed to be. Nothing about the second declaration has to
// resemble the first: not the action, not the collection, not the scopes.
func TestASecondDeclarationOfANameTakesOverEveryUnversionedCall(t *testing.T) {
	store, collection := declared(t, 900)
	fill(t, collection)

	// The known-positive, before anything collides: the name resolves, and it
	// resolves to a read that returns a row. Without this, every "the caller
	// got something else" below could be a test that never reached a document.
	first, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.recent"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("the first declaration of a name is version %d, want 1", first.Version)
	}
	before, err := store.Invoke(Caller{}, "orders.recent", 0, map[string]any{"id": "a0"})
	if err != nil {
		t.Fatal(err)
	}
	if before.Count != 1 || len(before.Rows) != 1 {
		t.Fatalf("the first vendor's read returned %d rows before any collision — this test is measuring nothing", len(before.Rows))
	}

	// The second party declares the same name. It is not refused and it is
	// not warned about: the only thing that says anything happened is the
	// version number in the answer, which a caller has to go and read.
	second, err := store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("orders.recent"))
	if err != nil {
		t.Fatalf("declaring over an existing name was refused: %v — if this is now the behaviour, docs/namespaces.md is out of date", err)
	}
	if second.Version != 2 {
		t.Fatalf("declaring over an existing name produced version %d, want 2", second.Version)
	}
	t.Logf("declare %q by vendor-a: version %d, action %q", first.Name, first.Version, first.Action)
	t.Logf("declare %q by vendor-b: version %d, action %q, refused: false, warned: false",
		second.Name, second.Version, second.Action)

	// The first vendor's caller has not changed a line. It calls the name it
	// was given, with the argument it was given, and deletes a document.
	after, err := store.Invoke(Caller{Actor: "the-first-vendors-app"}, "orders.recent", 0, map[string]any{"id": "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != 2 {
		t.Errorf("an unversioned call ran version %d, want 2 — Invoke takes the newest version of a name", after.Version)
	}
	if after.Changed != 1 {
		t.Errorf("a call that was declared as a read changed %d documents, want 1 — the second declaration decided what this call does", after.Changed)
	}
	if _, found, err := collection.Get("a1"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("the document survived, so the call above did not do what its Changed count says")
	}
	t.Logf("Invoke(%q, version 0, id=a1) by the first vendor's app: version %d, rows %d, changed %d, document a1 still present: false",
		"orders.recent", after.Version, len(after.Rows), after.Changed)

	// InvokeVersion is the whole of the escape hatch, and it works: the first
	// vendor's declaration is still there and still runs, for a caller that
	// knows the number 1. Nothing hands that number to a caller who did not
	// record it at declare time — see TestWhatIsHereShowsOnlyTheNewestVersionOfAName.
	pinned, err := store.Invoke(Caller{}, "orders.recent", 1, map[string]any{"id": "a0"})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Version != 1 {
		t.Errorf("InvokeVersion(1) ran version %d, want 1", pinned.Version)
	}
	if pinned.Count != 1 {
		t.Errorf("the pinned version returned %d rows, want 1 — the older declaration should still run unchanged", pinned.Count)
	}
	t.Logf("InvokeVersion(%q, 1, id=a0): version %d, rows %d, changed %d",
		"orders.recent", pinned.Version, len(pinned.Rows), pinned.Changed)
}

// TestWhatIsHereShowsOnlyTheNewestVersionOfAName measures what the one
// discovery surface in the product reports after a collision. It is the reason
// InvokeVersion is a weaker escape hatch than it looks: the catalogue does not
// say a name has two declarations, so a caller that did not record its version
// at declare time has nothing to read the number off.
func TestWhatIsHereShowsOnlyTheNewestVersionOfAName(t *testing.T) {
	store, _ := declared(t, 901)

	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.recent")); err != nil {
		t.Fatal(err)
	}
	// A second, uncontested name, so that "one entry per name" is measured
	// against a catalogue that holds more than one entry.
	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("orders.recent")); err != nil {
		t.Fatal(err)
	}

	here, err := store.WhatIsHere(Caller{Actor: "an-operator"})
	if err != nil {
		t.Fatal(err)
	}
	if len(here.Operations) == 0 {
		t.Fatal("the catalogue holds no operations after three declarations — this test is measuring nothing")
	}

	seen := map[string][]int{}
	for _, operation := range here.Operations {
		seen[operation.Name] = append(seen[operation.Name], operation.Version)
	}
	if len(seen) != 2 {
		t.Fatalf("the catalogue holds %d distinct names after three declarations, want 2", len(seen))
	}
	if got := seen["orders.one"]; len(got) != 1 || got[0] != 1 {
		t.Errorf("the uncontested name is listed as %v, want [1]", got)
	}
	if got := seen["orders.recent"]; len(got) != 1 || got[0] != 2 {
		t.Errorf("the contested name is listed as %v, want [2] — the catalogue reports one version per name, and it is the newest", got)
	}

	// Said as a claim rather than left implied: the first vendor's
	// declaration is stored, runnable and absent from the only list of what
	// this database holds.
	if _, found, err := store.Operation("orders.recent", 1); err != nil || !found {
		t.Fatalf("version 1 is not readable by number (found=%v, err=%v), so the claim above is about nothing", found, err)
	}
	for _, operation := range here.Operations {
		t.Logf("WhatIsHere lists: name %q version %d action %q", operation.Name, operation.Version, operation.Action)
	}
}

// TestAStoredDeclarationSaysWhoDeclaredItAndTheLogAgrees replaces
// TestNothingOnAStoredDeclarationSaysWhoDeclaredIt, which measured the gap
// ISS-32 closed: the change log knew who declared an operation and the
// declaration itself did not, so the answer vanished with the log — under a
// Retain cap, or through a dump and restore, which carries no log at all. See
// the note under "Does anything record who declared each version" in
// docs/namespaces.md, which is where the old test's output is quoted.
//
// Deleting the old assertion rather than keeping both, because the old one was
// the claim that a field is absent and the field is now there; the two cannot
// simultaneously be true. What is kept is the part that is still worth
// measuring and is stronger than either half alone: the declaration and the
// log have to name the SAME declarer. Two records of one fact that can drift
// apart are worse than one record, and nothing else would notice if they did.
func TestAStoredDeclarationSaysWhoDeclaredItAndTheLogAgrees(t *testing.T) {
	store, _ := declared(t, 902)

	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.recent")); err != nil {
		t.Fatal(err)
	}

	stored, found, err := store.Operation("orders.recent", 1)
	if err != nil || !found {
		t.Fatalf("reading back the declaration: found=%v err=%v", found, err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	// The known-positive for the search: the encoded declaration is the thing
	// under test and it does carry the fields it is supposed to.
	if !strings.Contains(string(encoded), `"name":"orders.recent"`) {
		t.Fatalf("the encoded declaration is not the one declared: %s", encoded)
	}
	if !strings.Contains(string(encoded), "vendor-a") {
		t.Errorf("the stored declaration does not name its declarer: %s", encoded)
	}

	// One spelling, and only one. Two fields carrying the same fact is how a
	// reader ends up asking which of them wins; the alternatives swept here
	// are the names a second one would plausibly arrive under.
	fields := map[string]any{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, carries := fields["declaredBy"]; !carries {
		t.Errorf("the stored declaration has no declaredBy field: %s", encoded)
	}
	for _, field := range []string{"actor", "by", "owner", "declared_by", "namespace", "vendor"} {
		if _, carries := fields[field]; carries {
			t.Errorf("a stored declaration carries a %q field as well as declaredBy — two records of one fact", field)
		}
	}

	// The log still knows too, and it has to agree. This was the contrast the
	// old test drew; it is now an equality.
	actors := map[int]string{}
	entries := 0
	if err := store.Changes(0, func(change Change) bool {
		entries++
		if change.Kind == ChangeOperation && change.Operation != nil {
			actors[change.Operation.Version] = change.By.Actor
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if entries == 0 {
		t.Fatal("the change log walk found no entries at all — the comparison below is measuring nothing")
	}
	if actors[1] != "vendor-a" {
		t.Errorf("the log records version 1 as declared by %q, want %q", actors[1], "vendor-a")
	}
	if stored.DeclaredBy == nil {
		t.Fatalf("the declaration carries no declarer while the log says %q", actors[1])
	}
	if stored.DeclaredBy.Identity != actors[1] {
		t.Errorf("the declaration says %q declared version 1 and the log says %q — the two records of one fact disagree",
			stored.DeclaredBy.Identity, actors[1])
	}
	t.Logf("stored declaration: %s", encoded)
	t.Logf("change log says version 1 was declared by %q", actors[1])
}

// TestTheLogEntryNamingADeclarerIsPrunable measures the lifetime of the one
// place that does record who declared a version.
//
// Retain is not an exotic setting — its own doc says a cap "is not optional in
// practice". Under one, the entry that names who declared version 1 is dropped
// like any other entry, while version 1 itself stays stored and runnable. So
// "the log knows" has an expiry date that the declaration it describes does
// not.
func TestTheLogEntryNamingADeclarerIsPrunable(t *testing.T) {
	store, collection := declared(t, 903)

	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.recent")); err != nil {
		t.Fatal(err)
	}

	// The known-positive: with no cap, the entry is there to be found.
	if !declarerRecorded(t, store, "vendor-a") {
		t.Fatal("the log does not name the declarer even before anything was trimmed — this test is measuring nothing")
	}

	store.Retain(3)
	for i := 0; i < 12; i++ {
		put(t, collection, map[string]any{
			"id": "later", "author": "someone", "published": float64(i), "slug": "later",
		})
		if _, err := store.Invoke(Caller{Actor: "an-app"}, "orders.recent", 1, map[string]any{"id": "a0"}); err != nil {
			t.Fatal(err)
		}
	}

	if declarerRecorded(t, store, "vendor-a") {
		t.Error("the log still names the declarer under Retain(3) after twelve later entries; docs/namespaces.md claims it does not")
	} else {
		t.Log("Retain(3) + 12 later entries: the log no longer names who declared version 1")
	}

	// And the declaration it described is still here, still the thing a name
	// resolves to. That asymmetry is the finding.
	if _, found, err := store.Operation("orders.recent", 1); err != nil || !found {
		t.Fatalf("version 1 went with the log entry (found=%v err=%v) — then the asymmetry this test reports does not exist", found, err)
	}
}

// declarerRecorded reports whether the change log still holds an entry saying
// this actor declared an operation.
func declarerRecorded(t *testing.T, store *Store, actor string) bool {
	t.Helper()
	found := false
	if err := store.Changes(0, func(change Change) bool {
		if change.Kind == ChangeOperation && change.By.Actor == actor {
			found = true
			return false
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// TestAnOperationNameMayHoldAnySeparatorAName is the measurement behind "a
// prefix convention costs nothing to ship": usableName refuses an empty name,
// a name over 128 bytes and a name holding a zero byte, and nothing else. Any
// separator a convention might pick is already legal, and so is a name that
// ignores the convention entirely.
//
// It is also the measurement behind "validating a namespace at declare time
// cannot be added in 1.x": every name below is valid in 1.0, so a 1.1 that
// refused any of them would be tightening a rule, which COMPATIBILITY.md
// section 3 forbids.
func TestAnOperationNameMayHoldAnySeparatorAName(t *testing.T) {
	store, _ := declared(t, 904)

	for _, name := range []string{
		"recent",
		"orders.recent",
		"orders/recent",
		"acme:orders.recent",
		"@acme/orders.recent",
		"orders.recent.v2.final.really",
		strings.Repeat("n", 128),
	} {
		if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead(name)); err != nil {
			t.Errorf("declaring %q was refused: %v — a name rule tighter than usableName has appeared", name, err)
		}
	}

	// The known-negatives, so that "everything is accepted" is not what the
	// loop above is really measuring.
	for _, name := range []string{"", strings.Repeat("n", 129), "orders\x00recent"} {
		if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead(name)); err == nil {
			t.Errorf("declaring %q was accepted, and usableName says it should not be", name)
		}
	}
}
