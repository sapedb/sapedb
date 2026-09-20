package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// This file is the measurement behind one sentence in internal/server's
// declare.go: that the Declare frame runs the same checks `sapedb apply` runs.
//
// It lives here because this is the only package that can drive both halves of
// that claim against ONE database. apply() is unexported, so nothing outside
// internal/cli can call the real thing — and a test that called
// store.DeclareOperation and said "that is what apply calls" would be
// re-asserting the claim, not measuring it. Here the apply arm is the function
// the command runs, and the wire arm is a real socket into a real server, and
// both are pointed at the same *store.Store.
//
// The shape of the table matters as much as its rows. It holds refusals AND
// two declarations that must be accepted, so a bug that refused everything —
// a handler that never reads the payload, a database it cannot reach, an
// operator gate left shut — shows up as a failed control rather than as a
// table of refusals that all agree with each other for the wrong reason.

const (
	pairSecret   = "the secret only the control plane has"
	pairPassword = "a-password-of-the-right-shape"
)

// TestDeclaringOverTheWireRefusesExactlyWhatApplyRefuses runs one table of
// declarations down both paths and demands the same answer, word for word.
func TestDeclaringOverTheWireRefusesExactlyWhatApplyRefuses(t *testing.T) {
	table := []struct {
		name      string
		operation store.Operation
		// refused is the store's own words, or "" for a declaration both
		// paths must accept.
		refused string
	}{
		{
			name: "a scan with no limit",
			operation: store.Operation{
				Name: "notes.all", Collection: "notes", Action: store.ActionScan, Index: "by_author",
			},
			refused: "sapedb/store: the declaration does not make sense: a scan must declare how many rows it may return",
		},
		{
			name: "a scan with a negative limit",
			operation: store.Operation{
				Name: "notes.back", Collection: "notes", Action: store.ActionScan, Index: "by_author", Limit: -1,
			},
			refused: "sapedb/store: the declaration does not make sense: a scan must declare how many rows it may return",
		},
		{
			// The count's own half of the limit rule. A scan declares how many
			// rows it may return; a count returns none, so what it declares is
			// how far it walks — and until that rule covered the count, this
			// row was accepted by both paths, which made `"action": "count"`
			// the one declaration in this store that said nothing about what
			// it costs.
			//
			// Measured here rather than through Explore on purpose: the
			// shell's asOperation fills the limit in for a count exactly as it
			// does for a scan, so a test taken down that path would be green
			// whether this rule existed or not.
			name: "a count with no limit",
			operation: store.Operation{
				Name: "notes.how_many", Collection: "notes", Action: store.ActionCount, Index: "by_author",
			},
			refused: "sapedb/store: the declaration does not make sense: a count must declare how far it walks — it hands back a number rather than rows, so the walk is the whole of what it costs",
		},
		{
			name: "a count with a negative limit",
			operation: store.Operation{
				Name: "notes.backwards_count", Collection: "notes", Action: store.ActionCount,
				Index: "by_author", Limit: -1,
			},
			refused: "sapedb/store: the declaration does not make sense: a count must declare how far it walks — it hands back a number rather than rows, so the walk is the whole of what it costs",
		},
		{
			name: "a collection that was never declared",
			operation: store.Operation{
				Name: "ghosts.all", Collection: "ghosts", Action: store.ActionScan, Limit: 10,
			},
			refused: `sapedb/store: no such collection: "ghosts"`,
		},
		{
			name: "an index that was never declared",
			operation: store.Operation{
				Name: "notes.by_nothing", Collection: "notes", Action: store.ActionScan,
				Index: "by_nothing", Limit: 10,
			},
			refused: `sapedb/store: no such index: "by_nothing" of "notes"`,
		},
		{
			name: "a get with no key",
			operation: store.Operation{
				Name: "notes.one", Collection: "notes", Action: store.ActionGet,
			},
			refused: "sapedb/store: the declaration does not make sense: a get says which document by its key",
		},
		{
			name: "two arguments with one name",
			operation: store.Operation{
				Name: "notes.twice", Collection: "notes", Action: store.ActionGet,
				Input: []store.Parameter{
					{Name: "id", Type: store.TypeString, Required: true},
					{Name: "id", Type: store.TypeString, Required: true},
				},
				Key: &store.Term{Arg: "id"},
			},
			refused: `sapedb/store: the declaration does not make sense: two arguments are called "id"`,
		},
		{
			name: "an argument that is required and has a default",
			operation: store.Operation{
				Name: "notes.both", Collection: "notes", Action: store.ActionGet,
				Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true, Default: "x"}},
				Key:   &store.Term{Arg: "id"},
			},
			refused: `sapedb/store: the declaration does not make sense: "id" is required and also has a default`,
		},
		{
			name: "an argument of a type nobody declares",
			operation: store.Operation{
				Name: "notes.odd", Collection: "notes", Action: store.ActionGet,
				Input: []store.Parameter{{Name: "id", Type: "colour"}},
				Key:   &store.Term{Arg: "id"},
			},
			refused: `sapedb/store: the declaration does not make sense: argument "id" is declared "colour"`,
		},
		{
			name: "a key that comes from nowhere",
			operation: store.Operation{
				Name: "notes.nowhere", Collection: "notes", Action: store.ActionGet,
				Key: &store.Term{},
			},
			refused: "sapedb/store: the declaration does not make sense: the key must be exactly one of an argument, a value, or an earlier step",
		},
		{
			name: "a batch that does nothing",
			operation: store.Operation{
				Name: "notes.nothing", Collection: "notes", Action: store.ActionBatch,
			},
			refused: "sapedb/store: the declaration does not make sense: a batch does nothing",
		},
		{
			name: "an action this store has never had",
			operation: store.Operation{
				Name: "notes.select", Collection: "notes", Action: "select",
			},
			refused: `sapedb/store: the declaration does not make sense: "select" is not something an operation can do`,
		},
		{
			// A space is a usable name here (internal/store's usableName bans
			// only the empty string, a zero byte, and over 128 characters), so
			// this row is the empty one — the first draft used "notes all" and
			// the table's own control caught it as a stale premise.
			name: "a declaration with no name",
			operation: store.Operation{
				Name: "", Collection: "notes", Action: store.ActionScan, Limit: 10,
			},
			refused: "operation name: sapedb/store: that is not a usable name: it is empty",
		},
		{
			name: "a declaration with a zero byte in its name",
			operation: store.Operation{
				Name: "notes.\x00all", Collection: "notes", Action: store.ActionScan, Limit: 10,
			},
			refused: "operation name: sapedb/store: that is not a usable name: it holds a zero byte",
		},

		// The two controls. Everything above is a refusal, and a table of
		// refusals that agree tells you nothing unless something in it is
		// accepted by both paths as well.
		{
			name: "a scan that declares its limit",
			operation: store.Operation{
				Name: "notes.recent", Collection: "notes", Action: store.ActionScan,
				Index: "by_author", Limit: 10,
			},
		},
		{
			name: "a get with a key that comes from an argument",
			operation: store.Operation{
				Name: "notes.get", Collection: "notes", Action: store.ActionGet,
				Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
				Key:   &store.Term{Arg: "id"},
			},
		},
		{
			// The control for the two count rows above. Without it they could
			// both be passing because something refuses every count, which is
			// a different bug wearing the same green.
			name: "a count that declares how far it walks",
			operation: store.Operation{
				Name: "notes.counted", Collection: "notes", Action: store.ActionCount,
				Index: "by_author", Limit: 100,
			},
		},
	}

	client, srv := pairedServer(t)

	accepted := 0
	for _, one := range table {
		t.Run(one.name, func(t *testing.T) {
			applyErr := applying(t, srv, one.operation)
			_, wireErr := client.Declare(one.operation)

			if one.refused == "" {
				if applyErr != nil {
					t.Fatalf("apply refused a declaration it must accept: %v", applyErr)
				}
				if wireErr != nil {
					t.Fatalf("Declare refused a declaration it must accept: %v", wireErr)
				}
				accepted++
				return
			}

			if applyErr == nil {
				t.Fatalf("apply accepted %q — this row's premise is stale, fix the row, not the code", one.name)
			}
			if wireErr == nil {
				t.Fatal("Declare accepted what apply refused: this frame is a way around a rule")
			}

			// apply wraps the store's error with the file and the operation
			// name; the wire carries the store's error on its own. What must
			// match is the store's own sentence, which is the thing a rule is.
			refused := &wire.ErrRefused{}
			if !errors.As(wireErr, &refused) {
				t.Fatalf("a refusal from Declare is not an ErrRefused: %v (%T)", wireErr, wireErr)
			}
			if refused.Message != one.refused {
				t.Fatalf("Declare refused with\n got  %q\n want %q", refused.Message, one.refused)
			}
			if !strings.HasSuffix(applyErr.Error(), one.refused) {
				t.Fatalf("apply refused with\n got  %q\n which does not end in the same sentence Declare used:\n      %q",
					applyErr.Error(), one.refused)
			}
		})
	}

	if accepted != 3 {
		t.Fatalf("%d of the table's control rows were accepted by both paths, want 3 — a table of nothing but refusals measures nothing", accepted)
	}
}

// applying runs the real apply() over a one-operation schema file, against the
// same database the wire client below reaches.
func applying(t *testing.T, srv *server.Server, operation store.Operation) error {
	t.Helper()

	body, err := json.Marshal(schema{Operations: []store.Operation{operation}})
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
		// apply validates before it writes, so the ordinary refusal leaves
		// nothing behind — but a refusal that did leave half a transaction
		// would hand it to the next row of the table, and every row after
		// that would be measuring the mess rather than the rule.
		if back := db.Rollback(); back != nil {
			t.Fatalf("rolling back a refused apply: %v", back)
		}
	}
	return applyErr
}

// pairedServer is one server, serving one socket, with the notes collection
// declared — and an operator-elevated wire client already pointed at it.
func pairedServer(t *testing.T) (*wire.Client, *server.Server) {
	t.Helper()

	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: pairSecret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = srv.Serve(listener) }()

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "notes",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	signature, err := srv.Sign("acme", pairPassword, "main")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	client, err := wire.Dial(connection.Connection{
		Account: "acme", Password: pairPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}, wire.Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Operate(pairSecret); err != nil {
		t.Fatal(err)
	}
	return client, srv
}

// TestApplyRecordsWhoDeclared is the `apply` half of the attribution the
// Declare frame's own test measures over the wire.
//
// `sapedb apply` is the older of the two paths into store.DeclareOperation and
// it recorded nobody for exactly as long as the wire path did. It is measured
// separately rather than trusted, because the two paths pass their caller in
// from different places — the server builds one from the connection it
// verified, apply builds one from the account named on the command line — and
// only one of those can be checked by testing the other.
//
// What the name is worth is not the same on the two paths, and this test does
// not pretend otherwise: running apply means holding the database file's
// exclusive lock, so the account here is a label rather than a proof. A label
// is still the difference between an audit trail and a blank.
func TestApplyRecordsWhoDeclared(t *testing.T) {
	_, srv := pairedServer(t)

	operation := store.Operation{
		Name: "notes.applied", Collection: "notes", Action: store.ActionScan,
		Index: "by_author", Limit: 10,
	}
	if err := applying(t, srv, operation); err != nil {
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
		if change.Kind != store.ChangeOperation {
			return true
		}
		declarations++
		if change.Operation == nil || change.Operation.Name != operation.Name {
			return true
		}
		found = true
		if change.By.Actor != "acme (apply)" {
			t.Errorf("an operation declared by apply is logged against actor %q, want %q",
				change.By.Actor, "acme (apply)")
		}
		if change.By.Operation != "declare" {
			t.Errorf("an operation declared by apply is logged under %q, want %q",
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
		t.Fatal("the change log holds no operation entries after an apply that declared one")
	}
	if !found {
		t.Fatalf("no operation entry for %q among the %d in the log", operation.Name, declarations)
	}
}
