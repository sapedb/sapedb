package server

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAComposedReadSharesTheDatabase asks whether two COMPOSED reads of one
// database run inside it at the same time.
//
// It is the standing gate for ISS-9, and it is deliberately the same shape as
// TestReadsShareTheDatabase (parallel_reads_test.go) rather than a measurement
// of its own invention: each read adds itself to the database's reading counter
// for as long as it is inside the store, and the high-water mark of that
// counter is the answer. Two at once is sharing. A database behind an exclusive
// lock can never reach two, on any number of cores, because the second read has
// not been let in — which is why this counts instead of timing. That gate's own
// godoc has the longer version of why a ratio of durations could not tell "the
// lock serialized them" from "the machine had nothing spare to run them on".
//
// What is new here is only what is being invoked. Before ISS-9 this test could
// not have reached two however the locking behaved: composition is spelled
// `action: batch`, batch is a write in store.Writes, and SharedRead answered no
// before it looked at a single step — so three scans in one call, which is the
// headline SAPE-20 sells, dropped the read lock and took the database to
// itself.
func TestAComposedReadSharesTheDatabase(t *testing.T) {
	const documents = 4000
	const readers = 4
	const patience = 5 * time.Second

	// The same precondition, stated the same way and for the same reason: a
	// composed read is CPU between two lock operations with nothing in it that
	// yields, so on a single-threaded run the first finishes before the second
	// is scheduled and the overlap is absent in fact rather than prevented by a
	// lock. A skip would be worse than a failure — this repository's CI counts
	// a skipped test as a failure on purpose.
	if threads := runtime.GOMAXPROCS(0); threads < 2 {
		t.Fatalf("GOMAXPROCS is %d, and two reads cannot be inside a database at once on one thread "+
			"whatever the locking allows, so nothing here could be measured: "+
			"this test needs at least two (runtime.NumCPU reports %d)",
			threads, runtime.NumCPU())
	}

	server, address := running(t, false)
	loadRows(t, server, "acme", "main", documents)
	declareThreePages(t, server, "acme", "main")

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
		// One each before the measurement, so what is watched is composed reads
		// and not the page reads a database does only the first time.
		threePagesOnce(t, clients[i])
	}

	stop := make(chan struct{})
	reading := sync.WaitGroup{}
	for _, one := range clients {
		reading.Add(1)
		go func(one *client) {
			defer reading.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				threePagesOnce(t, one)
			}
		}(one)
	}

	began := time.Now()
	for db.everReading.Load() < 2 && time.Since(began) < patience {
		time.Sleep(time.Millisecond)
	}
	took := time.Since(began)

	close(stop)
	reading.Wait()

	most := db.everReading.Load()
	t.Logf("%d connections running a composed operation of three scans over %d documents for %v: "+
		"the most reads inside the database at once was %d", readers, documents, took.Round(time.Millisecond), most)

	if most == 0 {
		t.Fatalf("%d connections ran a composed read for %v and the database recorded no read running at any point: "+
			"the composed operation never took the shared path, so it is still classified as a write",
			readers, took.Round(time.Millisecond))
	}
	if most < 2 {
		t.Fatalf("%d connections ran a composed read for %v; the reads reached the database and ran there, and never two at once: "+
			"a composed read waited for the one before it, which is queueing, not sharing",
			readers, took.Round(time.Millisecond))
	}
	if left := db.reading.Load(); left != 0 {
		t.Fatalf("every reader has stopped and the database still counts %d reads running: "+
			"the count is not balanced, so the %d above is not a measurement of anything",
			left, most)
	}
}

// TestAComposedWriteStillTakesTheDatabaseToItself is the other side of ISS-9,
// and it is measured by the same counter rather than by asking a classifier.
//
// db.read (server.go) is the ONLY place the reading counter is touched, and it
// is reached only down the shared branch of invoke. So a composed operation
// that runs many times and leaves that counter at zero never held the read
// lock — which is the fact the ticket is about, observed where it happens
// rather than asserted about a function that was called on the way there.
//
// It runs on a database of its own so the counter starts at zero and the
// number at the end is about these calls and nothing else.
func TestAComposedWriteStillTakesTheDatabaseToItself(t *testing.T) {
	const writes = 40

	server, address := running(t, false)
	loadRows(t, server, "acme", "writers", 8)
	declareThreePages(t, server, "acme", "writers")
	declareAWritingComposition(t, server, "acme", "writers")

	db, err := server.database("acme", "writers")
	if err != nil {
		t.Fatal(err)
	}
	if before := db.everReading.Load(); before != 0 {
		t.Fatalf("the database had already recorded %d concurrent reads before this test invoked anything", before)
	}

	one := dial(t, address)
	if frame := one.open(server, "acme", "writers"); frame.Type != protocol.Welcome {
		t.Fatalf("handshake: %s", frame.Type)
	}

	for i := 0; i < writes; i++ {
		frame := one.invoke("rows.page_then_write", map[string]any{})
		if frame.Type != protocol.Result {
			t.Fatalf("the composed write: %s: %s", frame.Type, frame.Payload)
		}
		result := decode[store.Result](t, frame)
		if result.Changed != 1 {
			t.Fatalf("the composed write changed %d documents, want 1 — this round wrote nothing, so it says nothing about locks", result.Changed)
		}
	}

	// The measurement. Not "at most one at a time" — zero: a call that took the
	// read lock would have been counted, once, even running alone.
	if most := db.everReading.Load(); most != 0 {
		t.Fatalf("%d composed operations that each insert a document were recorded as reads (high-water mark %d): "+
			"a batch whose step calls an insert took the read lock", writes, most)
	}

	// The control, so the zero above is about the write and not about a
	// database nothing can read: the read-only composition on the SAME database
	// is counted.
	if frame := one.invoke("rows.three_pages", map[string]any{}); frame.Type != protocol.Result {
		t.Fatalf("the composed read: %s: %s", frame.Type, frame.Payload)
	}
	if most := db.everReading.Load(); most != 1 {
		t.Fatalf("one composed read on this database was recorded as %d reads, want 1: "+
			"the zero above would be a counter nothing reaches rather than a measurement of the write path", most)
	}
}

// declareThreePages declares the headline composed read: three scans of the
// same collection in one call, which is what SAPE-20 sells and what ISS-9 says
// performs as a pure read and executes like a write.
func declareThreePages(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// A limit large enough that each leg builds real rows, which is what makes
	// a call long enough for two of them to be inside the database at once.
	const page = 400

	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "rows.page", Collection: "rows", Action: store.ActionScan, Limit: page,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "rows.three_pages", Collection: "rows", Action: store.ActionBatch, Limit: 3 * page,
		Steps: []store.Step{
			{Operation: "rows.page", Version: 1},
			{Operation: "rows.page", Version: 1},
			{Operation: "rows.page", Version: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// declareAWritingComposition declares a batch that reads a page and then calls
// an insert — read-shaped at the top, a write one call down, which is the shape
// the classification has to see through.
func declareAWritingComposition(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "rows.add", Collection: "rows", Action: store.ActionInsert,
		Document: map[string]store.Term{"body": {Value: "written by a composed step", Constant: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "rows.page_then_write", Collection: "rows", Action: store.ActionBatch, Limit: 401,
		Steps: []store.Step{
			{Operation: "rows.page", Version: 1},
			{Operation: "rows.add", Version: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

func threePagesOnce(t *testing.T, one *client) {
	t.Helper()
	frame := one.invoke("rows.three_pages", map[string]any{})
	if frame.Type != protocol.Result {
		t.Fatalf("the composed read: %s: %s", frame.Type, frame.Payload)
	}
	result := decode[store.Result](t, frame)
	if result.Count == 0 {
		t.Fatal("the composed read came back with no rows: this round measured an empty database, not a read")
	}
}
