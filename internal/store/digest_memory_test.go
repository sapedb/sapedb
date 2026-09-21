package store

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// TestTheMemoryAHashRangeUsesDoesNotGrowWithTheRange is acceptance criterion
// 5, and it is the assertion that the joined value never exists anywhere.
//
// It is measured against a database in a real file rather than the simulated
// disk the rest of this package uses, because the simulated one holds every
// page in this process's heap: a measurement taken through it would be
// measuring the fixture, and would stay green over an implementation that
// concatenated the whole range before hashing it. The pager has no page cache
// — every read is a ReadAt — so what the heap holds during the walk is what
// this walk is holding.
//
// The cap below is an absolute number rather than a ratio between two runs on
// purpose. A ratio is satisfied by an implementation that holds a constant
// fraction of the range, and it needs two fixtures built to say anything at
// all; a walk that holds one row at a time out of thirty-two megabytes should
// not move the live heap by more than single-digit megabytes, and if it ever
// does, the interesting question is not by how much.
func TestTheMemoryAHashRangeUsesDoesNotGrowWithTheRange(t *testing.T) {
	if testing.Short() {
		t.Skip("this one builds a thirty-two megabyte fixture on disk")
	}

	const (
		chunk = 4096
		// Generous, and still nowhere near the range. A walk that holds the
		// range, or any fixed fraction of it worth worrying about, cannot fit
		// under this. It does NOT move when the range does, which is the
		// whole claim: see rows below.
		allowed = 12 << 20
	)

	// Eight thousand rows is a range of thirty-two megabytes and a test that
	// runs in under two seconds, which is what belongs in a suite everybody
	// runs. SAPEDB_HASH_ROWS raises it so the same measurement can be taken
	// at a size far past what the machine could hold — usually next to a
	// GOMEMLIMIT small enough that holding the range is not an option — and
	// the number it prints is read the same way, against the same cap.
	rows := 8192
	if given := os.Getenv("SAPEDB_HASH_ROWS"); given != "" {
		asked, err := strconv.Atoi(given)
		if err != nil || asked < 1 {
			t.Fatalf("SAPEDB_HASH_ROWS is %q, which is not a number of rows", given)
		}
		rows = asked
	}
	// What the walk reads, decoded. The rows on disk are larger, because
	// base64 of a chunk is four thirds of it.
	ranged := chunk * rows

	file, err := vfs.OpenFile(filepath.Join(t.TempDir(), "sapedb"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	pages, err := pager.Create(file, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}

	// One chunk, reused: the fixture must not be the thing that proves the
	// point. Every row carries the same bytes, so building this costs one
	// chunk of memory too.
	body := base64.StdEncoding.EncodeToString(madeUpFile(chunk))
	for part := 0; part < rows; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"part": float64(part),
			"body": body,
		}); err != nil {
			t.Fatal(err)
		}
		// Committed in batches so that the WRITE side is not what runs out of
		// memory first. This test is about the read.
		if part%512 == 511 {
			if err := store.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	declareOp(t, store, digesting("blobs.digest", rows))

	// The baseline is taken after the fixture is built and collected, so what
	// is measured below is the walk and not what writing it left behind.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Sampled from outside rather than counted from inside, because what has
	// to stay small is the live heap at its worst moment, and the walk cannot
	// see its own worst moment.
	var peak, stop atomic.Uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		var at runtime.MemStats
		for stop.Load() == 0 {
			runtime.ReadMemStats(&at)
			if at.HeapAlloc > peak.Load() {
				peak.Store(at.HeapAlloc)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result := invoke(t, store, "blobs.digest", map[string]any{
		"from": "blobs/000000",
		"to":   fmt.Sprintf("blobs/%06d", rows-1),
	})

	stop.Store(1)
	<-done

	if result.Count != rows {
		t.Fatalf("the walk read %d rows of %d, so this measured the wrong thing", result.Count, rows)
	}
	if peak.Load() == 0 {
		t.Fatal("the sampler never read the heap, so there is no measurement here")
	}

	grew := int64(peak.Load()) - int64(before.HeapAlloc)
	if grew < 0 {
		grew = 0
	}
	t.Logf("range %d MiB over %d rows: heap %d KiB before, peak %d KiB, grew %d KiB",
		ranged>>20, rows, before.HeapAlloc>>10, peak.Load()>>10, grew>>10)

	if grew > allowed {
		t.Fatalf("hashing %d MiB grew the live heap by %d MiB, and one row at a time should cost single-digit megabytes — something is holding the range",
			ranged>>20, grew>>20)
	}
}
