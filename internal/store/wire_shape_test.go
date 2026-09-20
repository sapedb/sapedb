package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCatalogueCollectionsIsNeverNull: a database nobody has declared
// anything in yet is the first one every new user's client sees. Its
// Collections must marshal as `[]`, not `null` — a caller reaching straight
// for `.map()`/`for...of`, the ordinary way to use an array, must not break
// on exactly that database.
func TestCatalogueCollectionsIsNeverNull(t *testing.T) {
	_, db := fresh(t, 401)

	here, err := db.WhatIsHere(Caller{Actor: "ops@acme"})
	if err != nil {
		t.Fatal(err)
	}
	if here.Collections == nil {
		t.Fatal("Collections came back nil before it was even marshaled")
	}

	encoded, err := json.Marshal(here)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"collections":[]`) {
		t.Fatalf("an empty catalogue marshaled as %s, want \"collections\":[]", encoded)
	}
}

// TestBoundFieldNamesAreLowerCase: the CLI's own embedded client
// (internal/wire) and the TypeScript client both build an Access whose
// From/To are a Bound and send it as JSON. The TypeScript client writes
// lower-case `values`/`exclusive` (see CollectionSpec's sibling type Bound in
// ecosy-sapedb's src/client/index.ts). Without a json tag, Go's own zero
// value for these fields marshals as `Values`/`Exclusive` — a different
// spelling that the server has so far only accepted by accident, through
// encoding/json's case-insensitive fallback on read. This test pins the
// spelling both writers must now agree on, not the fallback that used to
// paper over their disagreement.
func TestBoundFieldNamesAreLowerCase(t *testing.T) {
	encoded, err := json.Marshal(Bound{Values: []any{"history", 1.0}, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"values":["history",1],"exclusive":true}`; got != want {
		t.Fatalf("Bound marshaled as %s, want %s", got, want)
	}

	// Exclusive is the ordinary case (an inclusive, i.e. non-exclusive,
	// bound) and drops out of the wire entirely rather than writing `false`
	// on every scan boundary — matching every other bool this package puts
	// on the wire (e.g. Index.Unique).
	encoded, err = json.Marshal(Bound{Values: []any{"history"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"values":["history"]}`; got != want {
		t.Fatalf("an inclusive Bound marshaled as %s, want %s", got, want)
	}
}

// TestSpecIndexesIsNeverNull records the policy decision for task 0068 §1
// (Phase A): Spec.Indexes follows the same "never null" rule as
// Catalogue.Collections, applied consistently across the whole surface that
// is about to leave internal/. A collection declared with no indexes at all
// used to read back with Indexes nil — Declare never allocates one — and
// Indexes carries no `omitempty`, so it marshaled as `null`. That is exactly
// the shape TestCatalogueCollectionsIsNeverNull above pins one level up, and
// it is reachable the same way: a brand new collection, read back by the
// first caller who asks what it looks like.
//
// Fixed at Collection.Spec() (collection.go), not at Declare/install/
// writeSpec — the disk write path is left alone on purpose, the same way
// NextIndexID/NextRollupID's tags are left alone below: only the copy handed
// to a caller is normalized, not what gets written.
func TestSpecIndexesIsNeverNull(t *testing.T) {
	_, db := fresh(t, 402)

	collection, err := db.Declare(Spec{
		Name: "empty",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
	})
	if err != nil {
		t.Fatal(err)
	}

	spec := collection.Spec()
	if spec.Indexes == nil {
		t.Fatal("Indexes came back nil before it was even marshaled")
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"indexes":[]`) {
		t.Fatalf("a Spec with no indexes marshaled as %s, want \"indexes\":[]", encoded)
	}

	// The same shape has to survive one level up too: an index-less
	// collection sitting inside the Catalogue that WhatIsHere hands back,
	// which is what a real caller actually reads.
	here, err := db.WhatIsHere(Caller{Actor: "ops@acme"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(here)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"indexes":[]`) {
		t.Fatalf("a Catalogue holding an index-less collection marshaled as %s, want \"indexes\":[] in it somewhere", encoded)
	}
}

// TestEndpointTermsIsNeverNull: a hand-authored schema.json can declare a
// scan endpoint with no lower or upper constraint by simply omitting
// "terms" — `{"exclusive": true}` is a legal Endpoint, meaning "bounded at
// this end by nothing". Explore's own asOperation never produces this shape
// (endpoint() in explore.go always builds Terms with make()), but
// DeclareOperation decodes an Operation straight from a client-authored
// file, and Endpoint.Terms carries no `omitempty` — so, unnormalized, that
// Endpoint reads back with Terms nil and marshals as `"terms":null`. The
// TypeScript mirror of Endpoint documents Terms as "never absent, so never
// worth an ? here" and reaches for `.map()` on the strength of that promise.
//
// Fixed in validateOperation (ops.go), which both DeclareOperation and
// Explore call before anything is marshaled or stored — so this reaches the
// operation actually written to disk, not just the copy handed back in
// memory.
func TestEndpointTermsIsNeverNull(t *testing.T) {
	_, db := fresh(t, 403)
	if _, err := db.Declare(articles()); err != nil {
		t.Fatal(err)
	}

	stored, err := db.DeclareOperation(Operation{
		Name:       "articles.by_author.scan",
		Collection: "articles",
		Action:     ActionScan,
		Index:      "by_author",
		From:       &Endpoint{Exclusive: true}, // no Terms at all
		Limit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.From == nil || stored.From.Terms == nil {
		t.Fatalf("From.Terms came back nil before it was even marshaled: %+v", stored.From)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"terms":[]`) {
		t.Fatalf("a declared Operation with an unbounded end marshaled as %s, want \"terms\":[] in it somewhere", encoded)
	}

	// Reading it back the way a caller actually would — through Operations(),
	// which both WhatIsHere's Catalogue and Explored.Draft are answered
	// from — must show the same normalized shape, not just the value
	// DeclareOperation happened to hand back in the same call.
	operations, err := db.Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].From == nil || operations[0].From.Terms == nil {
		t.Fatalf("Operations() read back From.Terms as nil: %+v", operations)
	}
}

// TestSpecCounterTagsStayUnderscored records the decision not to "fix"
// NextIndexID/NextRollupID into camelCase like every other multi-word field
// this package puts on the wire: Spec is round-tripped to the on-disk
// catalogue by json.Marshal/Unmarshal (see writeSpec/load), and a database
// declared by an earlier server already has these two keys spelled with an
// underscore. Renaming the tag would not touch what is on disk already:
// json.Unmarshal silently leaves an unmatched field at its zero value, so
// the counter would read back as 0 and the next Declare would hand out
// index/rollup ids already in use by that collection's existing entries.
//
// This test is the regression a future "make it consistent" cleanup would
// need to see turn red before it could ship. See the comment on the field
// itself in spec.go for the full reasoning.
func TestSpecCounterTagsStayUnderscored(t *testing.T) {
	encoded, err := json.Marshal(Spec{
		Name:         "books",
		NextIndexID:  5,
		NextRollupID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"next_index_id":5`) {
		t.Fatalf("Spec marshaled as %s, want \"next_index_id\":5", encoded)
	}
	if !strings.Contains(string(encoded), `"next_rollup_id":7`) {
		t.Fatalf("Spec marshaled as %s, want \"next_rollup_id\":7", encoded)
	}

	// The part that actually breaks if the tag changes: reading a Spec
	// written to disk by a version of the server that still spells these
	// keys with an underscore. If the struct tag were renamed to
	// "nextIndexId"/"nextRollupId", this would unmarshal as 0/0, and the
	// counters of every collection already declared would silently reset.
	stored := []byte(`{"name":"books","next_index_id":5,"next_rollup_id":7}`)
	restored := Spec{}
	if err := json.Unmarshal(stored, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.NextIndexID != 5 || restored.NextRollupID != 7 {
		t.Fatalf("a Spec written by an earlier server read back as %+v, want NextIndexID=5, NextRollupID=7", restored)
	}
}
