package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// blobs is a collection shaped like the thing this action exists for: a file
// cut into rows, each row carrying its slice as base64 text under "body", and
// the key carrying the order.
//
// The index is here to be refused rather than to be used. A hashRange walks
// key order and nothing else, and a collection with no index at all could not
// measure that.
func blobs() Spec {
	return Spec{
		Name: "blobs",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name:   "by_part",
			Fields: []Field{{Path: "part", Type: TypeNumber, Missing: MissingSkip}},
		}},
	}
}

// madeUpFile is bytes that look nothing like text and repeat nowhere: every
// byte value appears, including 0x00 and 0xFF, which is the pair that makes
// hashing the values "as stored" impossible and this action's decode
// necessary.
func madeUpFile(size int) []byte {
	file := make([]byte, size)
	state := uint32(0x9E3779B9)
	for i := range file {
		state = state*1664525 + 1013904223
		file[i] = byte(state >> 24)
	}
	return file
}

// split writes a file into a collection as base64 rows of the given size, and
// hands back the key of the first row and of the last.
func split(t *testing.T, collection *Collection, file []byte, chunk int) (string, string) {
	t.Helper()

	first, last := "", ""
	for at, part := 0, 0; at < len(file); at, part = at+chunk, part+1 {
		end := at + chunk
		if end > len(file) {
			end = len(file)
		}
		key := fmt.Sprintf("%s/%06d", collection.spec.Name, part)
		if _, err := collection.Put(map[string]any{
			"id":   key,
			"part": float64(part),
			"body": base64.StdEncoding.EncodeToString(file[at:end]),
		}); err != nil {
			t.Fatalf("writing row %d: %v", part, err)
		}
		if first == "" {
			first = key
		}
		last = key
	}
	return first, last
}

// digesting is the declaration under test: the bounds come from the caller,
// and the field, the decode and the limit are written into it.
func digesting(name string, limit int) Operation {
	return Operation{
		Name: name, Collection: "blobs", Action: ActionHashRange,
		Input: []Parameter{
			{Name: "from", Type: TypeString, Required: true},
			{Name: "to", Type: TypeString, Required: true},
		},
		From:   &Endpoint{Terms: []Term{{Arg: "from"}}},
		To:     &Endpoint{Terms: []Term{{Arg: "to"}}},
		Field:  "body",
		Decode: DecodeBase64,
		Limit:  limit,
	}
}

// TestTheDigestOfARangeIsTheSHA256OfTheFileItWasSplitFrom is acceptance
// criterion 1, and it is the one that decides whether the feature is worth
// having at all.
//
// The comparison is against a SHA-256 taken outside this store, of the
// original bytes, before they were ever split or encoded — which is exactly
// what a client holds and exactly what it will compare against. If those two
// numbers can disagree for any reason other than the data being wrong, then
// two correct systems disagree forever and the disagreement looks like
// corruption, which is the failure this ticket was written to prevent.
func TestTheDigestOfARangeIsTheSHA256OfTheFileItWasSplitFrom(t *testing.T) {
	_, store := fresh(t, 7)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}

	// Six megabytes in three thousand and seventy-two rows of two kilobytes,
	// which is the shape the application that asked for this actually has.
	const chunk = 2048
	file := madeUpFile(6 << 20)
	first, last := split(t, collection, file, chunk)
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	rows := len(file) / chunk
	if rows < 3000 {
		t.Fatalf("this fixture is meant to be thousands of rows and is %d", rows)
	}

	declareOp(t, store, digesting("blobs.digest", 5000))
	result := invoke(t, store, "blobs.digest", map[string]any{"from": first, "to": last})

	// Computed here, from the bytes, with nothing of this package involved.
	outside := sha256.Sum256(file)
	if result.Digest != hex.EncodeToString(outside[:]) {
		t.Fatalf("the store answered %s and the file hashes to %s",
			result.Digest, hex.EncodeToString(outside[:]))
	}
	if result.Count != rows {
		t.Errorf("the digest was taken over %d rows, and the file is %d rows", result.Count, rows)
	}
	// Thirty-two bytes, as hex. The whole reason this action can exist before
	// any size ceiling does.
	if len(result.Digest) != 64 {
		t.Errorf("the digest is %d characters", len(result.Digest))
	}
	if len(result.Rows) != 0 {
		t.Errorf("a hashRange handed back %d rows as well", len(result.Rows))
	}
}

// TestTheSameBytesSplitTwoWaysHashTheSame is acceptance criterion 2, and it is
// the assertion that would catch a separator creeping in between the values.
//
// A delimiter of any kind makes the chunk boundaries part of the answer, and
// then how a file was split becomes a fact about the file — which it is not.
// The two collections below hold the same bytes cut at one kilobyte and at
// four, so they agree only if nothing at all goes between the values.
func TestTheSameBytesSplitTwoWaysHashTheSame(t *testing.T) {
	file := madeUpFile(1 << 20)
	outside := sha256.Sum256(file)

	digests := map[int]string{}
	for _, chunk := range []int{1024, 4096} {
		_, store := fresh(t, int64(chunk))
		collection, err := store.Declare(Caller{}, blobs())
		if err != nil {
			t.Fatal(err)
		}
		first, last := split(t, collection, file, chunk)
		if err := store.Commit(); err != nil {
			t.Fatal(err)
		}

		declareOp(t, store, digesting("blobs.digest", 5000))
		result := invoke(t, store, "blobs.digest", map[string]any{"from": first, "to": last})

		// Both are also checked against the file itself, so that "they agree"
		// cannot be satisfied by two runs of the same wrong thing.
		if result.Digest != hex.EncodeToString(outside[:]) {
			t.Fatalf("at %d-byte rows the digest is %s, and the file hashes to %s",
				chunk, result.Digest, hex.EncodeToString(outside[:]))
		}
		digests[chunk] = result.Digest

		// And the row counts really are different, so the two runs are not
		// accidentally the same walk.
		if want := len(file) / chunk; result.Count != want {
			t.Fatalf("at %d-byte rows the walk read %d rows, want %d", chunk, result.Count, want)
		}
	}

	if digests[1024] != digests[4096] {
		t.Fatalf("1 KB rows hash to %s and 4 KB rows to %s — something goes between the values",
			digests[1024], digests[4096])
	}
}

// TestARowThatCannotGoIntoTheDigestRefusesAndNamesItsKey is acceptance
// criterion 3.
//
// The precedent is the rollup, which refuses a document it cannot add rather
// than skipping it, because a total that silently skips is a total nobody can
// trust. A digest is worse: it is thirty-two bytes that look exactly as
// authoritative with a row missing as without. So all three ways a row can
// fail to be text this store can hash stop the walk, and each of them says
// which row.
func TestARowThatCannotGoIntoTheDigestRefusesAndNamesItsKey(t *testing.T) {
	for _, one := range []struct {
		what string
		row  map[string]any
	}{
		{"missing", map[string]any{"id": "blobs/000002", "part": 2.0}},
		{"wrongly typed", map[string]any{"id": "blobs/000002", "part": 2.0, "body": 42.0}},
		{"not base64", map[string]any{"id": "blobs/000002", "part": 2.0, "body": "not base64!"}},
	} {
		t.Run(one.what, func(t *testing.T) {
			_, store := fresh(t, 11)
			collection, err := store.Declare(Caller{}, blobs())
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []int{0, 1, 3, 4} {
				if _, err := collection.Put(map[string]any{
					"id":   fmt.Sprintf("blobs/%06d", part),
					"part": float64(part),
					"body": base64.StdEncoding.EncodeToString([]byte{byte(part)}),
				}); err != nil {
					t.Fatal(err)
				}
			}
			declareOp(t, store, digesting("blobs.digest", 100))

			// The control: without the bad row in it, this same range hashes.
			// A red line on its own cannot tell "the guard fired" from
			// "nothing here works".
			good, err := store.Invoke(Caller{}, "blobs.digest", 0,
				map[string]any{"from": "blobs/000000", "to": "blobs/000004"})
			if err != nil || good.Digest == "" {
				t.Fatalf("the range without the bad row: %v %+v", err, good)
			}

			if _, err := collection.Put(one.row); err != nil {
				t.Fatal(err)
			}
			if err := store.Commit(); err != nil {
				t.Fatal(err)
			}

			result, err := store.Invoke(Caller{}, "blobs.digest", 0,
				map[string]any{"from": "blobs/000000", "to": "blobs/000004"})
			if !errors.Is(err, ErrDigest) {
				t.Fatalf("a %s field answered %v, want ErrDigest", one.what, err)
			}
			if !strings.Contains(err.Error(), "blobs/000002") {
				t.Errorf("the refusal does not name the key: %v", err)
			}
			if result.Digest != "" {
				t.Errorf("a refused digest came back anyway: %s", result.Digest)
			}
		})
	}
}

// TestAHashRangeAtItsCeilingRefusesRatherThanHashingAPrefix is acceptance
// criterion 4, and the reason it is a refusal rather than a Truncated flag.
//
// A scan that stops at its limit hands back fewer rows, and a caller can see
// that it did. A digest of part of a range is the same thirty-two bytes in the
// same field as a digest of all of it, so a caller comparing it against their
// file sees a mismatch and goes looking for corruption that is not there.
// There is no shorter honest answer, so there is no answer.
func TestAHashRangeAtItsCeilingRefusesRatherThanHashingAPrefix(t *testing.T) {
	_, store := fresh(t, 13)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}
	file := madeUpFile(10 * 64)
	first, last := split(t, collection, file, 64)
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Exactly at the ceiling is not over it: ten rows and a limit of ten is
	// the answer, not the refusal. This pins the boundary, which is the half
	// of the rule an off-by-one would quietly move.
	declareOp(t, store, digesting("blobs.exact", 10))
	whole := invoke(t, store, "blobs.exact", map[string]any{"from": first, "to": last})
	outside := sha256.Sum256(file)
	if whole.Digest != hex.EncodeToString(outside[:]) {
		t.Fatalf("ten rows under a limit of ten answered %s", whole.Digest)
	}
	if whole.Count != 10 {
		t.Fatalf("ten rows under a limit of ten read %d", whole.Count)
	}

	// And one row short of the range is a refusal, not the digest of the nine
	// it managed.
	declareOp(t, store, digesting("blobs.short", 9))
	prefix := sha256.Sum256(file[:9*64])
	result, err := store.Invoke(Caller{}, "blobs.short", 0,
		map[string]any{"from": first, "to": last})
	if !errors.Is(err, ErrCeiling) {
		t.Fatalf("a walk past its limit answered %v, want ErrCeiling", err)
	}
	if result.Digest != "" {
		t.Fatalf("a refused walk handed back %s", result.Digest)
	}
	// Said as its own assertion because it is the exact failure this rule
	// exists to prevent, and a reader of this test should not have to work out
	// that the empty string above covers it.
	if result.Digest == hex.EncodeToString(prefix[:]) {
		t.Fatalf("the digest of the first nine rows was handed back as though it were the range")
	}
	if !strings.Contains(err.Error(), "9") {
		t.Errorf("the refusal does not say what the limit was: %v", err)
	}
}

// TestTheCeilingIsCountedPerRunAndNotPerPartition is the W7 shape, measured.
//
// The gap task 0071 recorded is a host that checks every individual call and
// never the total: an operation declaring `limit 50` served fifty million rows
// because nobody added them up. A partitioned collection is where that gap
// could open here without anybody writing a loop, because one run of this
// operation is several walks of several files. Six rows over two partitions
// under a limit of five refuses; a counter that started again at each file
// would see three and three and be delighted.
func TestTheCeilingIsCountedPerRunAndNotPerPartition(t *testing.T) {
	_, _, store := partitioned(t, 300)
	entries := entriesByMonth(t, store, 0)

	written := 0
	for _, month := range []time.Month{time.January, time.February} {
		atMonth(t, store, time.Date(2026, month, 15, 0, 0, 0, 0, time.UTC))
		for i := 0; i < 3; i++ {
			if _, err := entries.Put(map[string]any{
				"account": "cash",
				"body":    base64.StdEncoding.EncodeToString([]byte{byte(written)}),
			}); err != nil {
				t.Fatal(err)
			}
			written++
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if names := store.Parts(); len(names) != 2 {
		t.Fatalf("this test needs two partitions and has %v", names)
	}

	across := Operation{
		Name: "entries.digest", Collection: "entries", Action: ActionHashRange,
		Field: "body", Decode: DecodeBase64, Limit: 5,
	}
	declareOp(t, store, across)

	if _, err := store.Invoke(Caller{}, "entries.digest", 0, nil); !errors.Is(err, ErrCeiling) {
		t.Fatalf("six rows over two partitions under a limit of five answered %v, want ErrCeiling — the ceiling is being counted per file rather than per run", err)
	}

	// The control: a limit that covers the total goes through, so the refusal
	// above is the ceiling and not the partitioning.
	wide := across
	wide.Limit = 6
	declareOp(t, store, wide)
	result := invoke(t, store, "entries.digest", nil)
	if result.Count != 6 {
		t.Fatalf("a limit of six read %d rows of six", result.Count)
	}
}

// TestWhatAHashRangeMayAndMayNotDeclare is every refusal this action adds,
// with the declaration that passes sitting next to each one.
//
// One table rather than one test per rule, because each row is the same shape:
// a field changed on a declaration that is otherwise known to work, so a row
// that goes green for the wrong reason is visible against its neighbours.
func TestWhatAHashRangeMayAndMayNotDeclare(t *testing.T) {
	_, store := fresh(t, 17)
	if _, err := store.Declare(Caller{}, blobs()); err != nil {
		t.Fatal(err)
	}

	// The control, first and on its own: this declaration is accepted.
	if _, err := store.DeclareOperation(Caller{}, digesting("blobs.ok", 100)); err != nil {
		t.Fatalf("the declaration these are all variations of was itself refused: %v", err)
	}
	// And so is one with no decode at all, which is the other legal shape.
	plain := digesting("blobs.plain", 100)
	plain.Decode = DecodeNone
	if _, err := store.DeclareOperation(Caller{}, plain); err != nil {
		t.Fatalf("a hashRange with no decode was refused: %v", err)
	}

	for _, one := range []struct {
		what   string
		change func(*Operation)
		says   string
	}{
		{"an index instead of key order", func(o *Operation) { o.Index = "by_part" }, "key order"},
		{"no field", func(o *Operation) { o.Field = "" }, "which field"},
		{"an encoding this store does not have", func(o *Operation) { o.Decode = "hex" }, "not \"hex\""},
		{"no limit", func(o *Operation) { o.Limit = 0 }, "how far it walks"},
		{"a negative limit", func(o *Operation) { o.Limit = -1 }, "how far it walks"},
		{"a direction", func(o *Operation) {
			o.Direction = &Term{Value: DirectionReverse, Constant: true}
		}, "different digest of the same bytes"},
		{"a projection", func(o *Operation) { o.Projection = []string{"body"} }, "rather than rows"},
	} {
		t.Run(one.what, func(t *testing.T) {
			operation := digesting("blobs.varied", 100)
			one.change(&operation)
			_, err := store.DeclareOperation(Caller{}, operation)
			if !errors.Is(err, ErrDeclaration) {
				t.Fatalf("%s was accepted (%v)", one.what, err)
			}
			if !strings.Contains(err.Error(), one.says) {
				t.Errorf("the refusal for %s reads %q, and does not say %q", one.what, err, one.says)
			}
		})
	}
}

// TestOnlyAHashRangeMayWriteDownAFieldOrADecode: a word nothing reads is how a
// caller ends up believing a promise nobody made, which is the same rule that
// already refuses a direction on a count.
//
// It matters most for "decode". A scan declaring `"decode": "base64"` looks
// exactly like an instruction to hand the rows back decoded, and a store that
// accepted it silently would be handing back base64 to somebody who had
// written down that they did not want it.
func TestOnlyAHashRangeMayWriteDownAFieldOrADecode(t *testing.T) {
	_, store := fresh(t, 19)
	if _, err := store.Declare(Caller{}, blobs()); err != nil {
		t.Fatal(err)
	}

	base := Operation{
		Name: "blobs.page", Collection: "blobs", Action: ActionScan, Limit: 10,
	}
	if _, err := store.DeclareOperation(Caller{}, base); err != nil {
		t.Fatalf("the scan these vary from was itself refused: %v", err)
	}

	for _, one := range []struct {
		what   string
		change func(*Operation)
	}{
		{"a field on a scan", func(o *Operation) { o.Field = "body" }},
		{"a decode on a scan", func(o *Operation) { o.Decode = DecodeBase64 }},
	} {
		t.Run(one.what, func(t *testing.T) {
			operation := base
			operation.Name = "blobs.varied"
			one.change(&operation)
			if _, err := store.DeclareOperation(Caller{}, operation); !errors.Is(err, ErrDeclaration) {
				t.Fatalf("%s was accepted (%v)", one.what, err)
			}
		})
	}
}

// TestAHashRangeCannotBeAStepOfSomethingElse: a composed answer holds one
// Digest, so a second hashRange step would overwrite the first in place — the
// caller would get the wrong range's hash in the right field, which is worse
// than the count's failure of having its answer dropped where nobody can read
// it.
func TestAHashRangeCannotBeAStepOfSomethingElse(t *testing.T) {
	_, store := fresh(t, 23)
	if _, err := store.Declare(Caller{}, blobs()); err != nil {
		t.Fatal(err)
	}
	inner := declareOp(t, store, digesting("blobs.digest", 100))

	composed := Operation{
		Name: "blobs.check", Collection: "blobs", Action: ActionBatch, Limit: 10,
		Input: []Parameter{
			{Name: "from", Type: TypeString, Required: true},
			{Name: "to", Type: TypeString, Required: true},
		},
		Steps: []Step{{
			Name: "digest", Operation: inner.Name, Version: inner.Version,
			With: map[string]Term{"from": {Arg: "from"}, "to": {Arg: "to"}},
		}},
	}
	_, err := store.DeclareOperation(Caller{}, composed)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a hashRange as a step was accepted (%v)", err)
	}
	if !strings.Contains(err.Error(), "one digest") {
		t.Errorf("the refusal reads %q", err)
	}
}

// TestTheEnvelopeOfAHashRangeSaysHowFarItWalks: the cost envelope is what a
// caller reads before running an operation somebody else wrote, and for this
// action it is the only place the cost appears at all — the answer is
// thirty-two bytes whether the walk was four rows or four million.
//
// ceiling() reports 1 for a hashRange, which is what it would contribute to an
// enclosing batch's sum. Printing that here would tell a reader that an
// operation which may read five thousand rows costs one.
func TestTheEnvelopeOfAHashRangeSaysHowFarItWalks(t *testing.T) {
	_, store := fresh(t, 29)
	if _, err := store.Declare(Caller{}, blobs()); err != nil {
		t.Fatal(err)
	}
	declareOp(t, store, digesting("blobs.digest", 5000))

	envelope, err := store.Envelope("blobs.digest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Limit != 5000 {
		t.Errorf("the envelope says this costs %d, and the declaration says 5000", envelope.Limit)
	}
	if envelope.WholeDocument {
		t.Error("the envelope says the whole document escapes from an operation that hands back a digest")
	}
	if len(envelope.Projection) != 0 {
		t.Errorf("the envelope says %v escapes", envelope.Projection)
	}
	if len(envelope.Indexes) != 1 || envelope.Indexes[0] != ClusteredIndex {
		t.Errorf("the envelope says it walks %v", envelope.Indexes)
	}
}

// TestAHashRangeWithNoDecodeHashesTheTextAsStored is the other half of the
// decode enumeration, and the reason the enumeration is needed.
//
// The same rows, read the two ways, give two different and both correct
// answers: the hash of the base64 transcript, and the hash of the bytes it
// transcribes. They are not interchangeable and the declaration is the only
// place that can say which one was meant.
func TestAHashRangeWithNoDecodeHashesTheTextAsStored(t *testing.T) {
	_, store := fresh(t, 31)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}
	file := madeUpFile(3 * 64)
	first, last := split(t, collection, file, 64)
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	plain := digesting("blobs.asstored", 100)
	plain.Decode = DecodeNone
	declareOp(t, store, plain)
	declareOp(t, store, digesting("blobs.decoded", 100))

	transcript := ""
	for at := 0; at < len(file); at += 64 {
		transcript += base64.StdEncoding.EncodeToString(file[at : at+64])
	}
	wantStored := sha256.Sum256([]byte(transcript))
	wantBytes := sha256.Sum256(file)

	stored := invoke(t, store, "blobs.asstored", map[string]any{"from": first, "to": last})
	decoded := invoke(t, store, "blobs.decoded", map[string]any{"from": first, "to": last})

	if stored.Digest != hex.EncodeToString(wantStored[:]) {
		t.Errorf("hashing as stored answered %s, want the hash of the transcript %s",
			stored.Digest, hex.EncodeToString(wantStored[:]))
	}
	if decoded.Digest != hex.EncodeToString(wantBytes[:]) {
		t.Errorf("hashing decoded answered %s, want the hash of the file %s",
			decoded.Digest, hex.EncodeToString(wantBytes[:]))
	}
	// And they really are different, which is the whole reason for the word.
	if stored.Digest == decoded.Digest {
		t.Fatal("the two decodes gave the same answer, so this fixture proves nothing")
	}
}
