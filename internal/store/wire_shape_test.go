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
