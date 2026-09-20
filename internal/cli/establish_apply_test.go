package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// This file is to the Establish frame what declare_apply_test.go is to
// Declare: the measurement behind the sentence in internal/server's
// establish.go that the frame runs the same checks `sapedb apply` runs.
//
// A second way into store.Declare is a second place a rule can quietly not
// apply, and the way that happens is never a decision — it is a handler that
// normalises a field on the way in, or fills a missing one, or checks the
// cheap half of a declaration and calls the store for the rest. So the claim
// is not made in a doc comment and checked by reading the code. One table of
// declarations goes down both paths, against ONE database, and the refusals
// have to be the store's own sentence on both.
//
// It lives here for the reason the operation table does: apply() is
// unexported, so this is the only package that can drive the real command
// rather than a re-implementation of it.
//
// The shape of the table matters as much as its rows. It holds refusals AND
// three declarations both paths must accept, so a bug that refused everything
// — a handler that never reads the payload, a database it cannot reach, an
// operator gate left shut — shows up as a failed control rather than as a
// table of refusals that all agree with each other for the wrong reason.

// TestEstablishingOverTheWireRefusesExactlyWhatApplyRefuses runs one table of
// collection declarations down both paths and demands the same answer, word
// for word.
func TestEstablishingOverTheWireRefusesExactlyWhatApplyRefuses(t *testing.T) {
	// notes is the collection pairedServer already declared, redeclared
	// field for field. Rows that must collide with something already stored
	// start from it.
	notes := func() store.Spec {
		return store.Spec{
			Name: "notes",
			Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
			Indexes: []store.Index{{
				Name:   "by_author",
				Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
			}},
		}
	}

	table := []struct {
		name string
		spec store.Spec
		// refused is the store's own words, or "" for a declaration both
		// paths must accept.
		refused string
	}{
		{
			name:    "a collection with no name",
			spec:    store.Spec{Name: "", Key: store.Key{Path: "id", Type: store.TypeString}},
			refused: "collection name: sapedb/store: that is not a usable name: it is empty",
		},
		{
			name:    "a name with a zero byte in it",
			spec:    store.Spec{Name: "no\x00tes", Key: store.Key{Path: "id", Type: store.TypeString}},
			refused: "collection name: sapedb/store: that is not a usable name: it holds a zero byte",
		},
		{
			name:    "a nested primary key",
			spec:    store.Spec{Name: "nested", Key: store.Key{Path: "meta.id", Type: store.TypeString}},
			refused: `sapedb/store: the declaration does not make sense: the primary key must be one field of the document, not "meta.id"`,
		},
		{
			name:    "a primary key with no type",
			spec:    store.Spec{Name: "untyped", Key: store.Key{Path: "id"}},
			refused: `sapedb/store: the declaration does not make sense: a primary key is a string or a number, not ""`,
		},
		{
			name:    "a generated ULID on a number key",
			spec:    store.Spec{Name: "generated", Key: store.Key{Path: "id", Type: store.TypeNumber, Auto: "ulid"}},
			refused: `sapedb/store: the declaration does not make sense: a generated ULID is a string, but the key is declared "number"`,
		},
		{
			name:    "a key the store has no way to generate",
			spec:    store.Spec{Name: "uuid_keyed", Key: store.Key{Path: "id", Type: store.TypeString, Auto: "uuid"}},
			refused: `sapedb/store: the declaration does not make sense: "uuid" is not something the store knows how to generate`,
		},
		{
			name: "an index with no name",
			spec: store.Spec{
				Name: "unnamed_index", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{
					Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
				}},
			},
			refused: "index name: sapedb/store: that is not a usable name: it is empty",
		},
		{
			name: "an index with no fields",
			spec: store.Spec{
				Name: "fieldless", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{Name: "by_author"}},
			},
			refused: `sapedb/store: the declaration does not make sense: index "by_author" has no fields`,
		},
		{
			name: "a field of a type nobody declares",
			spec: store.Spec{
				Name: "dated", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{
					Name:   "by_made",
					Fields: []store.Field{{Path: "made", Type: "date", Missing: store.MissingSkip}},
				}},
			},
			refused: `sapedb/store: the declaration does not make sense: index "by_made" declares "made" as "date"`,
		},
		{
			// The rule that has no default on purpose: what happens to a
			// document with no value for the field decides which documents a
			// range query returns, so leaving it unsaid is not a detail.
			name: "an index that does not say what happens to a document without the field",
			spec: store.Spec{
				Name: "unsaid", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{
					Name:   "by_author",
					Fields: []store.Field{{Path: "author", Type: store.TypeString}},
				}},
			},
			refused: `sapedb/store: the declaration does not make sense: index "by_author" must say what happens to documents without "author" (skip, first or last)`,
		},
		{
			name: "two indexes of one name",
			spec: store.Spec{
				Name: "twice_named", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{
					{Name: "by_author", Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}}},
					{Name: "by_author", Fields: []store.Field{{Path: "slug", Type: store.TypeString, Missing: store.MissingSkip}}},
				},
			},
			refused: `sapedb/store: the declaration does not make sense: two indexes are called "by_author"`,
		},
		{
			name: "one field named twice in one index",
			spec: store.Spec{
				Name: "doubled", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{
					Name: "by_author",
					Fields: []store.Field{
						{Path: "author", Type: store.TypeString, Missing: store.MissingSkip},
						{Path: "author", Type: store.TypeString, Missing: store.MissingSkip},
					},
				}},
			},
			refused: `sapedb/store: the declaration does not make sense: index "by_author" names "author" twice`,
		},
		{
			name: "an index spreading over a field it does not have",
			spec: store.Spec{
				Name: "spread", Key: store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{{
					Name:   "by_tag",
					Array:  "other",
					Fields: []store.Field{{Path: "tag", Type: store.TypeString, Missing: store.MissingSkip}},
				}},
			},
			refused: `sapedb/store: the declaration does not make sense: index "by_tag" spreads over "other", which is not one of its fields`,
		},

		// The three rows below are the ones only a redeclaration can reach.
		// They are the reason this table is run against a database that
		// already holds "notes" rather than an empty one: spec.validate()
		// cannot see any of them, so a path that called validate and stopped
		// would pass every row above and fail here.
		{
			name: "moving the primary key of a collection that exists",
			spec: func() store.Spec { s := notes(); s.Key.Path = "key"; return s }(),
			refused: `sapedb/store: this does not match what was declared before: the primary key of "notes" was declared ` +
				`{Path:id Type:string Auto:ulid}`,
		},
		{
			name: "an index that keeps its name and changes its shape",
			spec: func() store.Spec {
				s := notes()
				s.Indexes[0].Unique = true
				return s
			}(),
			refused: `sapedb/store: this does not match what was declared before: index "by_author" of "notes" is already declared differently`,
		},
		{
			name: "a rollup that keeps its name and changes its shape",
			spec: func() store.Spec {
				s := notes()
				s.Name = "totalled"
				s.Rollups = []store.Rollup{{
					Name:  "per_author",
					Group: []store.Field{{Path: "slug", Type: store.TypeString, Missing: store.MissingSkip}},
					Count: true,
				}}
				return s
			}(),
			refused: `sapedb/store: this does not match what was declared before: rollup "per_author" of "totalled" is already declared differently`,
		},

		// The three controls. Everything above is a refusal, and a table of
		// refusals that agree tells you nothing unless something in it is
		// accepted by both paths as well.
		{
			name: "a new collection with an index",
			spec: store.Spec{
				Name: "events", Key: store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
				Indexes: []store.Index{{
					Name:   "by_at",
					Fields: []store.Field{{Path: "at", Type: store.TypeNumber, Missing: store.MissingLast}},
				}},
			},
		},
		{
			name: "a new collection with no indexes at all",
			spec: store.Spec{Name: "ledger", Key: store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"}},
		},
		{
			// The control for the three redeclaration rows above: without it
			// they could all be passing because something refuses every
			// declaration of a name that already exists, which is a different
			// bug wearing the same green.
			name: "the collection that already exists, declared again unchanged",
			spec: notes(),
		},
	}

	client, srv := pairedServer(t)

	// The rollup row above needs something to collide with, declared the same
	// way the notes collection was: from inside the process, before either
	// path is asked anything.
	seedRollup(t, srv)

	accepted := 0
	for _, one := range table {
		t.Run(one.name, func(t *testing.T) {
			applyErr := applyingSpec(t, srv, one.spec)
			_, wireErr := client.Establish(one.spec)

			if one.refused == "" {
				if applyErr != nil {
					t.Fatalf("apply refused a declaration it must accept: %v", applyErr)
				}
				if wireErr != nil {
					t.Fatalf("Establish refused a declaration it must accept: %v", wireErr)
				}
				accepted++
				return
			}

			if applyErr == nil {
				t.Fatalf("apply accepted %q — this row's premise is stale, fix the row, not the code", one.name)
			}
			if wireErr == nil {
				t.Fatal("Establish accepted what apply refused: this frame is a way around a rule")
			}

			// apply wraps the store's error with the file and the collection
			// name; the wire carries the store's error on its own. What must
			// match is the store's own sentence, which is the thing a rule is.
			refused := &wire.ErrRefused{}
			if !errors.As(wireErr, &refused) {
				t.Fatalf("a refusal from Establish is not an ErrRefused: %v (%T)", wireErr, wireErr)
			}
			if refused.Message != one.refused {
				t.Fatalf("Establish refused with\n got  %q\n want %q", refused.Message, one.refused)
			}
			if !strings.HasSuffix(applyErr.Error(), one.refused) {
				t.Fatalf("apply refused with\n got  %q\n which does not end in the same sentence Establish used:\n      %q",
					applyErr.Error(), one.refused)
			}
		})
	}

	if accepted != 3 {
		t.Fatalf("%d of the table's control rows were accepted by both paths, want 3 — a table of nothing but refusals measures nothing, and zero accepted rows is that table exactly",
			accepted)
	}
}

// seedRollup declares a collection carrying a rollup, from inside the process,
// so the table has something for a changed rollup to collide with.
func seedRollup(t *testing.T, srv *server.Server) {
	t.Helper()

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "totalled",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
		Rollups: []store.Rollup{{
			Name:  "per_author",
			Group: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
			Count: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// applyingSpec runs the real apply() over a one-collection schema file,
// against the same database the wire client reaches.
func applyingSpec(t *testing.T, srv *server.Server, spec store.Spec) error {
	t.Helper()

	body, err := json.Marshal(schema{Collections: []store.Spec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	applyErr := apply(db, []string{path}, io.Discard, store.Caller{Actor: "acme (apply)"})
	if applyErr != nil {
		// A refused collection declaration is NOT guaranteed to have left
		// nothing behind, which is what makes this rollback load-bearing here
		// rather than defensive: a redeclaration that drops a rollup's rows
		// and then trips over an index has already deleted them. Without the
		// rollback that mess would be handed to the next row of the table,
		// and every row after it would be measuring the mess rather than the
		// rule.
		if back := db.Rollback(); back != nil {
			t.Fatalf("rolling back a refused apply: %v", back)
		}
	}
	return applyErr
}

// TestApplyRecordsWhoDeclaredACollection is the `apply` half of the
// attribution the Establish frame's own test measures over the wire.
//
// It is measured separately rather than trusted, because the two paths build
// their caller in different places — the server from the connection it
// verified, apply from the account named on the command line — and only one of
// those can be checked by testing the other.
func TestApplyRecordsWhoDeclaredACollection(t *testing.T) {
	_, srv := pairedServer(t)

	spec := store.Spec{
		Name: "applied",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}
	if err := applyingSpec(t, srv, spec); err != nil {
		t.Fatalf("apply refused a declaration it must accept: %v", err)
	}

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	entries, declarations := 0, 0
	found := false
	if err := db.Changes(0, func(change store.Change) bool {
		entries++
		if change.Kind != store.ChangeDeclare {
			return true
		}
		declarations++
		if change.Collection != spec.Name {
			return true
		}
		found = true
		if change.By.Actor != "acme (apply)" {
			t.Errorf("a collection declared by apply is logged against actor %q, want %q",
				change.By.Actor, "acme (apply)")
		}
		if change.By.Operation != "declare" {
			t.Errorf("a collection declared by apply is logged under %q, want %q",
				change.By.Operation, "declare")
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	// A walk that found nothing is a broken measurement, not an empty result:
	// without these three, every assertion above is one that never ran.
	if entries == 0 {
		t.Fatal("the change log walk found no entries at all after an apply that succeeded")
	}
	if declarations == 0 {
		t.Fatal("the change log holds no collection entries after an apply that declared one")
	}
	if !found {
		t.Fatalf("no collection entry for %q among the %d in the log", spec.Name, declarations)
	}
}
