package server

import (
	"errors"
	"runtime"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

// TestClosingAServerGivesBackItsPartitionFilesToo is the fix for a Close that
// let go of one file out of nine.
//
// A database with a partitioned collection is one leader file plus one file
// per partition, each opened by db.store and each holding an exclusive lock of
// its own. Server.Close used to close db.pages — the leader — and then empty
// s.open, which dropped the only reference anybody held to the partitions.
// The locks stayed taken, so the next Server on the same directory was refused
// with vfs.ErrLocked on a .part file, and the only thing that ever released
// them was a garbage collection running the *os.File finalizers.
//
// Which is why this test holds the first server's store alive to the last
// line. Without that, the fd finalizers can run at any allocation and the test
// goes green on a build with the bug in it — that is not a stricter assertion
// than the old runtime.GC() dodge in partition_reads_test.go, it is the same
// fact from the other side: with the *Store reachable, no finalizer can run,
// so the second server opening those files is a measurement of Close and
// nothing else.
func TestClosingAServerGivesBackItsPartitionFilesToo(t *testing.T) {
	dir := t.TempDir()

	first := serverIn(t, dir)
	spread := declareSpread(t, first, "acme", "main", 16)

	// A name for the first server's store that outlives the server, so its
	// open partition files stay reachable and unfinalizable below.
	leader, release, err := first.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	release()

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := serverIn(t, dir)
	t.Cleanup(func() { _ = second.Close() })

	db, releaseSecond, err := second.Store("acme", "main")
	if err != nil {
		t.Fatalf("a second server could not open a directory the first one closed: %v", err)
	}
	defer releaseSecond()

	// One get per key, which is one partition file opened per distinct hash:
	// the read path (Collection.into) is what actually opens a .part file, so
	// a lock still held by the first server surfaces here and nowhere else.
	for _, id := range spread {
		result, err := db.Invoke(store.Caller{Actor: "test"}, "spread.get", 0, map[string]any{"id": id})
		if err != nil {
			if errors.Is(err, vfs.ErrLocked) {
				t.Fatalf("reading %q: the first server never gave back the partition file: %v", id, err)
			}
			t.Fatalf("reading %q: %v", id, err)
		}
		if result.Count != 1 {
			t.Fatalf("reading %q came back with %d documents, want 1", id, result.Count)
		}
	}

	runtime.KeepAlive(leader)
}

// TestClosingAServerTwiceIsNotAnError: Close empties what it closed, so a
// second call has nothing left to let go of. Worth pinning because the
// partition close added to Close() is a second failure source in a method
// tests and cleanups call more than once.
func TestClosingAServerTwiceIsNotAnError(t *testing.T) {
	dir := t.TempDir()

	server := serverIn(t, dir)
	declareSpread(t, server, "acme", "main", 4)

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("closing a closed server: %v", err)
	}
}
