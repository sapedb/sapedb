package store

import "testing"

// This file is the store's own half of the attribution the server measures
// over the wire. Three record() calls in store.go carried no By at all: the
// first declaration of a collection, the redeclaration that upgrades one, and
// the drop that destroys one.
//
// The third is the worst of the three, and not by a little. A declaration can
// be read back off the collection that now exists — the log entry is a
// convenience. A drop leaves NOTHING behind to ask: the documents, the index
// entries and the catalogue key are all gone, and the only record that the
// collection ever existed is the entry that did not say who removed it.
//
// Measured here rather than only through the server because the server can
// only reach two of the three: nothing declares a drop over the wire, and a
// rule that only holds where a test happens to point is a rule waiting to be
// removed as unused.

// TestTheChangeLogSaysWhoDeclaredACollection covers both of store.Declare's
// record() calls: the first declaration and the upgrade-in-place. They are
// separate calls in separate branches, so a caller plumbed into one of them is
// a caller plumbed into neither for a database anybody has deployed twice.
func TestTheChangeLogSaysWhoDeclaredACollection(t *testing.T) {
	_, store := fresh(t, 40)

	// The known-negative first, and it is a real one rather than a formality:
	// an empty Caller has to come back empty, or an actor found further down
	// could be a constant this path always writes.
	if _, err := store.Declare(Caller{}, Spec{Name: "anonymous", Key: Key{Path: "id", Type: TypeString}}); err != nil {
		t.Fatal(err)
	}

	first := articles()
	first.Indexes = first.Indexes[:1]
	if _, err := store.Declare(Caller{Actor: "ann"}, first); err != nil {
		t.Fatal(err)
	}
	// The upgrade: the same name, one more index, which takes the other
	// record() call.
	if _, err := store.Declare(Caller{Actor: "bo"}, articles()); err != nil {
		t.Fatal(err)
	}

	entries := 0
	actors := map[string][]string{}
	operations := map[string]string{}
	if err := store.Changes(0, func(change Change) bool {
		entries++
		if change.Kind != ChangeDeclare {
			return true
		}
		actors[change.Collection] = append(actors[change.Collection], change.By.Actor)
		operations[change.Collection] = change.By.Operation
		return true
	}); err != nil {
		t.Fatal(err)
	}

	// A walk that found nothing is a broken measurement, not an empty log.
	if entries == 0 {
		t.Fatal("the change log walk found no entries at all after three declarations — this test is measuring nothing")
	}
	if len(actors) == 0 {
		t.Fatal("the change log holds no collection entries after three declarations")
	}

	if got := actors["anonymous"]; len(got) != 1 || got[0] != "" {
		t.Errorf("a collection declared with an empty Caller is logged against %q — the actor comes from somewhere other than the caller", got)
	}
	got := actors["articles"]
	if len(got) != 2 {
		t.Fatalf("the log holds %d entries for articles, want 2 — the declaration and the upgrade", len(got))
	}
	if got[0] != "ann" {
		t.Errorf("the first declaration is logged against %q, want %q", got[0], "ann")
	}
	if got[1] != "bo" {
		t.Errorf("the upgrade is logged against %q, want %q — the second record() call took no caller", got[1], "bo")
	}
	if operations["articles"] != "declare" {
		t.Errorf("a collection declaration is logged under %q, want %q", operations["articles"], "declare")
	}
}

// TestTheChangeLogSaysWhoDroppedACollection is the third of the three, and the
// one where the missing name could not be recovered from anywhere else.
func TestTheChangeLogSaysWhoDroppedACollection(t *testing.T) {
	_, store := fresh(t, 41)

	if _, err := store.Declare(Caller{Actor: "ann"}, articles()); err != nil {
		t.Fatal(err)
	}
	if err := store.Drop(Caller{Actor: "cy"}, "articles"); err != nil {
		t.Fatal(err)
	}

	entries, drops := 0, 0
	var dropped *Change
	if err := store.Changes(0, func(change Change) bool {
		entries++
		if change.Kind != ChangeDrop {
			return true
		}
		drops++
		one := change
		dropped = &one
		return true
	}); err != nil {
		t.Fatal(err)
	}

	if entries == 0 {
		t.Fatal("the change log walk found no entries at all after a declaration and a drop — this test is measuring nothing")
	}
	if drops == 0 {
		t.Fatal("the change log holds no drop entry after a collection was dropped, so nothing below is looking at one")
	}
	if dropped.Collection != "articles" {
		t.Fatalf("the drop entry names collection %q", dropped.Collection)
	}
	if dropped.By.Actor != "cy" {
		t.Errorf("a collection dropped by %q is logged against actor %q — the only record that this collection ever existed does not say who removed it",
			"cy", dropped.By.Actor)
	}
	if dropped.By.Operation != "drop" {
		t.Errorf("a drop is logged under %q, want %q", dropped.By.Operation, "drop")
	}

	// The known-negative for this arm too: an embedder that passes no caller
	// gets an entry that says so, rather than one that says "cy" because the
	// name came from anywhere but the caller.
	if _, err := store.Declare(Caller{}, Spec{Name: "quiet", Key: Key{Path: "id", Type: TypeString}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Drop(Caller{}, "quiet"); err != nil {
		t.Fatal(err)
	}
	anonymous := 0
	if err := store.Changes(0, func(change Change) bool {
		if change.Kind == ChangeDrop && change.Collection == "quiet" {
			anonymous++
			if change.By.Actor != "" {
				t.Errorf("a drop with an empty Caller is logged against actor %q", change.By.Actor)
			}
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if anonymous == 0 {
		t.Fatal("no drop entry for the second collection, so the contrast above was never checked")
	}
}
