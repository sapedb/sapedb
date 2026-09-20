package server

import (
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestReadsRunTogether measures whether two reads of one database overlap.
//
// It is a timing measurement rather than an assertion about locks, because a
// lock is not what a caller pays: four counts that take one count's time have
// run together, and four that take four have not, whatever the code says it
// does. The read is a count over enough documents that the engine work is the
// whole of it — a single-key get spends most of its time in the protocol,
// which is parallel either way and would hide the thing being measured.
func TestReadsRunTogether(t *testing.T) {
	const documents = 4000
	const readers = 4

	server, address := running(t, false)
	loadRows(t, server, "acme", "main", documents)

	solo := timeReads(t, server, address, 1)
	together := timeReads(t, server, address, readers)

	t.Logf("one reader: %v; %d readers: %v (%.2f× one)", solo, readers, together,
		float64(together)/float64(solo))

	// Serialized (a plain sync.Mutex, measured on this same machine for task
	// 0070 §1), four of these cost close to four — the A/B this test is a
	// standing version of saw 3.9-4.0×. Shared, it measured 1.6-2.1× here,
	// noisy with whatever else this machine is doing at the moment (this is
	// wall-clock time over a real socket, not an isolated microbenchmark).
	// The bar sits at 3×: well above the shared case's noise band, well below
	// the serialized one, so it fails on queueing rather than on a loaded CI
	// box. (A 2× bar measured flaky here — occasional 2.0-2.3× runs with
	// nothing structurally wrong — which is why this is 3×, not the 2× a
	// tighter machine could get away with.)
	if together > 3*solo {
		t.Fatalf("%d reads of one database took %v, and one takes %v: they are queueing, not sharing",
			readers, together, solo)
	}
}

// loadRows declares a collection with a count over all of it, and puts documents
// in it, in one transaction.
func loadRows(t *testing.T, server *Server, account, name string, documents int) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	collection, err := db.Declare(store.Spec{
		Name: "rows",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Operation{
		Name: "rows.count", Collection: "rows", Action: store.ActionCount,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < documents; i++ {
		if _, err := collection.Put(map[string]any{"body": "a row of no particular interest"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// timeReads is how long it takes for every one of n connections to finish one
// count, all of them asked at once.
func timeReads(t *testing.T, server *Server, address string, n int) time.Duration {
	t.Helper()

	clients := make([]*client, n)
	for i := range clients {
		clients[i] = dial(t, address)
		if frame := clients[i].open(server, "acme", "main"); frame.Type != protocol.Welcome {
			t.Fatalf("handshake: %s", frame.Type)
		}
	}

	// One round first, so that nothing being measured is a page read that
	// happens to be the first.
	for _, one := range clients {
		countOnce(t, one)
	}

	best := time.Duration(0)
	for round := 0; round < 3; round++ {
		start := make(chan struct{})
		wait := sync.WaitGroup{}
		for _, one := range clients {
			wait.Add(1)
			go func(one *client) {
				defer wait.Done()
				<-start
				countOnce(t, one)
			}(one)
		}
		began := time.Now()
		close(start)
		wait.Wait()
		took := time.Since(began)
		if best == 0 || took < best {
			best = took
		}
	}
	return best
}

func countOnce(t *testing.T, one *client) {
	t.Helper()
	frame := one.invoke("rows.count", map[string]any{})
	if frame.Type != protocol.Result {
		t.Fatalf("count: %s: %s", frame.Type, frame.Payload)
	}
	// A count that found nothing would time an empty walk and call it a read.
	result := decode[store.Result](t, frame)
	if result.Count == 0 {
		t.Fatal("the count came back zero: this round measured an empty database, not a read")
	}
}

// TestReadsRunTogetherAtN32 is TestReadsRunTogether's own measurement, at the
// concurrency the A1 task asked to see: one database, 32 readers, through a
// real socket, on a heavier op (count over 20000 rows) so the server-side
// critical section is what the timing actually reflects rather than protocol
// overhead swallowing it. Not part of the permanent suite's assertions — it
// only logs — kept here for A1's own A/B, not as a standing regression gate
// (a gate at N=32 on a shared CI box would be too noisy to keep green).
func TestReadsRunTogetherAtN32(t *testing.T) {
	const documents = 20000
	const readers = 32

	server, address := running(t, false)
	loadRows(t, server, "acme", "main", documents)

	solo := timeReads(t, server, address, 1)
	together := timeReads(t, server, address, readers)

	t.Logf("one reader: %v; %d readers: %v (%.2f× one)", solo, readers, together,
		float64(together)/float64(solo))
}
