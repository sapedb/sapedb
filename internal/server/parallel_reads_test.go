package server

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestReadsShareTheDatabase asks whether two reads of one database run inside
// it at the same time. It is the standing gate for SAPE-18 — reads share a
// database instead of queueing behind each other — and it does not measure
// time.
//
// The gate it replaces did. It timed one read, then four at once, and failed
// when four cost more than three. That ratio is bounded by the number of cores
// the machine has: four counts over four thousand documents are four pieces of
// CPU work, and on a two-core runner they cannot finish in much under four
// times one however well the locking behaves. So the ratio could not tell "the
// lock serialized them" from "the machine had nothing spare to run them on",
// and on a small runner it reported the first while the second was true. It
// failed every one of this repository's first automated runs, against a build
// whose reads do share, printing "they are queueing, not sharing" — a sentence
// that was false each time it appeared. The comment there blamed a loaded CI
// box and set the bar at 3x to leave room for it. Load was never the cause,
// and no bar can fix the cause, because on a one-core box a serialized build
// and a sharing one produce the same number.
//
// What this one does instead is count. Each read adds itself to the database's
// reading counter for as long as it is inside the store, and the high-water
// mark of that counter is the answer: two reads in there at once is sharing,
// and a database behind an exclusive lock can never reach two, on any number
// of cores, because the second read has not been let in. The counting is in
// the database itself (server.go) rather than in a sampler here, because a
// sampler competes for threads with the reads it is watching and can only miss
// what it does not catch: on one thread, 2000 goroutine profiles over eight
// seconds found no read inside the store at all while four connections read
// without pause. A read that counts itself is exact rather than sampled.
//
// What it does need is more than one thread, and that is a precondition it
// states rather than a bar it tunes. A count is CPU between two lock
// operations with nothing in it that yields, so on a single-threaded run the
// first read finishes before the second is ever scheduled: the overlap is
// absent in fact, not prevented by a lock, and no observation can tell those
// apart. Note what the precondition is not — it is not that cores are free.
// Two threads fully occupied by other work still run these reads inside each
// other; they are only slower. That is the whole difference from the gate this
// replaces, which needed spare cores and had no way to say so.
func TestReadsShareTheDatabase(t *testing.T) {
	const documents = 4000
	const readers = 4
	// patience is how long the readers are given to produce an overlap. A
	// count over 4000 documents takes milliseconds, so a sharing build reaches
	// two within the first few; this is long enough that the only thing a
	// slower machine costs is a slower pass.
	const patience = 5 * time.Second

	// Loudly, and before anything is measured. A skip would be worse than a
	// failure here: this repository's CI counts a skipped test as a failure on
	// purpose, and a gate that quietly stands down on some machines is not one.
	if threads := runtime.GOMAXPROCS(0); threads < 2 {
		t.Fatalf("GOMAXPROCS is %d, and two reads cannot be inside a database at once on one thread "+
			"whatever the locking allows, so nothing here could be measured: "+
			"this test needs at least two (runtime.NumCPU reports %d)",
			threads, runtime.NumCPU())
	}

	server, address := running(t, false)
	loadRows(t, server, "acme", "main", documents)

	db, err := server.database("acme", "main")
	if err != nil {
		t.Fatal(err)
	}

	clients := make([]*client, readers)
	for i := range clients {
		clients[i] = dial(t, address)
		if frame := clients[i].open(server, "acme", "main"); frame.Type != protocol.Welcome {
			t.Fatalf("handshake: %s", frame.Type)
		}
		// One count each before the measurement, so that what is watched is
		// reads and not the page reads a database does only the first time.
		countOnce(t, clients[i])
	}

	stop := make(chan struct{})
	counting := sync.WaitGroup{}
	for _, one := range clients {
		counting.Add(1)
		go func(one *client) {
			defer counting.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				countOnce(t, one)
			}
		}(one)
	}

	began := time.Now()
	for db.everReading.Load() < 2 && time.Since(began) < patience {
		time.Sleep(time.Millisecond)
	}
	took := time.Since(began)

	close(stop)
	counting.Wait()

	most := db.everReading.Load()
	t.Logf("%d connections counting %d documents each for %v: the most reads inside the database at once was %d",
		readers, documents, took.Round(time.Millisecond), most)

	// Zero is not the same failure as one, and saying so is the point: a run
	// that recorded no reads at all has measured nothing, and a claim about
	// locking would be invented rather than observed.
	if most == 0 {
		t.Fatalf("%d connections counted for %v and the database recorded no read running at any point: "+
			"either they never reached the store or the counting in database.read is broken, "+
			"so this run says nothing about whether reads share",
			readers, took.Round(time.Millisecond))
	}
	if most < 2 {
		t.Fatalf("%d connections counted for %v; reads reached the database and ran there, and never two of them at once: "+
			"a read waited for the one before it to finish, which is queueing, not sharing",
			readers, took.Round(time.Millisecond))
	}

	// The high-water mark is only worth reading if the count comes back down.
	// Every reader has had its last answer by now, so every read has finished.
	if left := db.reading.Load(); left != 0 {
		t.Fatalf("every reader has stopped and the database still counts %d reads running: "+
			"the count is not balanced, so the %d above is not a measurement of anything",
			left, most)
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

	collection, err := db.Declare(store.Caller{}, store.Spec{
		Name: "rows",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "rows.count", Collection: "rows", Action: store.ActionCount,
		// A count declares how far it walks, like any other declaration; this
		// one is above the number of documents the test writes, so the count
		// it reports is the whole collection.
		Limit: documents * 10,
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

func countOnce(t *testing.T, one *client) {
	t.Helper()
	frame := one.invoke("rows.count", map[string]any{})
	if frame.Type != protocol.Result {
		t.Fatalf("count: %s: %s", frame.Type, frame.Payload)
	}
	// A count that found nothing would walk an empty collection and be
	// recorded as a read of a database with nothing in it.
	result := decode[store.Result](t, frame)
	if result.Count == 0 {
		t.Fatal("the count came back zero: this round measured an empty database, not a read")
	}
}
