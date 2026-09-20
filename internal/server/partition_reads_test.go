package server

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestPartitionedReadsAreWrites asks whether a get on a partitioned
// collection is a read.
//
// Four connections get four keys that hash to four different partitions, on a
// server that has just opened the database and so has none of those partition
// files open yet. Under -race, a build that lets these run together reports
// the write to the open-partition map for what it is.
func TestPartitionedReadsAreWrites(t *testing.T) {
	const keys = 16

	dir := t.TempDir()

	// Filled by one server, read by another, so that the partitions this test
	// reads are ones the reading server has never opened. That is the moment a
	// get on a partitioned collection has something to write.
	filling := serverIn(t, dir)
	spread := declareSpread(t, filling, "acme", "main", keys)
	if err := filling.Close(); err != nil {
		t.Fatal(err)
	}

	// No runtime.GC() here any more. It used to be: Server.Close closed
	// db.pages and never walked db.store's own open partition files, so each
	// one's exclusive lock was still held by an *os.File nothing had a name
	// for, and reopening the same directory failed with vfs.ErrLocked for a
	// reason that had nothing to do with the race this test measures. Two
	// forced collections ran the fd finalizers and papered over it. Close now
	// gives those files back on purpose (internal/store's Store.Close, called
	// from Server.Close), measured by TestClosingAServerGivesBackItsPartition
	// FilesToo in close_test.go — so the dodge is gone, and if that fix ever
	// regresses this test goes red with it instead of hiding it.
	server, address := serving(t, dir)

	clients := make([]*client, len(spread))
	for i := range clients {
		clients[i] = dial(t, address)
		if frame := clients[i].open(server, "acme", "main"); frame.Type != protocol.Welcome {
			t.Fatalf("handshake: %s", frame.Type)
		}
	}

	start := make(chan struct{})
	wait := sync.WaitGroup{}
	found := make([]int, len(clients))
	for i, one := range clients {
		wait.Add(1)
		go func(i int, one *client) {
			defer wait.Done()
			<-start
			frame := one.invoke("spread.get", map[string]any{"id": spread[i]})
			if frame.Type != protocol.Result {
				t.Errorf("get %q: %s: %s", spread[i], frame.Type, frame.Payload)
				return
			}
			found[i] = decode[store.Result](t, frame).Count
		}(i, one)
	}
	close(start)
	wait.Wait()

	hits := 0
	for _, one := range found {
		hits += one
	}
	if hits == 0 {
		t.Fatal("no get found its document: this measured an empty database, not a read")
	}
	t.Logf("%d of %d gets found their document", hits, len(found))
}

// serverIn is a server on a directory of this test's choosing, not serving.
func serverIn(t *testing.T, dir string) *Server {
	t.Helper()
	server, err := New(Options{Dir: dir, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// serving is serverIn with a socket under it.
func serving(t *testing.T, dir string) (*Server, string) {
	t.Helper()
	server := serverIn(t, dir)
	t.Cleanup(func() { _ = server.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()
	return server, listener.Addr().String()
}

// declareSpread makes a hash-partitioned collection, fills it, and hands back
// one key per partition.
func declareSpread(t *testing.T, server *Server, account, name string, documents int) []string {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}

	collection, err := db.Declare(store.Spec{
		Name:      "spread",
		Key:       store.Key{Path: "id", Type: store.TypeString},
		Partition: &store.Partition{By: store.ByHash, Into: 8},
	})
	if err != nil {
		release()
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Operation{
		Name: "spread.get", Collection: "spread", Action: store.ActionGet,
		Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &store.Term{Arg: "id"},
	}); err != nil {
		release()
		t.Fatal(err)
	}

	made := []string{}
	for i := 0; i < documents; i++ {
		id := fmt.Sprintf("k%03d", i)
		if _, err := collection.Put(map[string]any{"id": id, "body": "x"}); err != nil {
			release()
			t.Fatal(err)
		}
		made = append(made, id)
	}
	if err := db.Commit(); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	return made
}
