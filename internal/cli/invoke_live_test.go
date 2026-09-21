package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestInvokingAgainstARealServer is shell_live_test.go's argument applied to
// the one command that was missing: this package's own tests agree with this
// package, and a shell that parses an `invoke` line perfectly into a call the
// server refuses has not closed anything.
//
// So: a real server, a real signed connection, declarations a stranger's
// module would look like — namespaced, with arguments — and every action an
// invoke can carry. A write, a read, a scan that stops at its declared limit,
// a count, and a batch across two collections in one transaction.
func TestInvokingAgainstARealServer(t *testing.T) {
	mustExist := true
	secret := "the-secret-this-server-was-started-with"

	live, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })

	db, release, err := live.Store("acme", "books")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []store.Spec{
		{
			Name: "books",
			Key:  store.Key{Path: "id", Type: store.TypeString},
			Indexes: []store.Index{{
				Name:   "by_shelf",
				Fields: []store.Field{{Path: "shelf", Type: store.TypeString, Missing: store.MissingSkip}},
			}},
		},
		{
			Name: "loans",
			Key:  store.Key{Path: "id", Type: store.TypeString},
		},
	} {
		if _, err := db.Declare(store.Caller{}, spec); err != nil {
			t.Fatal(err)
		}
	}

	// Two of these are namespaced, which is the shape an installed bundle
	// declares and the shape whose colon has to survive the argument parser.
	for _, operation := range []store.Operation{
		{
			Name: "shop:books.shelve", Collection: "books", Action: store.ActionInsert,
			Input: []store.Parameter{
				{Name: "id", Type: store.TypeString, Required: true},
				{Name: "title", Type: store.TypeString, Required: true},
				{Name: "shelf", Type: store.TypeString, Required: true},
				{Name: "pages", Type: store.TypeNumber, Required: true},
			},
			Document: map[string]store.Term{
				"id": {Arg: "id"}, "title": {Arg: "title"},
				"shelf": {Arg: "shelf"}, "pages": {Arg: "pages"},
				"on_loan": {Value: false, Constant: true},
			},
		},
		{
			Name: "shop:books.get", Collection: "books", Action: store.ActionGet,
			Input:      []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
			Key:        &store.Term{Arg: "id"},
			Projection: []string{"id", "title", "shelf"},
		},
		{
			Name: "books.on_shelf", Collection: "books", Action: store.ActionScan,
			Index: "by_shelf", Limit: 2,
			Input:      []store.Parameter{{Name: "shelf", Type: store.TypeString, Required: true}},
			From:       &store.Endpoint{Terms: []store.Term{{Arg: "shelf"}}},
			To:         &store.Endpoint{Terms: []store.Term{{Arg: "shelf"}}},
			Projection: []string{"id", "title"},
		},
		{
			Name: "books.how_many", Collection: "books", Action: store.ActionCount,
			Index: "by_shelf", Limit: 100,
			Input: []store.Parameter{{Name: "shelf", Type: store.TypeString, Required: true}},
			From:  &store.Endpoint{Terms: []store.Term{{Arg: "shelf"}}},
			To:    &store.Endpoint{Terms: []store.Term{{Arg: "shelf"}}},
		},
		{
			Name: "books.borrow", Collection: "loans", Action: store.ActionBatch,
			Input: []store.Parameter{
				{Name: "book", Type: store.TypeString, Required: true},
				{Name: "loan", Type: store.TypeString, Required: true},
			},
			Steps: []store.Step{
				{
					Name: "book", Action: store.ActionUpdate, Collection: "books",
					Key: &store.Term{Arg: "book"}, Exists: &mustExist,
					Set: map[string]store.Term{"on_loan": {Value: true, Constant: true}},
				},
				{
					Name: "loan", Action: store.ActionInsert, Collection: "loans",
					Document: map[string]store.Term{
						"id":   {Arg: "loan"},
						"book": {Step: "book", Field: "key"},
					},
				},
			},
		},
	} {
		if _, err := db.DeclareOperation(store.Caller{Actor: "the test"}, operation); err != nil {
			t.Fatalf("declaring %q: %v", operation.Name, err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	release()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = live.Serve(listener) }()

	client, err := connect(options{secret: secret, account: "acme", db: "books"}, listener.Addr().String(), true)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	out := &strings.Builder{}
	typing := strings.NewReader(strings.Join([]string{
		// Three inserts, so the scan below has something to stop short of.
		`invoke shop:books.shelve id=bk-1 title=the-first shelf=history pages=210`,
		`invoke shop:books.shelve id=bk-2 title=the-second shelf=history pages=95`,
		`invoke shop:books.shelve id=bk-3 title=the-third shelf=history pages=400`,
		`invoke shop:books.get id=bk-2`,
		`invoke books.on_shelf shelf=history`,
		`invoke books.how_many shelf=history`,
		`invoke books.borrow book=bk-1 loan=ln-1`,
		`invoke shop:books.get id=bk-1`,

		// And the four refusals, against a server that would answer if they
		// got that far.
		`invoke shop:books.get`,
		`invoke shop:books.get id=bk-1 shelf=history`,
		`invoke shop:books.shelve id=bk-4 title=t shelf=s pages=many`,
		`invoke shop:books.melt id=bk-1`,
		`exit`,
	}, "\n") + "\n")

	if err := Shell(client, "books", typing, out); err != nil {
		t.Fatal(err)
	}
	printed := out.String()

	for _, wanted := range []string{
		// The inserts: a key came back, and one document changed.
		`key "bk-1"`,
		"changed 1",
		// The get, through its declared projection.
		`{"id":"bk-2","shelf":"history","title":"the-second"}`,
		// The scan stopped at the limit the declaration names, and said so.
		"stopped at the limit",
		// The count, which is a number rather than rows.
		"  3\n",
		// The batch: two documents in one transaction.
		"changed 2",

		// The four refusals, each naming what was wrong.
		`"shop:books.get" needs "id"`,
		`"shop:books.get" does not take "shelf"`,
		`"pages" is a number, and "many" is not one`,
		`there is no operation called "shop:books.melt"`,
	} {
		if !strings.Contains(printed, wanted) {
			t.Errorf("the session did not contain %q:\n%s", wanted, printed)
		}
	}

	// The scan's declared limit is 2 and three books are on that shelf, so
	// the third must not be in the answer. A limit that never left this
	// process would show up here and nowhere else.
	if strings.Contains(printed, "the-third") {
		t.Errorf("the scan came back past its declared limit:\n%s", printed)
	}

	// The batch left the database readable afterwards, through the same
	// declaration as before it — asserted on the text that follows the
	// batch's own answer, so a get that only worked before the transaction
	// would not pass it.
	_, after, found := strings.Cut(printed, "changed 2")
	if !found || !strings.Contains(after, `{"id":"bk-1","shelf":"history","title":"the-first"}`) {
		t.Errorf("the read after the batch did not come back:\n%s", printed)
	}

	// And the log names the operation, not "explore". That is the difference
	// between an invoke and the typed accesses beside it: the change log of a
	// database somebody invoked into says which declaration did it.
	db, release, err = live.Store("acme", "books")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	shelved := 0
	if err := db.Changes(1, func(change store.Change) bool {
		if change.By.Operation == "shop:books.shelve" {
			shelved++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if shelved != 3 {
		t.Errorf("the log holds %d writes by shop:books.shelve, want 3", shelved)
	}
}
