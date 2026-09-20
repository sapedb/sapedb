package server

import (
	"errors"
	"net"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

func (c *client) establish(spec store.Spec) protocol.Frame {
	c.t.Helper()
	c.send(protocol.Establish, establishing{Spec: spec})
	return c.read()
}

// notesSpec is the collection the tests below establish over the wire, kept in
// a function so a row that changes one field does not change it for everybody.
func notesSpec() store.Spec {
	return store.Spec{
		Name: "notes",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}
}

// TestEstablishingNeedsMoreThanAConnectionString is the gate.
//
// A connection string says which database a caller reaches. It does not say
// what may be declared once there, and a frame that creates collections is the
// last place that distinction should be left implicit: a caller who could
// establish a collection on any database its string reached could fill
// somebody else's disk with indexes.
//
// The control at the end is what makes the refusal mean anything: the SAME
// frame on the SAME connection is accepted once the proof is given, so the
// refusal above is the gate and not a malformed request.
func TestEstablishingNeedsMoreThanAConnectionString(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))

	frame := client.establish(notesSpec())
	if frame.Type != protocol.Failure {
		t.Fatalf("an ordinary connection established a collection: %s %s", frame.Type, frame.Payload)
	}
	if code := decode[struct {
		Code string `json:"code"`
	}](t, frame).Code; code != "not_operator" {
		t.Errorf("refused with code %q, want not_operator", code)
	}

	// Nothing was stored by the attempt.
	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Collection("notes")
	release()
	if !errors.Is(err, store.ErrNoCollection) {
		t.Fatalf("a refused declaration left something behind: %v", err)
	}

	// The control: the same frame, on the same connection, after the proof.
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}
	if frame := client.establish(notesSpec()); frame.Type != protocol.Result {
		t.Fatalf("an operator was refused: %s", frame.Payload)
	}
}

// TestEstablishingAnExistingCollectionUpgradesItInPlace is the decision this
// frame had to make and the measurement it was made on.
//
// Declare answers the same question differently: an operation declared over a
// name that exists gets a NEW version, and every older one stays runnable,
// because a caller is built against a version and a redeclaration must not
// move the ground under it.
//
// A collection cannot be versioned that way and the reason is physical, not a
// matter of taste: a collection is where the documents are. Two versions would
// be two sets of index entries over one set of documents, or two sets of
// documents — and either way, the next writer has to be told which one it
// meant, through an interface that has never had a version in it.
//
// So the answer is: establishing a name that already exists brings THAT
// collection up to date, in place. That is store.Declare's own behaviour,
// which is why this test measures it through the frame rather than asserting
// it: the id does not change (so it is one collection and not a second one),
// the documents already stored are still there, an index that arrives late is
// built over them, an index left out takes its entries with it, and the
// changes that cannot be made in place are refused rather than done quietly.
func TestEstablishingAnExistingCollectionUpgradesItInPlace(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	first := decode[established](t, mustEstablish(t, client, notesSpec())).Spec
	if first.ID == 0 {
		t.Fatalf("the collection came back with id 0, so nothing assigned it one: %+v", first)
	}

	// Documents, written through the engine that is serving, so the
	// redeclaration below has something already stored to be measured against.
	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	notes, err := db.Collection("notes")
	if err != nil {
		release()
		t.Fatal(err)
	}
	for _, author := range []string{"ann", "bo", "cy"} {
		if _, err := notes.Put(map[string]any{"id": author, "author": author, "slug": "s-" + author}); err != nil {
			release()
			t.Fatal(err)
		}
	}
	if err := db.Commit(); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	// An index that arrives after the documents did.
	later := notesSpec()
	later.Indexes = append(later.Indexes, store.Index{
		Name:   "by_slug",
		Fields: []store.Field{{Path: "slug", Type: store.TypeString, Missing: store.MissingSkip}},
	})
	second := decode[established](t, mustEstablish(t, client, later)).Spec

	// (1) One collection, not two. This is the whole difference from Declare:
	// there is no version, and the id says the thing that came back is the
	// thing that was already there.
	if second.ID != first.ID {
		t.Fatalf("redeclaring made a collection with id %d where id %d was, so it is not the same collection", second.ID, first.ID)
	}
	if len(second.Indexes) != 2 {
		t.Fatalf("the upgraded collection holds indexes %+v", second.Indexes)
	}
	// The index that was already there keeps the id it already had — an index
	// id is written into every one of its keys, so renumbering it would orphan
	// every entry.
	if second.Indexes[0].Name != "by_author" || second.Indexes[0].ID != first.Indexes[0].ID {
		t.Fatalf("the index that was already there came back as %+v, want %+v", second.Indexes[0], first.Indexes[0])
	}

	// (2) The documents survived, and the late index was built over them
	// rather than only over whatever is written next.
	db, release, err = server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	notes, err = db.Collection("notes")
	if err != nil {
		release()
		t.Fatal(err)
	}
	slugs := 0
	if err := notes.Scan("by_slug", store.Range{}, func(store.Found) bool { slugs++; return true }); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if slugs != 3 {
		t.Fatalf("the index declared after the documents holds %d entries, want 3 — it was not built from what was already there", slugs)
	}

	// (3) An index left out of a later declaration is dropped, entries and
	// all. Measured because it is the half of "in place" that destroys
	// something: whoever establishes a collection is editing the one that
	// exists, and this says so out loud.
	fewer := decode[established](t, mustEstablish(t, client, notesSpec())).Spec
	if len(fewer.Indexes) != 1 {
		t.Fatalf("an index left out of the declaration is still there: %+v", fewer.Indexes)
	}
	db, release, err = server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	notes, err = db.Collection("notes")
	if err != nil {
		release()
		t.Fatal(err)
	}
	err = notes.Scan("by_slug", store.Range{}, func(store.Found) bool { return true })
	release()
	if !errors.Is(err, store.ErrNoIndex) {
		t.Fatalf("scanning the dropped index: want ErrNoIndex, got %v", err)
	}

	// (4) And what cannot be done in place is refused, in the store's own
	// words, rather than done quietly or done as a second collection.
	moved := notesSpec()
	moved.Key.Path = "key"
	frame := client.establish(moved)
	if frame.Type != protocol.Failure {
		t.Fatalf("moving the primary key of a collection that holds documents was accepted: %s", frame.Payload)
	}
	const cannot = `sapedb/store: this does not match what was declared before: the primary key of "notes" was declared {Path:id Type:string Auto:ulid}`
	if got := reason(t, frame); got != cannot {
		t.Fatalf("refused with\n got  %q\n want %q", got, cannot)
	}

	// And the refusal left the collection as it was, rather than half moved.
	still := decode[established](t, mustEstablish(t, client, notesSpec())).Spec
	if still.ID != first.ID || still.Key.Path != "id" {
		t.Fatalf("after the refusal the collection reads %+v", still)
	}
}

// mustEstablish sends the frame and fails the test if it was refused, so the
// tests above read as the sequence they are.
func mustEstablish(t *testing.T, c *client, spec store.Spec) protocol.Frame {
	t.Helper()
	frame := c.establish(spec)
	if frame.Type != protocol.Result {
		t.Fatalf("establishing %q: %s", spec.Name, frame.Payload)
	}
	return frame
}

// TestACollectionEstablishedOverTheWireSurvivesTheServerBeingRestarted is the
// difference between a declaration and a declaration that was committed.
//
// The handler calls store.Declare and then Commit, exactly as `sapedb apply`
// does. Without that second call the collection is real for the connection
// that made it and gone at the next restart — which is a failure nothing in
// the same process can see, because the in-memory catalogue agrees with the
// caller right up until the file is reopened.
func TestACollectionEstablishedOverTheWireSurvivesTheServerBeingRestarted(t *testing.T) {
	dir := t.TempDir()

	first := serverIn(t, dir)
	declare(t, first, "acme", "main")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = first.Serve(listener) }()

	client := dial(t, listener.Addr().String())
	greeting := decode[welcome](t, client.open(first, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	mustEstablish(t, client, notesSpec())

	// Nothing writes between the declaration and the shutdown, on purpose.
	_ = listener.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := serverIn(t, dir)
	t.Cleanup(func() { _ = second.Close() })

	db, release, err := second.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	notes, err := db.Collection("notes")
	if err != nil {
		t.Fatalf("the collection did not survive the restart: %v", err)
	}
	stored := notes.Spec()
	if len(stored.Indexes) != 1 || stored.Indexes[0].Name != "by_author" {
		t.Fatalf("it came back as %+v", stored)
	}
}
