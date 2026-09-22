package cli

import (
	"errors"
	"runtime"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

// TestClosingACommandsDatabaseGivesBackItsPartitionFilesToo is the fix for a
// closer that let go of the leader and the directory lock and kept every
// partition file the command had touched (ISS-7).
//
// It is the same bug, in the same shape, as the one fixed server-side in
// 390e976 — internal/server/server.go's Close and the comment there have the
// diagnosis — and it is measured the same way: by the mechanism that actually
// failed. A second open of the same directory in the same process must be able
// to READ, because the leader and the directory lock were always given back and
// only the .part files were not. Asserting that a function was called would
// have passed against a closer that called it in the wrong order or on the
// wrong store.
//
// The first store is held alive to the last line for the reason close_test.go
// in internal/server gives: without a name for it the fd finalizers can run at
// any allocation and release the locks the closer should have released, and the
// test goes green against a build with the bug in it.
func TestClosingACommandsDatabaseGivesBackItsPartitionFilesToo(t *testing.T) {
	setup := start(t)
	opts := options{
		dir: setup.dir, account: "acme", db: "main", secret: secret, lookup: setup.env(nil),
	}

	first, closeFirst, err := open(opts)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}

	entries, err := first.Declare(store.Caller{}, store.Spec{
		Name:      "entries",
		Key:       store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Partition: &store.Partition{By: store.ByTime, Every: store.EveryMonth},
	})
	if err != nil {
		t.Fatalf("declaring a partitioned collection: %v", err)
	}
	if _, err := first.DeclareOperation(store.Caller{}, store.Operation{
		Name: "entries.get", Collection: "entries", Action: store.ActionGet,
		Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &store.Term{Arg: "id"},
	}); err != nil {
		t.Fatalf("declaring the read: %v", err)
	}

	// Writing is what opens a partition file: until something lands in one,
	// there is no .part file to be left locked and this test would measure
	// nothing.
	written := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		key, err := entries.Put(map[string]any{"body": "an entry of no particular interest"})
		if err != nil {
			t.Fatalf("writing: %v", err)
		}
		written = append(written, key.(string))
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}

	closeFirst()

	second, closeSecond, err := open(opts)
	if err != nil {
		t.Fatalf("a second open of a directory this process had already closed: %v", err)
	}
	defer closeSecond()

	// One get per key, which is one partition file opened per distinct key: the
	// read path (Collection.into) is what actually opens a .part file, so a
	// lock the first open never gave back surfaces here and nowhere else.
	for _, id := range written {
		result, err := second.Invoke(store.Caller{Actor: "test"}, "entries.get", 0, map[string]any{"id": id})
		if err != nil {
			if errors.Is(err, vfs.ErrLocked) {
				t.Fatalf("reading %q: the first open never gave back the partition file: %v", id, err)
			}
			t.Fatalf("reading %q: %v", id, err)
		}
		if result.Count != 1 {
			t.Fatalf("reading %q came back with %d documents, want 1", id, result.Count)
		}
	}

	runtime.KeepAlive(first)
}

// TestClosingACommandsDatabaseTwiceIsNotAPanic: the closer gained a call, and
// it is a closer that a deferred cleanup and an explicit close can both reach.
// store.Close empties what it closed, so a second call has nothing left to let
// go of — worth pinning because a second failure source in a function nobody
// checks the return value of is exactly the kind that is found in production.
func TestClosingACommandsDatabaseTwiceIsNotAPanic(t *testing.T) {
	setup := start(t)
	opts := options{
		dir: setup.dir, account: "acme", db: "main", secret: secret, lookup: setup.env(nil),
	}

	db, closeDB, err := open(opts)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name:      "entries",
		Key:       store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Partition: &store.Partition{By: store.ByTime, Every: store.EveryMonth},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}

	closeDB()
	closeDB()
}
