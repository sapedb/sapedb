package server

import (
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAnIncompatibleEstablishReachesTheWireAsIncompatible is ISS-21's
// measurement.
//
// store.ErrIncompatible is the refusal store.Declare gives when a
// redeclaration cannot be applied in place — moving a primary key, changing
// how a collection is partitioned, redefining a rollup or an index under a
// name that already names something else. Before this task, codeFor's table
// had no entry for it: grep -c ErrIncompatible internal/server/server.go
// returned 0 against a table that (measured the same way) maps 22 other
// sentinels. Every one of those refusals reached a client as the wire code
// "failed" — indistinguishable from a disk error or a closed database.
//
// That only started mattering with the Establish frame (SAPE-14): before it,
// ErrIncompatible could only be raised offline inside `sapedb apply`, where a
// human reads the sentence rather than a program switching on a code.
//
// This test does not call codeFor directly — a table test that only reads the
// table agrees with itself. It sends the frame that actually produces
// ErrIncompatible inside store.Declare (moving the key of a collection that
// is already established) down a real connection, and reads back the code a
// client would actually see.
func TestAnIncompatibleEstablishReachesTheWireAsIncompatible(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	mustEstablish(t, client, notesSpec())

	// The same shape TestEstablishingAnExistingCollectionUpgradesItInPlace
	// uses to prove the message: moving the primary key of a collection that
	// is already declared cannot be done in place.
	moved := notesSpec()
	moved.Key.Path = "key"

	frame := client.establish(moved)
	if frame.Type != protocol.Failure {
		t.Fatalf("moving the primary key of an established collection was accepted: %s %s", frame.Type, frame.Payload)
	}

	got := decode[struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}](t, frame)

	if got.Code != "incompatible" {
		t.Errorf("codeFor sent %q over the wire, want %q (message was %q)", got.Code, "incompatible", got.Message)
	}
}

// TestDeclareFrameDoesNotReachErrIncompatible documents, rather than merely
// asserts, why ISS-21's fix does not also need a wire test through the
// Declare frame (13).
//
// Establish (14) calls store.Declare, which is where ErrIncompatible is
// raised. Declare (13) calls store.DeclareOperation instead — a different
// function, for a different kind of declaration (an operation, not a
// collection) — and DeclareOperation's own validation path is
// validateOperation, which only ever wraps store.ErrDeclaration (already
// mapped to "declaration" before this task). There is no redeclaration
// concept for an operation to make incompatible: declaring an existing
// operation name writes a NEW version and leaves the old one runnable,
// exactly the versioning store.Declare cannot do for a collection. So the two
// frames do not share a route to this error, and this test measures that
// directly: declaring an operation under a name that already exists must
// succeed with a new version, never with ErrIncompatible.
func TestDeclareFrameDoesNotReachErrIncompatible(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	mustEstablish(t, client, notesSpec())

	first := store.Operation{
		Name: "notes.by_author", Collection: "notes", Action: store.ActionScan,
		Index: "by_author", Limit: 10,
	}
	frame := client.declare(first)
	if frame.Type != protocol.Result {
		t.Fatalf("declaring %q the first time: %s %s", first.Name, frame.Type, frame.Payload)
	}
	firstVersion := decode[declared](t, frame).Operation.Version

	// Redeclaring the same name is exactly the shape a collection redeclare
	// takes when it hits ErrIncompatible — an existing name, declared again,
	// differently (a different Limit). For an operation this is not a
	// conflict: it is a new version.
	second := first
	second.Limit = 5
	frame = client.declare(second)
	if frame.Type != protocol.Result {
		t.Fatalf("redeclaring %q: %s %s", second.Name, frame.Type, frame.Payload)
	}
	secondVersion := decode[declared](t, frame).Operation.Version
	if secondVersion != firstVersion+1 {
		t.Fatalf("redeclaring %q gave version %d after version %d, want %d",
			second.Name, secondVersion, firstVersion, firstVersion+1)
	}
}
