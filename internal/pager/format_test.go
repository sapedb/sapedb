package pager

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/sapedb/sapedb/internal/vfs"
)

// TestOpeningAnUnknownFormatNamesBothNumbersAndLeavesTheFileAlone is ISS-4's
// measurement.
//
// Before this task, ErrFormat had no test anywhere in the repository: a
// repo-wide search for the sentinel in every *_test.go file found nothing —
// grep -rl ErrFormat --include=*_test.go . lists zero files — while the same
// search shape for every sibling error readMeta can also return (ErrNotSapedb,
// ErrChecksum, ErrNoMeta, ErrPageKind, ErrQuota, ErrOutOfRange,
// ErrReadOnlyPage, ErrTruncated) finds at least one file each. That asymmetry
// is the positive control: the search shape does find tests when a test
// exists, so ErrFormat's empty result is a real gap, not a search that never
// worked.
//
// This test builds a file with two committed transactions (so both meta
// pages hold a real, checksummed layout), corrupts only the two-byte Format
// field — offFormat, a separate uint16 at HeaderBytes+8 — in BOTH meta
// copies, and leaves pager.Magic (the 8-byte "SAPEDB\0\x01" at offMagic,
// HeaderBytes) and every other byte untouched. Both copies have to be wrong
// for the refusal to surface at all: loadMeta only returns firstErr when
// BOTH readMeta calls fail (see loadMeta's `firstErr != nil && secondErr !=
// nil` branch) — corrupting only meta page 0 would just make Open silently
// fall back to the still-good meta page 1, which is TestOneDamagedMetaPageIsSurvivable's
// whole point next door and would make this test pass for the wrong reason.
func TestOpeningAnUnknownFormatNamesBothNumbersAndLeavesTheFileAlone(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 20)
	first := writePage(t, pager, 'a')
	if err := pager.Commit(first); err != nil { // writes meta 0
		t.Fatal(err)
	}
	second := writePage(t, pager, 'b')
	if err := pager.Commit(second); err != nil { // writes meta 1
		t.Fatal(err)
	}

	const badFormat = Format + 1

	image := disk.Durable()

	binary.BigEndian.PutUint16(image[offFormat:], badFormat)
	binary.BigEndian.PutUint16(image[PageBytes+offFormat:], badFormat)

	corrupted := append([]byte(nil), image...)

	broken := vfs.NewSim(20, vfs.Faults{})
	broken.Restore(image)

	_, err := Open(broken, 0)
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("opening a file with format %d against a build that reads %d: want ErrFormat, got %v", badFormat, Format, err)
	}

	// The message is the entire value of this refusal: a caller has to be
	// told both what the file says and what this build knows, or "failed"
	// would have done just as well. A test that only checked the error type
	// would pass on a message that said nothing useful.
	want := fmt.Sprintf("sapedb/pager: file format is from another version: file says %d, this build reads %d", badFormat, Format)
	if got := err.Error(); got != want {
		t.Fatalf("message:\n got  %q\n want %q", got, want)
	}

	// A refusal that leaves the file modified is worse than no refusal: the
	// next attempt to read it would be reading damage this build caused,
	// not the original mismatch. Open only ever reads on this path — nothing
	// past loadMeta runs before OpenWith returns the error — so the bytes on
	// disk after the failed Open must be exactly the bytes it was asked to
	// read, corruption and all.
	after := broken.Durable()
	if !bytes.Equal(after, corrupted) {
		t.Fatalf("a refused Open changed the file on disk")
	}
}
