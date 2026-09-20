package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/ulid"
)

// Applying an entry is covered by TestAReplicaFedTheLogEndsUpTheSame and
// TestOperationsTravelDownTheLogToo: documents, declarations and operations
// all travel, and the second pass over the same log changes nothing.
//
// What those do not look at is the two things an entry carries that are not
// the change itself — the number the declaration was given, and the caller's
// own id for the write. Both were dropped on the floor by Apply, and both are
// invisible to a comparison of documents and declarations, which is why they
// survived a test that compares documents and declarations.

// documentsOf is every document of a database, addressed by collection and
// key.
func documentsOf(t *testing.T, store *Store) map[string]map[string]any {
	t.Helper()

	out := map[string]map[string]any{}
	for _, name := range store.Collections() {
		collection, err := store.Collection(name)
		if err != nil {
			t.Fatalf("collection %q: %v", name, err)
		}
		if err := collection.Walk(func(key any, document map[string]any) bool {
			out[fmt.Sprintf("%s/%v", name, key)] = document
			return true
		}); err != nil {
			t.Fatalf("walk %q: %v", name, err)
		}
	}
	return out
}

// written is a database with one of every kind of entry in its log, and the
// log it produced.
//
// Every kind, because a replay that handles five of six is a replay that is
// wrong only on the database where the sixth one happened.
func written(t *testing.T, seed int64) (*Store, []Change) {
	t.Helper()

	_, primary := fresh(t, seed)

	collection, err := primary.Declare(Caller{}, articles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Declare(Caller{}, Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString, Auto: "ulid"}}); err != nil {
		t.Fatal(err)
	}
	notes, err := primary.Collection("notes")
	if err != nil {
		t.Fatal(err)
	}

	declareOp(t, primary, byAuthor())
	narrowed := byAuthor()
	narrowed.Limit = 1
	declareOp(t, primary, narrowed)

	for i := 0; i < 4; i++ {
		put(t, collection, map[string]any{
			"id": fmt.Sprintf("a%d", i), "author": "ann",
			"published": float64(i), "title": fmt.Sprintf("Article %d", i),
			"slug": fmt.Sprintf("s%d", i),
		})
	}
	if _, err := collection.Delete("a3"); err != nil {
		t.Fatal(err)
	}

	// A write under a caller's own id, which is what a retry is answered from.
	if _, err := collection.PutBy(
		Attribution{Operation: "articles.write", Actor: "ann", WriteID: "w-1"},
		map[string]any{"id": "a7", "author": "ann", "published": float64(7), "title": "Under a write id", "slug": "s7"},
	); err != nil {
		t.Fatal(err)
	}

	// Somebody looking: an entry that changes nothing and still has to arrive.
	if _, err := primary.WhatIsHere(Caller{Actor: "operator"}); err != nil {
		t.Fatal(err)
	}

	// And a collection destroyed.
	put(t, notes, map[string]any{"body": "a note that goes with the collection"})
	if err := primary.Drop(Caller{Actor: "operator"}, "notes"); err != nil {
		t.Fatal(err)
	}

	all := changes(t, primary, 0)

	// A filter that selects nothing passes quietly, so this counts what it is
	// about to measure before measuring it.
	seen := map[string]int{}
	for _, change := range all {
		seen[change.Kind]++
	}
	for _, kind := range []string{ChangePut, ChangeDelete, ChangeDeclare, ChangeOperation, ChangeDrop, ChangeRead} {
		if seen[kind] == 0 {
			t.Fatalf("the log carries no %q entry, so applying one is not what this measured: %v", kind, seen)
		}
	}

	return primary, all
}

// follower is a blank database that mints generated keys somewhere else than
// the primary does, so that a key it invented rather than carried is a key a
// comparison can see.
func follower(t *testing.T, seed int64) *Store {
	t.Helper()

	_, replica := fresh(t, seed)
	at := int64(1900000000000)
	replica.Identifiers(ulid.With(
		func() time.Time { at++; return time.UnixMilli(at) },
		endless{},
	))
	return replica
}

// TestAFollowerThatDeclaresOfItsOwnDoesNotReuseAnID is about the number a
// declaration carries rather than the declaration itself.
//
// A collection id is written into the key of every document and every index
// entry it has. A database hands them out from a counter; one applying
// somebody else's log installs the number that arrives instead of taking one,
// and so never moves that counter. The first collection it declares of its
// own afterwards — which is what happens the moment it is promoted, or used
// for anything besides following — was handed a number already in use, and
// its documents would be written into the middle of another collection's.
func TestAFollowerThatDeclaresOfItsOwnDoesNotReuseAnID(t *testing.T) {
	// The control: a database that hands out its own numbers gives two
	// collections two of them, so the repeat looked for below is a finding
	// and not a check that cannot fail.
	_, plain := fresh(t, 270)
	first, err := plain.Declare(Caller{}, articles())
	if err != nil {
		t.Fatal(err)
	}
	second, err := plain.Declare(Caller{}, Spec{Name: "notes", Key: Key{Path: "id", Type: TypeString}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec().ID == second.Spec().ID {
		t.Fatalf("a database handed both collections id %d", first.Spec().ID)
	}

	_, all := written(t, 271)
	replica := follower(t, 272)
	for _, change := range all {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("apply %d (%s): %v", change.LSN, change.Kind, err)
		}
	}

	taken := map[uint32]string{}
	for _, name := range replica.Collections() {
		collection, err := replica.Collection(name)
		if err != nil {
			t.Fatal(err)
		}
		taken[collection.Spec().ID] = name
	}
	if len(taken) == 0 {
		t.Fatal("the replica followed no declaration, so there is no number to collide with")
	}

	mine, err := replica.Declare(Caller{}, Spec{Name: "local", Key: Key{Path: "id", Type: TypeString}})
	if err != nil {
		t.Fatal(err)
	}
	if name, clash := taken[mine.Spec().ID]; clash {
		t.Fatalf("the follower gave %q id %d, which %q already has", "local", mine.Spec().ID, name)
	}

	// And the collision it would have caused, measured rather than assumed:
	// writing into the new collection leaves the followed one as it was.
	before := documentsOf(t, replica)
	if before["articles/a1"] == nil {
		t.Fatal("the followed collection holds no articles/a1, so overwriting it is not what this measures")
	}
	if _, err := mine.Put(map[string]any{"id": "a1", "body": "written into the follower's own collection"}); err != nil {
		t.Fatal(err)
	}
	articlesHere, err := replica.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	document, found, err := articlesHere.Get("a1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the followed collection lost a document to the new one")
	}
	if fmt.Sprint(document) != fmt.Sprint(before["articles/a1"]) {
		t.Errorf("articles/a1 was %v and is now %v", before["articles/a1"], document)
	}
}

// TestAWriteIDArrivesWithTheEntryItBelongsTo is about the other thing an entry
// carries that is not the change: the caller's own id for the write.
//
// It is what a retry after a lost connection is answered from. A database that
// applied the change and not the id would answer the same retry by doing the
// write again — which is exactly the case the id exists for, on exactly the
// database somebody failed over to.
func TestAWriteIDArrivesWithTheEntryItBelongsTo(t *testing.T) {
	primary, all := written(t, 273)

	// The control: the id is answerable on the database that made the write.
	lsn, key, found, err := primary.Wrote("w-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the primary does not remember the write id it recorded")
	}

	replica := follower(t, 274)
	if _, _, before, err := replica.Wrote("w-1"); err != nil {
		t.Fatal(err)
	} else if before {
		t.Fatal("a blank database already remembers the write id, so arriving is not what this measures")
	}

	for _, change := range all {
		if err := replica.Apply(change); err != nil {
			t.Fatalf("apply %d (%s): %v", change.LSN, change.Kind, err)
		}
	}

	mirrored, mirroredKey, arrived, err := replica.Wrote("w-1")
	if err != nil {
		t.Fatal(err)
	}
	if !arrived {
		t.Fatal("the write id did not travel with its entry: a retry would be applied twice")
	}
	if mirrored != lsn {
		t.Errorf("the write landed at %d and the replica says %d", lsn, mirrored)
	}
	if fmt.Sprint(mirroredKey) != fmt.Sprint(key) {
		t.Errorf("the write was under key %v and the replica says %v", key, mirroredKey)
	}
}
