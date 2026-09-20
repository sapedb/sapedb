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

// TestOpeningAWrongPageSizeNamesBothNumbersAndLeavesTheFileAlone covers
// readMeta's second ErrFormat branch — the one guarding offPageSize rather
// than offFormat — which is a different message and, until now, an untested
// one: the format-number test above proves errors.Is(err, ErrFormat) and the
// wording for a bad format number, but every call site wraps the same
// sentinel, so a test that only checked errors.Is could not tell this branch
// apart from that one. It would pass even if this branch's message text were
// wrong, or if the two branches were swapped by a refactor.
//
// The corruption is offPageSize, a separate uint32 at HeaderBytes+10 — not
// offFormat (HeaderBytes+8) and not pager.Magic (HeaderBytes) — in both meta
// copies, so loadMeta cannot fall back to the untouched one.
func TestOpeningAWrongPageSizeNamesBothNumbersAndLeavesTheFileAlone(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 90)
	first := writePage(t, pager, 'a')
	if err := pager.Commit(first); err != nil { // writes meta 0
		t.Fatal(err)
	}
	second := writePage(t, pager, 'b')
	if err := pager.Commit(second); err != nil { // writes meta 1
		t.Fatal(err)
	}

	const badSize = PageBytes + 1

	image := disk.Durable()

	binary.BigEndian.PutUint32(image[offPageSize:], badSize)
	binary.BigEndian.PutUint32(image[PageBytes+offPageSize:], badSize)

	corrupted := append([]byte(nil), image...)

	broken := vfs.NewSim(90, vfs.Faults{})
	broken.Restore(image)

	_, err := Open(broken, 0)
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("opening a file with page size %d against a build that uses %d: want ErrFormat, got %v", badSize, PageBytes, err)
	}

	want := fmt.Sprintf("sapedb/pager: file format is from another version: pages are %d bytes, this build uses %d", badSize, PageBytes)
	if got := err.Error(); got != want {
		t.Fatalf("message:\n got  %q\n want %q", got, want)
	}

	after := broken.Durable()
	if !bytes.Equal(after, corrupted) {
		t.Fatalf("a refused Open changed the file on disk")
	}
}

// TestOpeningALedStoreWithAnUnknownFormatNamesBothNumbersAndLeavesTheFileAlone
// covers readLabel's format check in led.go, which is its own copy of the
// same test made against pager.go's meta pages, not a call into readMeta.
// A led store keeps no redundant copy of its label the way a leader keeps
// two meta pages — page 0 is written once by CreateLed and never again — so
// one corrupted field is already both copies there is.
//
// This is built as a real encrypted led store (CreateLed with a key, one
// flushed page) and reopened with the correct key, so the failure exercises
// the path an operator actually hits: readLabel's format check runs and
// fails before OpenLed ever gets to ask whether the key was right, so using
// the right key here proves the refusal is about the format field, not a
// side effect of the key path.
func TestOpeningALedStoreWithAnUnknownFormatNamesBothNumbersAndLeavesTheFileAlone(t *testing.T) {
	key := bytes.Repeat([]byte{0x5A}, 32)
	disk := vfs.NewSim(91, vfs.Faults{})
	sealed, err := CreateLed(disk, Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	secret := filled(t, sealed, 0x99)
	at, err := sealed.Flush(secret)
	if err != nil {
		t.Fatal(err)
	}

	const badFormat = Format + 1

	image := disk.Durable()
	binary.BigEndian.PutUint16(image[offFormat:], badFormat)

	corrupted := append([]byte(nil), image...)

	broken := vfs.NewSim(91, vfs.Faults{})
	broken.Restore(image)

	_, err = OpenLed(broken, at, Options{Key: key})
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("opening a led store with format %d against a build that reads %d: want ErrFormat, got %v", badFormat, Format, err)
	}

	want := fmt.Sprintf("sapedb/pager: file format is from another version: file says %d, this build reads %d", badFormat, Format)
	if got := err.Error(); got != want {
		t.Fatalf("message:\n got  %q\n want %q", got, want)
	}

	after := broken.Durable()
	if !bytes.Equal(after, corrupted) {
		t.Fatalf("a refused OpenLed changed the file on disk")
	}
}

// TestOpeningALedStoreWithAWrongPageSizeNamesBothNumbersAndLeavesTheFileAlone
// is the page-size half of readLabel's checks, built the same way and for
// the same reason as the format-number one above: a real encrypted led
// store, reopened with the correct key, so the refusal is provably about
// the page-size field rather than the key.
func TestOpeningALedStoreWithAWrongPageSizeNamesBothNumbersAndLeavesTheFileAlone(t *testing.T) {
	key := bytes.Repeat([]byte{0x5A}, 32)
	disk := vfs.NewSim(92, vfs.Faults{})
	sealed, err := CreateLed(disk, Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	secret := filled(t, sealed, 0x99)
	at, err := sealed.Flush(secret)
	if err != nil {
		t.Fatal(err)
	}

	const badSize = PageBytes + 1

	image := disk.Durable()
	binary.BigEndian.PutUint32(image[offPageSize:], badSize)

	corrupted := append([]byte(nil), image...)

	broken := vfs.NewSim(92, vfs.Faults{})
	broken.Restore(image)

	_, err = OpenLed(broken, at, Options{Key: key})
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("opening a led store with page size %d against a build that uses %d: want ErrFormat, got %v", badSize, PageBytes, err)
	}

	want := fmt.Sprintf("sapedb/pager: file format is from another version: pages are %d bytes, this build uses %d", badSize, PageBytes)
	if got := err.Error(); got != want {
		t.Fatalf("message:\n got  %q\n want %q", got, want)
	}

	after := broken.Durable()
	if !bytes.Equal(after, corrupted) {
		t.Fatalf("a refused OpenLed changed the file on disk")
	}
}
