package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// SAPE-9's wire guard.
//
// A refusal the client cannot name is a refusal the client cannot act on, and
// this repository has already paid for one: ISS-21 is the record of a sentinel
// with no row in codeFor's table arriving as `failed`. Adding a row and
// checking the row exists would be a test that reads its answer off the table
// it is guarding, so what is exercised here is a real declaration into a real
// namespace on a real database, whose refusal is then put through the same
// codeFor and failure() a connection uses.
//
// The expected code is the literal "namespace", written on this side and
// nowhere else.

// TestANamespaceRefusalReachesTheClientAsNamespaceNotFailed is the whole
// guard, both directions: the refusal carries the code, and the declaration
// that is allowed still succeeds.
func TestANamespaceRefusalReachesTheClientAsNamespaceNotFailed(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	widgetsScanOps(t, server, "acct", "namespace-db")

	db, release, err := server.Store("acct", "namespace-db")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	scan := func(name string) store.Operation {
		return store.Operation{
			Name: name, Collection: "widgets", Action: store.ActionScan,
			Index: store.ClusteredIndex,
			Input: []store.Parameter{{Name: "t", Type: store.TypeAny, Required: true}},
			From:  &store.Endpoint{Terms: []store.Term{{Arg: "t"}}},
			Limit: 10,
		}
	}

	// A bundle claims "acme": this is the Caller `sapedb install` builds, with
	// the signing key in Signer. The known-positive — without it, the refusal
	// below could be a refusal of something else entirely.
	if _, err := db.DeclareOperation(store.Caller{Actor: "an installer", Signer: "beef"}, scan("acme:widgets.scan")); err != nil {
		t.Fatalf("the first declaration into a free namespace was refused: %v", err)
	}

	// And now the connection's own operator, who is not that key.
	_, err = db.DeclareOperation(store.Caller{Actor: "acct (operator)"}, scan("acme:widgets.scan"))
	if err == nil {
		t.Fatal("declaring into a namespace held by a signing key was not refused")
	}
	if !errors.Is(err, store.ErrNamespace) {
		t.Errorf("errors.Is(err, store.ErrNamespace) is false for %v", err)
	}
	// errors.Is alone would still pass with no row in codeFor's table, which
	// is exactly the gap ISS-21 records — so the byte a client switches on is
	// checked separately.
	if got := codeFor(err); got != "namespace" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "namespace")
	}

	// All the way out to the bytes on the wire, because codeFor is not what a
	// client reads; failure()'s JSON is.
	body := struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}{}
	if err := json.Unmarshal(failure(err), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "namespace" {
		t.Errorf("the failure frame carries code %q, want %q", body.Code, "namespace")
	}
	if !strings.Contains(body.Message, "acme") {
		t.Errorf("the failure message does not name the namespace: %q", body.Message)
	}

	// The declaration the operator IS allowed to make still works: a
	// one-directional guard that only ever refuses could never go red for the
	// bug where everything is refused.
	if _, err := db.DeclareOperation(store.Caller{Actor: "acct (operator)"}, scan("widgets.scan_flat")); err != nil {
		t.Errorf("a flat name was refused after a namespace was claimed: %v", err)
	}
	if _, err := db.DeclareOperation(store.Caller{Actor: "acct (operator)"}, scan("acct:widgets.scan")); err != nil {
		t.Errorf("the operator was refused a namespace nobody holds: %v", err)
	}
}

// TestANamespaceRefusalIsNotTheDeclarationCode says out loud which of the
// neighbouring codes this one is not.
//
// "declaration" is the code for an operation that does not make sense, and it
// is the code a namespace refusal would most plausibly have been folded into.
// They are different instructions to whoever reads them: one says fix your
// declaration, the other says this name is not yours.
func TestANamespaceRefusalIsNotTheDeclarationCode(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	widgetsScanOps(t, server, "acct", "namespace-codes")

	db, release, err := server.Store("acct", "namespace-codes")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	good := store.Operation{
		Name: "acme:widgets.one", Collection: "widgets", Action: store.ActionScan,
		Index: store.ClusteredIndex,
		Input: []store.Parameter{{Name: "t", Type: store.TypeAny, Required: true}},
		From:  &store.Endpoint{Terms: []store.Term{{Arg: "t"}}},
		Limit: 10,
	}
	if _, err := db.DeclareOperation(store.Caller{Actor: "vendor-a"}, good); err != nil {
		t.Fatalf("the holding declaration was refused: %v", err)
	}

	taken := good
	taken.Name = "acme:widgets.two"
	_, err = db.DeclareOperation(store.Caller{Actor: "vendor-b"}, taken)
	if err == nil {
		t.Fatal("declaring into another actor's namespace was not refused")
	}
	if errors.Is(err, store.ErrDeclaration) {
		t.Errorf("a namespace refusal is also ErrDeclaration, so it would reach a client as %q: %v", "declaration", err)
	}
	if got := codeFor(err); got != "namespace" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "namespace")
	}

	// The control: an operation that really is malformed still reaches the
	// client as "declaration", so the row added above did not swallow it.
	broken := good
	broken.Name = "acme:widgets.three"
	broken.Limit = 0
	_, err = db.DeclareOperation(store.Caller{Actor: "vendor-a"}, broken)
	if err == nil {
		t.Fatal("a scan with no limit was accepted, so the control row measures nothing")
	}
	if got := codeFor(err); got != "declaration" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "declaration")
	}
}
