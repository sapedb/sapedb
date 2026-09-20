package store

import (
	"reflect"
	"sort"
	"testing"
)

// TestEnvelopeOfAPlainScanReadsItsLimitIndexAndProjection is SAPE-8's first
// required case: a flat scan's cost envelope, read without running it.
//
// The number a caller acts on is the limit — could they be handed more rows
// than they can afford to hold — and the exact field list, since a
// projection that leaks one field too many is a promise the declaration
// broke. Both are asserted by value, not by presence.
func TestEnvelopeOfAPlainScanReadsItsLimitIndexAndProjection(t *testing.T) {
	_, s := fresh(t, 8001)

	if _, err := s.Declare(Caller{}, Spec{
		Name: "books",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name:   "by_author",
			Fields: []Field{{Path: "author", Type: TypeString, Missing: MissingSkip}},
		}},
	}); err != nil {
		t.Fatalf("declare books: %v", err)
	}

	declared, err := s.DeclareOperation(Caller{}, Operation{
		Name: "books.by_author", Collection: "books", Action: ActionScan,
		Index:      "by_author",
		Limit:      25,
		Projection: []string{"title"},
	})
	if err != nil {
		t.Fatalf("declare books.by_author: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	envelope, err := s.Envelope("books.by_author", declared.Version)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	if envelope.Limit != 25 {
		t.Errorf("limit is %d, want 25 — this is the number a caller decides against", envelope.Limit)
	}
	if !reflect.DeepEqual(envelope.Collections, []string{"books"}) {
		t.Errorf("collections are %v, want [books]", envelope.Collections)
	}
	if !reflect.DeepEqual(envelope.Indexes, []string{"by_author"}) {
		t.Errorf("indexes are %v, want [by_author]", envelope.Indexes)
	}
	if !reflect.DeepEqual(envelope.Projection, []string{"title"}) {
		t.Errorf("projection is %v, want [title]", envelope.Projection)
	}
	if envelope.WholeDocument {
		t.Error("wholeDocument is true, but this scan declares a narrower projection")
	}
}

// TestEnvelopeOfACountReadsHowFarItWalksAndLeavesNothingEscaping is SAPE-8's
// second required case. A count's declared Limit is how far it walks, not
// how many rows return — it returns none — so the envelope's Limit is
// asserted against that meaning, and Projection/WholeDocument are asserted
// absent rather than merely unequal to some other value.
func TestEnvelopeOfACountReadsHowFarItWalksAndLeavesNothingEscaping(t *testing.T) {
	_, s := fresh(t, 8002)

	if _, err := s.Declare(Caller{}, Spec{
		Name: "books",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name:   "by_author",
			Fields: []Field{{Path: "author", Type: TypeString, Missing: MissingSkip}},
		}},
	}); err != nil {
		t.Fatalf("declare books: %v", err)
	}

	declared, err := s.DeclareOperation(Caller{}, Operation{
		Name: "books.count_by_author", Collection: "books", Action: ActionCount,
		Index: "by_author",
		Limit: 500,
	})
	if err != nil {
		t.Fatalf("declare books.count_by_author: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	envelope, err := s.Envelope("books.count_by_author", declared.Version)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	if envelope.Limit != 500 {
		t.Errorf("limit is %d, want 500 — how far this count may walk", envelope.Limit)
	}
	if !reflect.DeepEqual(envelope.Collections, []string{"books"}) {
		t.Errorf("collections are %v, want [books]", envelope.Collections)
	}
	if !reflect.DeepEqual(envelope.Indexes, []string{"by_author"}) {
		t.Errorf("indexes are %v, want [by_author]", envelope.Indexes)
	}
	if envelope.Projection != nil {
		t.Errorf("projection is %v, want none — a count hands back a number, not fields", envelope.Projection)
	}
	if envelope.WholeDocument {
		t.Error("wholeDocument is true, but a count returns no document at all")
	}
}

// TestEnvelopeOfAComposedOperationReadsTheCeilingWithoutWalkingItsStepsByHand
// is SAPE-8's third required case, and the one the ticket names directly:
// SAPE-20 fixed the rule that a composed operation's ceiling at any depth is
// the number it declares, and this asserts that number is reachable from the
// catalogue rather than something a caller has to re-derive by opening every
// operation a batch's steps name.
//
// It reuses compose_test.go's own fixture (composed, page) rather than a
// fresh one: that fixture is what SAPE-20's own ceiling test measures
// against, so an envelope that disagreed with it would be disagreeing with
// an already-measured fact, not a new one.
func TestEnvelopeOfAComposedOperationReadsTheCeilingWithoutWalkingItsStepsByHand(t *testing.T) {
	s := composed(t)

	declared, err := s.DeclareOperation(Caller{}, page())
	if err != nil {
		t.Fatalf("declare catalog.page: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	envelope, err := s.Envelope("catalog.page", declared.Version)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	// items.on_shelf (limit 50) + items.total_on_shelf (limit 1) = 51, the
	// same number catalog.page declares — read here without this test
	// having opened either callee itself.
	if envelope.Limit != 51 {
		t.Errorf("limit is %d, want 51 — the sum of the steps' own ceilings, which is what catalog.page declares", envelope.Limit)
	}
	if !reflect.DeepEqual(envelope.Collections, []string{"items"}) {
		t.Errorf("collections are %v, want [items] — reached through the steps, not from catalog.page's own Collection field", envelope.Collections)
	}
	if !reflect.DeepEqual(envelope.Indexes, []string{"by_shelf"}) {
		t.Errorf("indexes are %v, want [by_shelf] — items.on_shelf's index, read through the step that calls it", envelope.Indexes)
	}
	// items.on_shelf declares no projection, so the worst case — the whole
	// document escaping through that step — is what the envelope must say,
	// even though items.total_on_shelf's own answer is a synthetic row.
	if !envelope.WholeDocument {
		t.Error("wholeDocument is false, but items.on_shelf (called with no projection) hands back full documents")
	}
}

// TestEnvelopeOfAnOperationWithScopesReadsThemWithoutWalking is SAPE-8's
// fourth required case. Scopes needed no new plumbing — validateComposedStep
// already requires a composed operation's own Scopes to be the union of
// whatever it calls — so this asserts the envelope surfaces exactly the
// scopes a caller must present, copied straight off the declaration.
func TestEnvelopeOfAnOperationWithScopesReadsThemWithoutWalking(t *testing.T) {
	_, s := fresh(t, 8004)

	if _, err := s.Declare(Caller{}, Spec{
		Name: "accounts",
		Key:  Key{Path: "id", Type: TypeString},
	}); err != nil {
		t.Fatalf("declare accounts: %v", err)
	}

	declared, err := s.DeclareOperation(Caller{}, Operation{
		Name: "accounts.get", Collection: "accounts", Action: ActionGet,
		Input:  []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:    &Term{Arg: "id"},
		Scopes: []string{"admin", "accounts:read"},
	})
	if err != nil {
		t.Fatalf("declare accounts.get: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	envelope, err := s.Envelope("accounts.get", declared.Version)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	got := append([]string{}, envelope.Scopes...)
	sort.Strings(got)
	want := []string{"accounts:read", "admin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scopes are %v, want %v — what a caller must hold to run this at all", got, want)
	}
	if envelope.Limit != 1 {
		t.Errorf("limit is %d, want 1 — a get touches exactly one document", envelope.Limit)
	}
	if !envelope.WholeDocument {
		t.Error("wholeDocument is false, but this get declares no projection")
	}
}

// TestWhatIsHereCarriesAnEnvelopePerOperation checks that the envelope is
// reachable exactly the way the ticket asks: from the catalogue every client
// can already call, with no second round trip and no walk of Steps by hand.
func TestWhatIsHereCarriesAnEnvelopePerOperation(t *testing.T) {
	s := composed(t)
	if _, err := s.DeclareOperation(Caller{}, page()); err != nil {
		t.Fatalf("declare catalog.page: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	here, err := s.WhatIsHere(Caller{Actor: "ops@acme"})
	if err != nil {
		t.Fatal(err)
	}

	if len(here.Envelopes) != len(here.Operations) {
		t.Fatalf("%d envelopes for %d operations — one per operation is the whole point", len(here.Envelopes), len(here.Operations))
	}

	byName := map[string]Envelope{}
	for _, envelope := range here.Envelopes {
		byName[envelope.Operation] = envelope
	}
	page, found := byName["catalog.page"]
	if !found {
		t.Fatal("catalog.page has no envelope in the catalogue")
	}
	if page.Limit != 51 {
		t.Errorf("catalog.page's envelope from WhatIsHere says limit %d, want 51", page.Limit)
	}
}
