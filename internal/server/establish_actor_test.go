package server

import (
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestEstablishingOverTheWireGoesIntoTheLogWithAnActor is the audit hole this
// frame would have opened, closed at the same time it was cut.
//
// Declare arrived without an actor and had one added afterwards, once somebody
// noticed the entry recording the most consequential change there is — the one
// that decides what everybody else may do — was the one entry with nobody's
// name on it. A collection declaration is the same kind of change with a
// larger blast radius: it is where documents go, and an index added to it is
// a cost every writer pays from then on.
//
// It had been blank longer than Declare's was. store.Declare recorded
// `Change{Kind: ChangeDeclare, ...}` with no By from the day the change log
// existed, and `sapedb apply` was the only way to reach it, so the blank was
// survivable in the same way: whoever did it was standing at the machine
// holding the file's exclusive lock. This frame ends that, which is why the
// caller was plumbed through store.Declare in the same change rather than
// left for later.
//
// The order of what is measured is the point:
//
//  1. the log reader finds anything at all (a walk that found nothing would
//     make every assertion below pass without running);
//  2. it can already read an actor off an entry type that carried one before
//     this change — the explore — so a later empty Actor is the declaration's
//     fault and not the reader's;
//  3. the collection declared over the wire names the account that made it.
//
// And the contrast that says the actor is not a constant this path always
// writes: the collection the test's own setup declares in-process goes through
// the same function with an empty Caller and comes back with an empty actor.
func TestEstablishingOverTheWireGoesIntoTheLogWithAnActor(t *testing.T) {
	server, address := running(t, false)

	// Declared from inside the process with no caller at all, exactly as a
	// program embedding the store would. This is the contrast row.
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// An explore first. It is the known-positive: this entry has carried an
	// actor since the shell existed, so if the walk below cannot find an actor
	// on THIS entry, nothing it says about the declaration means anything.
	client.send(protocol.Explore, exploring{Access: store.Access{
		Kind: "scan", Collection: "articles", Index: "by_author", Limit: 5,
	}})
	if frame := client.read(); frame.Type != protocol.Result {
		t.Fatalf("exploring: %s %s", frame.Type, frame.Payload)
	}

	mustEstablish(t, client, notesSpec())

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	entries, declarations, reads := 0, 0, 0
	readActor := ""
	var wire, inProcess *store.Change

	if err := db.Changes(0, func(change store.Change) bool {
		entries++
		switch change.Kind {
		case store.ChangeRead:
			reads++
			if readActor == "" {
				readActor = change.By.Actor
			}
		case store.ChangeDeclare:
			declarations++
			one := change
			switch change.Collection {
			case "notes":
				wire = &one
			case "articles":
				inProcess = &one
			}
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	// (1) A walk that found nothing is a broken measurement, not a database
	// with nothing in it. Everything below would pass vacuously without this.
	if entries == 0 {
		t.Fatal("the change log walk found no entries at all on a database that was just declared into and read from — this test is measuring nothing")
	}
	if declarations == 0 {
		t.Fatal("the change log holds no collection entries, so nothing below is looking at a declaration")
	}
	if reads == 0 {
		t.Fatal("the change log holds no read entries, so the known-positive below is not there to check")
	}

	// (2) The known-positive.
	if readActor != "acme (operator)" {
		t.Fatalf("the explore entry names actor %q, want %q — the reader cannot see an actor that was already there, so it cannot be trusted about the one being added",
			readActor, "acme (operator)")
	}

	// (3) The collection declared over the wire.
	if wire == nil {
		t.Fatalf("no collection entry for %q among the %d in the log", "notes", declarations)
	}
	if wire.By.Actor != "acme (operator)" {
		t.Fatalf("the collection declared over the wire is logged against actor %q, want %q — the log does not say who declared it",
			wire.By.Actor, "acme (operator)")
	}
	if wire.By.Operation != "declare" {
		t.Fatalf("the collection declared over the wire is logged under %q, want %q", wire.By.Operation, "declare")
	}

	// The contrast. Same function, a caller with no actor in it, and the entry
	// says so rather than inheriting a name from anywhere.
	if inProcess == nil {
		t.Fatalf("no collection entry for the in-process declaration among the %d in the log", declarations)
	}
	if inProcess.By.Actor != "" {
		t.Fatalf("a collection declared in-process with an empty Caller is logged against actor %q — the actor is coming from somewhere other than the caller",
			inProcess.By.Actor)
	}
}

// TestRedeclaringOverTheWireAlsoGoesIntoTheLogWithAnActor exists because
// store.Declare writes its entry from two different places — the first
// declaration and the upgrade-in-place — and a caller plumbed into one of them
// is a caller plumbed into neither, for anybody reading the log of a database
// that has been running a while.
//
// The upgrade is the arm that matters more in practice: a collection is
// declared once and redeclared on every deploy that touches it.
func TestRedeclaringOverTheWireAlsoGoesIntoTheLogWithAnActor(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	mustEstablish(t, client, notesSpec())

	// The second one is the upgrade: a new index on a collection that exists,
	// which takes store.Declare's other record() call.
	upgraded := notesSpec()
	upgraded.Indexes = append(upgraded.Indexes, store.Index{
		Name:   "by_slug",
		Fields: []store.Field{{Path: "slug", Type: store.TypeString, Missing: store.MissingSkip}},
	})
	mustEstablish(t, client, upgraded)

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Both entries for "notes", in the order they were written.
	var forNotes []store.Change
	if err := db.Changes(0, func(change store.Change) bool {
		if change.Kind == store.ChangeDeclare && change.Collection == "notes" {
			forNotes = append(forNotes, change)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	if len(forNotes) != 2 {
		t.Fatalf("the log holds %d entries for notes, want 2 — the first declaration and the upgrade", len(forNotes))
	}
	for i, change := range forNotes {
		if change.By.Actor != "acme (operator)" {
			t.Errorf("entry %d for notes is logged against actor %q, want %q", i, change.By.Actor, "acme (operator)")
		}
	}
	// And the second is the upgrade rather than a repeat of the first, which
	// is what says the second record() call is the one that ran.
	if forNotes[1].Spec == nil || len(forNotes[1].Spec.Indexes) != 2 {
		t.Fatalf("the second entry for notes carries %+v, want the upgraded spec with two indexes", forNotes[1].Spec)
	}
}
