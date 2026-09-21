package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// SAPE-11 asks for a listing of "the external operations installed on a
// server", with each one's envelope beside it. Measuring what the store
// already models (internal/store/envelope.go, internal/bundle/bundle.go, and
// every path that reaches store.DeclareOperation) turns up no signer identity
// anywhere: store.Operation carries none, store.Caller carries none, and
// nothing in this tree calls bundle.Verify or bundle.Check outside
// internal/bundle's own tests — there is no install path yet, so there is no
// fact recorded anywhere that would tell an operator-typed operation apart
// from one a signed bundle would have installed. "External" is therefore a
// word for something this store does not model in v1: what exists today,
// reachable without running anything, is Catalogue plus its per-operation
// Envelope — exactly what SAPE-8 already computes — and these tests are
// about the one thing that was still unmeasured: whether that pair actually
// reaches a caller over the wire the same way Explore does, with the same
// values SAPE-8 reports.
//
// explore.go's own package doc, at the top, says exactly the same thing about
// the operator gate: WhatIsHere goes through the identical `if !live.operator`
// check Explore does, in the same handler, so proving the gate once for
// Explore already proves it for the catalogue. That existing coverage
// (TestExploringNeedsMoreThanAConnectionString) is not duplicated blow for
// blow here; TestTheCatalogueOverTheWireStillNeedsTheOperatorProof only
// checks the one thing that test does not: that a refused catalogue request
// carries no operation names.

func (c *client) catalogue() protocol.Frame {
	c.t.Helper()
	c.send(protocol.Explore, exploring{Catalogue: true})
	return c.read()
}

// TestTheCatalogueOverTheWireCarriesTheSameEnvelopeSAPE8Reports is SAPE-11's
// acceptance criterion 4: "The envelope in the listing is the same envelope
// SAPE-8 reports for the same operation — compared as values, so the two
// paths cannot drift apart." One path is store.Store.Envelope, called
// directly, in-process. The other is a real client, over a real socket,
// asking for the catalogue the same way `sapedb shell`'s `ls` does. Nothing
// before this test compared the two: internal/store's own envelope tests
// never leave that package, and every existing wire-level explore test in
// this file asks for rows, never for `"catalogue": true`.
//
// declare (server_test.go) already gives a scan with a limit
// (articles.by_author) and a scan that carries scopes (articles.secret,
// Scopes: ["articles:read"]) — the two SAPE-8 shapes this test needs, without
// a new fixture.
func TestTheCatalogueOverTheWireCarriesTheSameEnvelopeSAPE8Reports(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	frame := client.catalogue()
	if frame.Type != protocol.Result {
		t.Fatalf("asking for the catalogue: %s %s", frame.Type, frame.Payload)
	}
	answer := decode[explored](t, frame)
	if answer.Here == nil {
		t.Fatal("the catalogue came back nil")
	}
	if len(answer.Here.Envelopes) != len(answer.Here.Operations) {
		t.Fatalf("%d envelopes for %d operations", len(answer.Here.Envelopes), len(answer.Here.Operations))
	}

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	checked := map[string]bool{}
	for _, envelope := range answer.Here.Envelopes {
		direct, err := db.Envelope(envelope.Operation, envelope.Version)
		if err != nil {
			t.Fatalf("store.Envelope(%q, %d): %v", envelope.Operation, envelope.Version, err)
		}
		if !reflect.DeepEqual(envelope, direct) {
			t.Errorf("%q over the wire is %+v, store.Envelope says %+v", envelope.Operation, envelope, direct)
		}
		checked[envelope.Operation] = true
	}
	for _, name := range []string{"articles.add", "articles.by_author", "articles.secret"} {
		if !checked[name] {
			t.Errorf("the catalogue never carried an envelope for %q", name)
		}
	}

	// The one entry with scopes is worth a value assertion of its own, not
	// only the reflect.DeepEqual above: a bug that dropped Scopes on both
	// sides identically (say, a shared zero-value struct) would pass that
	// comparison while still failing the reader who needs to know what to
	// hold before calling this operation.
	byName := map[string]store.Envelope{}
	for _, envelope := range answer.Here.Envelopes {
		byName[envelope.Operation] = envelope
	}
	secret := byName["articles.secret"]
	if len(secret.Scopes) != 1 || secret.Scopes[0] != "articles:read" {
		t.Errorf("articles.secret's envelope over the wire carries scopes %v, want [articles:read]", secret.Scopes)
	}
	if secret.Limit != 10 {
		t.Errorf("articles.secret's envelope over the wire says limit %d, want 10", secret.Limit)
	}
}

// TestTheCatalogueOverTheWireStillNeedsTheOperatorProof is SAPE-11's
// acceptance criterion 5, the half TestExploringNeedsMoreThanAConnectionString
// does not already cover: a catalogue request refused for the same reason
// must not leak an operation name in the process of being refused.
func TestTheCatalogueOverTheWireStillNeedsTheOperatorProof(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")

	frame := client.catalogue()
	if frame.Type != protocol.Failure {
		t.Fatalf("an ordinary connection read the catalogue: %s %s", frame.Type, frame.Payload)
	}
	code := decode[struct {
		Code string `json:"code"`
	}](t, frame).Code
	if code != "not_operator" {
		t.Errorf("refused with code %q, want not_operator", code)
	}
	if strings.Contains(string(frame.Payload), "articles") {
		t.Errorf("the refusal named an operation: %s", frame.Payload)
	}
}

// TestAnEmptyDatabasesCatalogueIsAnEmptyListOverTheWire is SAPE-11's
// acceptance criterion 6, measured at the layer that actually matters for a
// caller: not that store.WhatIsHere returns non-nil slices in-process (already
// covered in internal/store), but that JSON marshalling on the way out over
// the wire does not turn that into `null` — which encoding/json would do
// silently if Catalogue's slices were ever nil instead of empty.
func TestAnEmptyDatabasesCatalogueIsAnEmptyListOverTheWire(t *testing.T) {
	server, address := running(t, false)

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	release()

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	frame := client.catalogue()
	if frame.Type != protocol.Result {
		t.Fatalf("asking for the catalogue: %s %s", frame.Type, frame.Payload)
	}
	if strings.Contains(string(frame.Payload), "null") {
		t.Errorf("an empty catalogue marshalled with a null in it: %s", frame.Payload)
	}
	answer := decode[explored](t, frame)
	if answer.Here == nil {
		t.Fatal("the catalogue came back nil")
	}
	if len(answer.Here.Operations) != 0 || len(answer.Here.Envelopes) != 0 || len(answer.Here.Collections) != 0 {
		t.Errorf("a database with nothing declared answered %+v", answer.Here)
	}
}
