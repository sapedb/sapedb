package sapedb

// The worked module, measured (SAPE-12).
//
// examples/library/module.json is a whole small business module rather than
// one missing verb: three collections with their own keys, four indexes, one
// rollup, and nine operations across five of the nine actions. It exists to be
// read by somebody outside this project who wants to write a second one, and
// these tests exist so that it cannot rot while nobody is looking.
//
// The three things they hold:
//
//  1. It installs over the wire, on one connection, into a database that has
//     none of its collections — which is SAPE-14's eighth acceptance criterion,
//     the one SAPE-14 shipped without and carried to this ticket. Collections
//     go over frame 14 (establish) and operations over frame 13 (declare);
//     there is no `sapedb apply` step and the daemon is never stopped.
//  2. Its cost envelopes say what it actually does. Not that the envelope
//     exists — that the four facts in it are the four facts a caller would
//     measure by running the operation and watching.
//  3. Every read that hands back documents declares a projection, so a client
//     can derive a typed row from it. This is SAPE-13's rule, and the module is
//     its positive case.
//
// Every expectation below is written out by hand on the test side. None of it
// is read out of module.json and compared against itself, because a test that
// derives its expectation from the thing it guards passes whatever that thing
// becomes.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/bundle"
	"github.com/sapedb/sapedb/internal/store"
)

// modulePath is the draft an author publishes and a stranger copies.
const modulePath = "examples/library/module.json"

// The three collections the module carries, written out here rather than
// counted out of the file.
var libraryCollections = []string{"library_books", "library_loans", "library_members"}

// The nine operations, and for each one whether it hands back documents. The
// three that do are the three that must declare a projection.
var libraryOperations = map[string]bool{
	"library:books.shelve":     false, // insert
	"library:members.join":     false, // insert
	"library:books.get":        true,  // get
	"library:books.by_author":  true,  // scan
	"library:books.on_shelf":   false, // count — a number, not rows
	"library:loans.borrow":     false, // batch
	"library:loans.give_back":  false, // batch
	"library:loans.of_member":  true,  // scan
	"library:loans.per_member": false, // totals — a rollup row, not a document
}

// TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfItsCollections
// is SAPE-14's criterion 8 and SAPE-12's second acceptance criterion, measured
// together because they are the same sentence: a packaged module installs into
// a database that did not anticipate it, over one connection, with the daemon
// left running.
//
// The database this starts on is empty — not "has different collections",
// empty — and that is asserted before anything is installed rather than
// assumed, because an install into a database that already held the
// collections would pass every check below without proving anything.
func TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfItsCollections(t *testing.T) {
	dir := t.TempDir()
	signature := seedAnEmptyDatabase(t, dir)

	daemon, address := startDaemon(t, dir)
	pid := daemon.Process.Pid

	client := dialLibrary(t, address, signature)
	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}

	// The control that makes every later failure legible: this connection can
	// read the catalogue already, so a refusal below is the install failing
	// rather than a connection that never worked.
	before, err := client.WhatIsHere()
	if err != nil {
		t.Fatalf("the connection cannot even read the catalogue: %v", err)
	}
	if len(before.Collections) != 0 || len(before.Operations) != 0 {
		t.Fatalf("the database was meant to be empty and holds %d collections and %d operations: %+v",
			len(before.Collections), len(before.Operations), before)
	}

	installed := installLibraryOverTheWire(t, client)

	// Same connection, and the daemon is the process it was. Asserted on the
	// process rather than inferred from the socket still answering.
	if daemon.Process.Pid != pid {
		t.Fatalf("the daemon is pid %d and was pid %d", daemon.Process.Pid, pid)
	}
	if daemon.ProcessState != nil {
		t.Fatalf("the daemon exited during the install: %v", daemon.ProcessState)
	}

	after, err := client.WhatIsHere()
	if err != nil {
		t.Fatalf("reading the catalogue after the install: %v", err)
	}

	gotCollections := make([]string, 0, len(after.Collections))
	for _, spec := range after.Collections {
		gotCollections = append(gotCollections, spec.Name)
	}
	sort.Strings(gotCollections)
	if fmt.Sprint(gotCollections) != fmt.Sprint(libraryCollections) {
		t.Fatalf("the database holds collections %v, want %v", gotCollections, libraryCollections)
	}

	for name := range libraryOperations {
		found := false
		for _, operation := range after.Operations {
			if operation.Name == name {
				found = true
				if operation.Version != 1 {
					t.Errorf("%q installed at version %d, want 1 on a database that never held it", name, operation.Version)
				}
			}
		}
		if !found {
			t.Errorf("%q is not in the catalogue after the install", name)
		}
	}
	if len(after.Operations) != len(libraryOperations) {
		t.Fatalf("the database holds %d operations, want the %d the module declares",
			len(after.Operations), len(libraryOperations))
	}
	if installed != len(libraryOperations) {
		t.Fatalf("the install declared %d operations, want %d", installed, len(libraryOperations))
	}

	// And now run it. A module that installs but cannot be used is a module
	// that installed.
	runTheLibrary(t, client)
}

// runTheLibrary exercises the module the way its reader would: shelve books,
// enrol a member, lend a book, read it back four different ways, and take it
// back again.
func runTheLibrary(t *testing.T, client *Client) {
	t.Helper()

	books := []struct{ id, title, author, shelf string }{
		{"bk-1", "The Left Hand of Darkness", "Le Guin", "sf-a"},
		{"bk-2", "A Wizard of Earthsea", "Le Guin", "sf-a"},
		{"bk-3", "Piranesi", "Clarke", "sf-b"},
	}
	for _, book := range books {
		if _, err := client.Invoke("library:books.shelve", map[string]any{
			"id": book.id, "title": book.title, "author": book.author, "shelf": book.shelf,
		}); err != nil {
			t.Fatalf("shelving %s: %v", book.id, err)
		}
	}

	if _, err := client.Invoke("library:members.join", map[string]any{
		"id": "mem-1", "name": "Ada", "joined": float64(20260101),
	}); err != nil {
		t.Fatalf("enrolling a member: %v", err)
	}

	// A get, through its projection.
	got, err := client.Invoke("library:books.get", map[string]any{"id": "bk-1"})
	if err != nil {
		t.Fatalf("reading bk-1: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("library:books.get returned %d rows, want 1", len(got.Rows))
	}
	if got.Rows[0]["title"] != "The Left Hand of Darkness" {
		t.Fatalf("bk-1 came back as %+v", got.Rows[0])
	}
	if got.Rows[0]["on_loan"] != false {
		t.Fatalf("a freshly shelved book says on_loan %v, want false", got.Rows[0]["on_loan"])
	}
	t.Logf("library:books.get bk-1 -> %v by %v, shelf %v, on_loan %v",
		got.Rows[0]["title"], got.Rows[0]["author"], got.Rows[0]["shelf"], got.Rows[0]["on_loan"])

	// A scan of an index of two fields, bounded on the first.
	byAuthor, err := client.Invoke("library:books.by_author", map[string]any{"author": "Le Guin"})
	if err != nil {
		t.Fatalf("scanning by author: %v", err)
	}
	if len(byAuthor.Rows) != 2 {
		t.Fatalf("Le Guin has %d books on the shelf, want 2: %+v", len(byAuthor.Rows), byAuthor.Rows)
	}
	// The index is (author, title), so the order is the titles' order.
	if byAuthor.Rows[0]["title"] != "A Wizard of Earthsea" {
		t.Fatalf("the scan came back in the order %v", titlesOf(byAuthor.Rows))
	}
	t.Logf("library:books.by_author \"Le Guin\" -> %d rows, in index order %v",
		len(byAuthor.Rows), titlesOf(byAuthor.Rows))

	// A count: a number, and no rows at all.
	counted, err := client.Invoke("library:books.on_shelf", map[string]any{"shelf": "sf-a"})
	if err != nil {
		t.Fatalf("counting shelf sf-a: %v", err)
	}
	if counted.Count != 2 {
		t.Fatalf("shelf sf-a holds %d books, want 2", counted.Count)
	}
	if len(counted.Rows) != 0 {
		t.Fatalf("a count handed back %d rows; it is meant to hand back a number", len(counted.Rows))
	}
	t.Logf("library:books.on_shelf \"sf-a\" -> count %d, rows %d", counted.Count, len(counted.Rows))

	// The batch: three writes across three collections, as one transaction.
	borrowed, err := client.Invoke("library:loans.borrow", map[string]any{
		"book": "bk-1", "member": "mem-1", "borrowed": float64(20260210), "due": float64(20260310),
	})
	if err != nil {
		t.Fatalf("borrowing bk-1: %v", err)
	}
	if borrowed.Changed != 3 {
		t.Fatalf("borrowing changed %d documents, want 3 — the book, the member and the new loan", borrowed.Changed)
	}
	t.Logf("library:loans.borrow bk-1 -> %d documents changed in one transaction", borrowed.Changed)

	// The book is now out, which is the condition the batch set.
	out, err := client.Invoke("library:books.get", map[string]any{"id": "bk-1"})
	if err != nil {
		t.Fatalf("re-reading bk-1: %v", err)
	}
	if out.Rows[0]["on_loan"] != true {
		t.Fatalf("bk-1 says on_loan %v after being borrowed, want true", out.Rows[0]["on_loan"])
	}

	// Lending it again is refused, by the condition written into the
	// declaration rather than by anything the caller remembered to check.
	if _, err := client.Invoke("library:loans.borrow", map[string]any{
		"book": "bk-1", "member": "mem-1", "borrowed": float64(20260211), "due": float64(20260311),
	}); err == nil {
		t.Fatal("a book already on loan was lent a second time")
	}

	// The loan is readable through its own index, newest first.
	loans, err := client.Invoke("library:loans.of_member", map[string]any{"member": "mem-1"})
	if err != nil {
		t.Fatalf("reading a member's loans: %v", err)
	}
	if len(loans.Rows) != 1 {
		t.Fatalf("mem-1 has %d loans, want 1: %+v", len(loans.Rows), loans.Rows)
	}
	if loans.Rows[0]["book"] != "bk-1" || loans.Rows[0]["returned"] != false {
		t.Fatalf("the loan came back as %+v", loans.Rows[0])
	}
	loanID, ok := loans.Rows[0]["id"].(string)
	if !ok || loanID == "" {
		t.Fatalf("the loan has no id to give back with: %+v", loans.Rows[0])
	}
	t.Logf("library:loans.of_member mem-1 -> %d loan, fields %v", len(loans.Rows), sortedKeys(loans.Rows[0]))

	// The rollup, kept by the same transaction that wrote the loan.
	totals, err := client.Invoke("library:loans.per_member", nil)
	if err != nil {
		t.Fatalf("reading the rollup: %v", err)
	}
	if len(totals.Rows) != 1 {
		t.Fatalf("the rollup holds %d rows, want 1: %+v", len(totals.Rows), totals.Rows)
	}
	if n := totals.Rows[0]["count"]; fmt.Sprint(n) != "1" {
		t.Fatalf("the rollup counted %v loans for mem-1, want 1 — row %+v", n, totals.Rows[0])
	}
	t.Logf("library:loans.per_member -> %+v", totals.Rows[0])

	// And back it comes.
	returned, err := client.Invoke("library:loans.give_back", map[string]any{
		"loan": loanID, "book": "bk-1", "returned_at": float64(20260225),
	})
	if err != nil {
		t.Fatalf("giving bk-1 back: %v", err)
	}
	if returned.Changed != 2 {
		t.Fatalf("returning changed %d documents, want 2 — the loan and the book", returned.Changed)
	}
	back, err := client.Invoke("library:books.get", map[string]any{"id": "bk-1"})
	if err != nil {
		t.Fatalf("re-reading bk-1 after its return: %v", err)
	}
	if back.Rows[0]["on_loan"] != false {
		t.Fatalf("bk-1 says on_loan %v after being returned, want false", back.Rows[0]["on_loan"])
	}
	t.Logf("library:loans.give_back -> %d documents changed; bk-1 on_loan %v",
		returned.Changed, back.Rows[0]["on_loan"])
}

// TestTheWorkedModulesCostEnvelopesSayWhatItActuallyDoes is SAPE-8's
// measurement applied to this module, and SAPE-12's fourth criterion.
//
// The envelopes below are typed out by hand. They are not read from
// module.json and they are not read from the catalogue and compared with
// themselves: each one is what a person deciding whether to install this
// module would work out for themselves from the declaration, and the test is
// whether the server agrees.
//
// Each envelope is then paired with the operation actually running, because a
// ceiling that only ever subtracts cannot go red on its own — an operation
// that returned nothing at all would satisfy "no more than 50 rows" forever.
func TestTheWorkedModulesCostEnvelopesSayWhatItActuallyDoes(t *testing.T) {
	dir := t.TempDir()
	signature := seedAnEmptyDatabase(t, dir)
	_, address := startDaemon(t, dir)

	client := dialLibrary(t, address, signature)
	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}
	installLibraryOverTheWire(t, client)

	want := map[string]store.Envelope{
		"library:books.get": {
			Operation: "library:books.get", Version: 1,
			Collections: []string{"library_books"},
			Indexes:     []string{},
			Limit:       1,
			Projection:  []string{"author", "id", "on_loan", "shelf", "title"},
		},
		"library:books.by_author": {
			Operation: "library:books.by_author", Version: 1,
			Collections: []string{"library_books"},
			Indexes:     []string{"by_author"},
			Limit:       50,
			Projection:  []string{"id", "on_loan", "shelf", "title"},
		},
		"library:loans.of_member": {
			Operation: "library:loans.of_member", Version: 1,
			Collections: []string{"library_loans"},
			Indexes:     []string{"by_member"},
			Limit:       25,
			Projection:  []string{"book", "borrowed", "due", "id", "returned"},
		},
		"library:books.on_shelf": {
			Operation: "library:books.on_shelf", Version: 1,
			Collections: []string{"library_books"},
			Indexes:     []string{"by_shelf"},
			Limit:       1000,
		},
		// The two facts worth the whole exercise: borrow touches all three
		// collections and give_back touches two of them. The list is exact in
		// both directions, and these two operations are what proves it —
		// library_members is in one and not in the other.
		"library:loans.borrow": {
			Operation: "library:loans.borrow", Version: 1,
			Collections: []string{"library_books", "library_loans", "library_members"},
			Indexes:     []string{},
			Limit:       3,
		},
		"library:loans.give_back": {
			Operation: "library:loans.give_back", Version: 1,
			Collections: []string{"library_books", "library_loans"},
			Indexes:     []string{},
			Limit:       2,
		},
	}

	here, err := client.WhatIsHere()
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	got := map[string]store.Envelope{}
	for _, envelope := range here.Envelopes {
		got[envelope.Operation] = envelope
	}

	for name, expected := range want {
		actual, found := got[name]
		if !found {
			t.Errorf("no cost envelope for %q — the catalogue carries %d", name, len(here.Envelopes))
			continue
		}
		if fmt.Sprint(actual.Collections) != fmt.Sprint(expected.Collections) {
			t.Errorf("%s touches collections %v, and its envelope says %v",
				name, expected.Collections, actual.Collections)
		}
		if fmt.Sprint(actual.Indexes) != fmt.Sprint(expected.Indexes) {
			t.Errorf("%s uses indexes %v, and its envelope says %v",
				name, expected.Indexes, actual.Indexes)
		}
		if actual.Limit != expected.Limit {
			t.Errorf("%s has a row ceiling of %d, and its envelope says %d",
				name, expected.Limit, actual.Limit)
		}
		if fmt.Sprint(actual.Projection) != fmt.Sprint(expected.Projection) {
			t.Errorf("%s lets fields %v escape, and its envelope says %v",
				name, expected.Projection, actual.Projection)
		}
	}

	// Now the other half: run them, and check the envelope against what came
	// back. Four books by one author, against a ceiling of 50 — the ceiling is
	// not what stops this scan, so the count below is a positive assertion and
	// not a ceiling being trivially satisfied.
	for _, book := range []struct{ id, title string }{
		{"bk-1", "A Wizard of Earthsea"},
		{"bk-2", "The Dispossessed"},
		{"bk-3", "The Left Hand of Darkness"},
		{"bk-4", "The Word for World Is Forest"},
	} {
		if _, err := client.Invoke("library:books.shelve", map[string]any{
			"id": book.id, "title": book.title, "author": "Le Guin", "shelf": "sf-a",
		}); err != nil {
			t.Fatalf("shelving %s: %v", book.id, err)
		}
	}

	scanned, err := client.Invoke("library:books.by_author", map[string]any{"author": "Le Guin"})
	if err != nil {
		t.Fatalf("scanning by author: %v", err)
	}
	if len(scanned.Rows) != 4 {
		t.Fatalf("the scan returned %d rows, want the 4 books written: %+v", len(scanned.Rows), scanned.Rows)
	}
	if len(scanned.Rows) > want["library:books.by_author"].Limit {
		t.Fatalf("the scan returned %d rows, over its declared ceiling of %d",
			len(scanned.Rows), want["library:books.by_author"].Limit)
	}
	// The escaping-field list, exercised rather than compared as a string: the
	// keys of a row that actually came out of the database are exactly the
	// fields the envelope said would escape, and "author" — which the document
	// has and the projection does not name — is not among them.
	for _, row := range scanned.Rows {
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if fmt.Sprint(keys) != fmt.Sprint(want["library:books.by_author"].Projection) {
			t.Fatalf("a row carries fields %v; its envelope promised %v",
				keys, want["library:books.by_author"].Projection)
		}
		if _, leaked := row["author"]; leaked {
			t.Fatalf("author escaped through an operation whose envelope does not list it: %+v", row)
		}
	}

	// And the collection list, in the direction that is easy to get wrong.
	// give_back's envelope does not list library_members, so running it must
	// leave the member document alone. Borrow's does, and moves it.
	if _, err := client.Invoke("library:members.join", map[string]any{
		"id": "mem-1", "name": "Ada", "joined": float64(20260101),
	}); err != nil {
		t.Fatalf("enrolling a member: %v", err)
	}
	if _, err := client.Invoke("library:loans.borrow", map[string]any{
		"book": "bk-1", "member": "mem-1", "borrowed": float64(20260210), "due": float64(20260310),
	}); err != nil {
		t.Fatalf("borrowing bk-1: %v", err)
	}
	loans, err := client.Invoke("library:loans.of_member", map[string]any{"member": "mem-1"})
	if err != nil {
		t.Fatalf("reading the loan back: %v", err)
	}
	if len(loans.Rows) != 1 {
		t.Fatalf("mem-1 has %d loans, want 1", len(loans.Rows))
	}
	loanID := loans.Rows[0]["id"].(string)

	memberBefore := readMember(t, client, dir)
	if _, err := client.Invoke("library:loans.give_back", map[string]any{
		"loan": loanID, "book": "bk-1", "returned_at": float64(20260225),
	}); err != nil {
		t.Fatalf("giving bk-1 back: %v", err)
	}
	memberAfter := readMember(t, client, dir)
	if fmt.Sprint(memberBefore) != fmt.Sprint(memberAfter) {
		t.Fatalf("give_back changed library_members, which its envelope does not list:\n  before %v\n  after  %v",
			memberBefore, memberAfter)
	}
	// Paired with the positive: borrow's envelope DOES list library_members,
	// and borrowing did move it — otherwise "unchanged" above would be a fact
	// about a collection nothing ever writes.
	if memberAfter["last_borrowed"] == nil {
		t.Fatalf("library_members was never written by borrow, so give_back leaving it alone proves nothing: %v", memberAfter)
	}
}

// TestEveryWorkedModuleReadOfDocumentsDeclaresAProjection is SAPE-13's rule,
// held against the published draft rather than against a database: a bundle
// author copies this file, and what they copy has to teach the projected path.
//
// Both directions are checked. Every read named below must declare a
// projection, and the set of reads found in the file must be exactly the set
// named below — so adding a fourth `scan` without a projection reddens this
// even though it would satisfy "every operation in my list has one".
func TestEveryWorkedModuleReadOfDocumentsDeclaresAProjection(t *testing.T) {
	var draft struct {
		Operations []store.Operation `json:"operations"`
	}
	raw, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("reading the published module: %v", err)
	}
	if err := json.Unmarshal(raw, &draft); err != nil {
		t.Fatalf("the published module is not readable as declarations: %v", err)
	}

	// Typed out here, not derived: these are the operations that hand a caller
	// a document, and so the ones whose row a client can only type if the
	// operation says which fields it returns.
	wantProjected := []string{
		"library:books.by_author",
		"library:books.get",
		"library:loans.of_member",
	}

	found := []string{}
	for _, operation := range draft.Operations {
		if operation.Action != store.ActionGet && operation.Action != store.ActionScan {
			continue
		}
		found = append(found, operation.Name)
		if len(operation.Projection) == 0 {
			t.Errorf("%q is a %s and declares no projection, so a client cannot type its row",
				operation.Name, operation.Action)
		}
	}
	sort.Strings(found)
	if fmt.Sprint(found) != fmt.Sprint(wantProjected) {
		t.Fatalf("the module's document reads are %v; this test was written against %v", found, wantProjected)
	}

	// The module is meant to be a whole module, not a worked read. If it stops
	// covering more than one action it has stopped being the example this
	// ticket asked for.
	actions := map[string]bool{}
	for _, operation := range draft.Operations {
		actions[operation.Action] = true
	}
	for _, action := range []string{
		store.ActionInsert, store.ActionGet, store.ActionScan, store.ActionCount,
		store.ActionBatch, store.ActionTotals,
	} {
		if !actions[action] {
			t.Errorf("the module no longer covers the %q action", action)
		}
	}

	// And every operation is in a namespace, which is the point of having one.
	for _, operation := range draft.Operations {
		namespace, _, err := store.SplitOperationName(operation.Name)
		if err != nil {
			t.Errorf("%q is not a usable name: %v", operation.Name, err)
			continue
		}
		if namespace != "library" {
			t.Errorf("%q is in namespace %q, want %q", operation.Name, namespace, "library")
		}
	}
}

// installLibraryOverTheWire is the install, and the thing under test in the
// first test above: seal the published draft with a key made here, put it
// through the same trust check `sapedb install` uses, and then send its
// collections and its operations down one connection that is already open.
//
// The key is generated per run rather than committed. A private key in a
// repository is a private key that is not private, and nothing here needs the
// signature to be the same one twice: what is being measured is that the
// published declarations survive being sealed, verified and installed, not
// that a particular 64 bytes do.
func installLibraryOverTheWire(t *testing.T, client *Client) int {
	t.Helper()

	raw, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("reading the published module: %v", err)
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		t.Fatalf("the published module is not a bundle this version can read: %v", err)
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Seal(&b, private); err != nil {
		t.Fatalf("sealing the published module: %v", err)
	}

	keyText, err := bundle.PublicKeyText(public)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := bundle.ParseTrust("worked-example=" + keyText)
	if err != nil {
		t.Fatal(err)
	}
	label, err := trust.Verify(b)
	if err != nil {
		t.Fatalf("the sealed module does not verify: %v", err)
	}
	if label != "worked-example" {
		t.Fatalf("the bundle verified as %q, want %q", label, "worked-example")
	}

	// Collections first, then operations: an operation naming a collection
	// that is not there yet is refused, and that ordering is the whole of what
	// a module install is.
	for _, spec := range b.Collections {
		if _, err := client.Establish(spec); err != nil {
			t.Fatalf("establishing %q over the wire: %v", spec.Name, err)
		}
	}
	for _, operation := range b.Operations {
		if _, err := client.Declare(operation); err != nil {
			t.Fatalf("declaring %q over the wire: %v", operation.Name, err)
		}
	}
	return len(b.Operations)
}

// dialLibrary opens one connection to a daemon this test started.
func dialLibrary(t *testing.T, address, signature string) *Client {
	t.Helper()

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, Timeout: 10 * time.Second, RequestTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("dialling the daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// readMember reads the one member document through an ad-hoc explore rather
// than through an operation of the module, so that what it observes does not
// depend on the projection the module happens to declare.
func readMember(t *testing.T, client *Client, dir string) map[string]any {
	t.Helper()
	_ = dir

	explored, err := client.Explore(Access{
		Kind:       store.ActionScan,
		Collection: "library_members",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("exploring library_members: %v", err)
	}
	if len(explored.Result.Rows) != 1 {
		t.Fatalf("library_members holds %d documents, want 1", len(explored.Result.Rows))
	}
	return explored.Result.Rows[0]
}

// sortedKeys is the fields a row actually carried, in a stable order.
func sortedKeys(row map[string]any) []string {
	out := make([]string, 0, len(row))
	for key := range row {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func titlesOf(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprint(row["title"]))
	}
	return out
}
