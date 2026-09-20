package server

import (
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestDeclaringOverTheWireGoesIntoTheLogWithAnActor is the audit hole the
// Declare frame opened.
//
// Every other entry in the change log carries an Attribution. A write names
// the operation that made it and the actor it ran for; a read through the
// shell names "explore" and the operator who typed it. A declaration named
// nobody — store.DeclareOperation recorded `Change{Kind: ChangeOperation, ...}`
// and stopped there.
//
// That was survivable while declaring meant `sapedb apply`, which needs the
// database file's exclusive lock, which means the server is stopped and
// whoever did it was standing at the machine. The Declare frame ended that:
// an operation can be declared from another host, over a live connection,
// against a running server — and the entry with nobody's name on it was the
// one recording the most consequential change there is, the one that decides
// what everybody else may do.
//
// Three things are measured, and the order is the point:
//
//  1. the log reader finds anything at all (it is pointed at a database with
//     a known number of entries in it, and zero is a failure rather than a
//     clean sheet);
//  2. it can already read an actor off an entry type that carried one before
//     this change — the explore below — so a later empty Actor is the
//     declaration's fault and not the reader's;
//  3. the declaration made over the wire names the account that made it.
//
// And the contrast that says the actor is not simply a constant this code
// path always writes: the operations declared in-process by the test's own
// setup go through the same function with an empty Caller and come back with
// an empty actor.
func TestDeclaringOverTheWireGoesIntoTheLogWithAnActor(t *testing.T) {
	server, address := running(t, false)

	// Declared from inside the process with no caller at all, exactly as a
	// program embedding the store would. These are the contrast rows.
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

	overTheWire := store.Operation{
		Name: "articles.declared_over_the_wire", Collection: "articles",
		Action: store.ActionScan, Index: "by_author", Limit: 10,
	}
	if frame := client.declare(overTheWire); frame.Type != protocol.Result {
		t.Fatalf("declaring over the wire: %s", frame.Payload)
	}

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	entries := 0
	declarations := 0
	reads := 0
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
		case store.ChangeOperation:
			declarations++
			one := change
			if change.Operation != nil && change.Operation.Name == overTheWire.Name {
				wire = &one
			}
			if change.Operation != nil && change.Operation.Name == "articles.by_author" {
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
		t.Fatal("the change log holds no operation entries, so nothing below is looking at a declaration")
	}
	if reads == 0 {
		t.Fatal("the change log holds no read entries, so the known-positive below is not there to check")
	}

	// (2) The known-positive. An explore has carried an actor since there was
	// a shell; if this is empty the reader is at fault and the declaration's
	// actor proves nothing either way.
	if readActor != "acme (operator)" {
		t.Fatalf("the explore entry names actor %q, want %q — the reader cannot see an actor that was already there, so it cannot be trusted about the one being added",
			readActor, "acme (operator)")
	}

	// (3) The declaration made over the wire.
	if wire == nil {
		t.Fatalf("no operation entry for %q among the %d in the log", overTheWire.Name, declarations)
	}
	if wire.By.Actor != "acme (operator)" {
		t.Fatalf("the operation declared over the wire is logged against actor %q, want %q — the log does not say who declared it",
			wire.By.Actor, "acme (operator)")
	}
	if wire.By.Operation != "declare" {
		t.Fatalf("the operation declared over the wire is logged under %q, want %q", wire.By.Operation, "declare")
	}

	// The contrast. Same function, a caller with no actor in it, and the
	// entry says so rather than inheriting a name from anywhere.
	if inProcess == nil {
		t.Fatalf("no operation entry for the in-process declaration among the %d in the log", declarations)
	}
	if inProcess.By.Actor != "" {
		t.Fatalf("an operation declared in-process with an empty Caller is logged against actor %q — the actor is coming from somewhere other than the caller",
			inProcess.By.Actor)
	}
}
