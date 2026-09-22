package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// TestTheBudgetCountsExactlyWhatTheAnswerWillCarry is the assertion every
// other one here rests on, and it is a byte-for-byte comparison rather than a
// ratio.
//
// A budget whose arithmetic is a guess is not a budget, it is a hope with a
// number next to it. What spend() claims to count is what encoding/json
// writes for the rows array minus its two brackets, and the only way to say
// that is true is to marshal the array and subtract them.
func TestTheBudgetCountsExactlyWhatTheAnswerWillCarry(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "body": "one"},
		{"id": "b", "body": "two", "extra": []any{1.0, 2.0, 3.0}},
		{"id": "c", "body": ""},
	}

	room := &budget{most: 1 << 20}
	for _, row := range rows {
		if err := room.spend("counting", row); err != nil {
			t.Fatalf("a kilobyte of rows did not fit in a megabyte: %v", err)
		}
	}

	written, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	// The array less one byte. `[` + row + `,` + row + `]` is one comma short
	// of a comma per row, and the budget charges a comma per row — so what it
	// counts is the whole array bar its closing bracket, which is the
	// arithmetic budget.spend's own doc claims and this is the check on it.
	want := len(written) - 1
	if room.used != want {
		t.Fatalf("the budget counted %d bytes for rows encoding/json writes as %d", room.used, want)
	}
	if room.rows != len(rows) {
		t.Fatalf("the budget counted %d rows of %d", room.rows, len(rows))
	}
}

// TestAReadIsRefusedAtTheByteTheBudgetRunsOut pins the boundary rather than
// the refusal.
//
// A test that only shows an enormous read being refused is satisfied by a
// server that refuses every read, and a test that only shows a small one
// answering is satisfied by one that refuses nothing. What has to be true is
// that the line falls where the budget says it does, so this sets the budget
// to the exact number of bytes a known number of rows costs and measures the
// row on either side of it.
func TestAReadIsRefusedAtTheByteTheBudgetRunsOut(t *testing.T) {
	const fits = 6

	_, store := fresh(t, 9)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}

	body := base64.StdEncoding.EncodeToString(madeUpFile(64))
	for part := 0; part < fits+4; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"part": float64(part),
			"body": body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Every row is the same shape and the same length, so the budget an exact
	// number of them costs is arithmetic and not a measurement of this
	// particular walk.
	one, err := json.Marshal(map[string]any{
		"id": "blobs/000000", "part": float64(0), "body": body,
	})
	if err != nil {
		t.Fatal(err)
	}
	each := len(one) + 1
	exactly := each * fits

	// A declaration whose own limit is exactly the number of rows the budget
	// has room for, over a collection that holds more. The walk therefore
	// stops at the declaration's limit having spent the budget down to its
	// last byte, and the row after it is the one the budget would refuse.
	declareOp(t, store, Operation{
		Name: "blobs.page", Collection: "blobs", Action: ActionScan, Limit: fits,
	})
	declareOp(t, store, Operation{
		Name: "blobs.all", Collection: "blobs", Action: ActionScan, Limit: fits + 4,
	})

	// Exactly enough. The last row lands on the budget and does not cross it,
	// which is the "a read that answers today must still answer" half.
	store.Budget(exactly)
	answered, err := store.Invoke(Caller{}, "blobs.page", 0, nil)
	if err != nil {
		t.Fatalf("a budget of exactly %d bytes refused the %d rows that cost exactly that: %v", exactly, fits, err)
	}
	if len(answered.Rows) != fits {
		t.Fatalf("the walk came back with %d rows, not the %d the budget had room for", len(answered.Rows), fits)
	}
	if !answered.Truncated {
		t.Fatal("the collection holds more rows than the declaration's limit, so this answer is partial and must say so")
	}

	// One byte less. The same walk, the same rows, and the last one no longer
	// fits — so this is the boundary and not a size.
	store.Budget(exactly - 1)
	_, refused := store.Invoke(Caller{}, "blobs.page", 0, nil)
	if !errors.Is(refused, ErrTooLarge) {
		t.Fatalf("a budget one byte short of %d rows answered anyway (%v)", fits, refused)
	}
	if errors.Is(refused, ErrCeiling) {
		t.Fatalf("the refusal reads as a ceiling, which is the declaration's limit and not the server's budget: %v", refused)
	}

	// And the budget is not a ceiling on the DECLARATION: with no budget at
	// all the wider call answers with everything in the collection.
	store.Budget(0)
	whole, err := store.Invoke(Caller{}, "blobs.all", 0, nil)
	if err != nil {
		t.Fatalf("with no budget the wider call was still refused: %v", err)
	}
	if len(whole.Rows) != fits+4 {
		t.Fatalf("with no budget the walk came back with %d rows, not %d", len(whole.Rows), fits+4)
	}
}

// TestABudgetedReadAlsoBoundsAGetAndAComposedCall is the answer to "which
// rows does it charge", measured rather than asserted in a comment.
//
// A budget that covered the scan and nothing else would be a budget with two
// ways around it: a get of a single very large document — there is no maximum
// size for a stored value — and a batch whose steps each call an operation
// that reads.
func TestABudgetedReadAlsoBoundsAGetAndAComposedCall(t *testing.T) {
	_, store := fresh(t, 11)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}

	body := base64.StdEncoding.EncodeToString(madeUpFile(4096))
	for part := 0; part < 4; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"part": float64(part),
			"body": body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	declareOp(t, store, Operation{
		Name: "blobs.one", Collection: "blobs", Action: ActionGet,
		Key: &Term{Value: "blobs/000000", Constant: true},
	})
	declareOp(t, store, Operation{
		Name: "blobs.page", Collection: "blobs", Action: ActionScan, Limit: 4,
	})
	declareOp(t, store, Operation{
		Name: "blobs.twice", Collection: "blobs", Action: ActionBatch, Limit: 8,
		Steps: []Step{
			{Operation: "blobs.page", Version: 1},
			{Operation: "blobs.page", Version: 1},
		},
	})

	// A get of one row, refused by a budget smaller than that row.
	store.Budget(64)
	if _, err := store.Invoke(Caller{}, "blobs.one", 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a get of a row larger than the whole budget was not refused (%v)", err)
	}
	store.Budget(1 << 20)
	if got := invoke(t, store, "blobs.one", nil); len(got.Rows) != 1 {
		t.Fatalf("with room the same get came back with %d rows", len(got.Rows))
	}

	// A batch spends ONE budget across its steps, not one per step. Four rows
	// fit; the same four twice do not.
	page := invoke(t, store, "blobs.page", nil)
	if len(page.Rows) != 4 {
		t.Fatalf("the page came back with %d rows, not 4", len(page.Rows))
	}
	written, err := json.Marshal(page.Rows)
	if err != nil {
		t.Fatal(err)
	}
	// What the budget charges for one page: the array bar its closing bracket
	// — see TestTheBudgetCountsExactlyWhatTheAnswerWillCarry. Two pages
	// therefore cost exactly twice this, which is what makes the two
	// assertions below a boundary rather than two sizes.
	onePage := len(written) - 1

	// A batch refuses to start on top of anything half-written, and the
	// declarations above are writes.
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	store.Budget(onePage + onePage/2)
	if _, err := store.Invoke(Caller{}, "blobs.twice", 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("two pages ran inside a budget with room for one and a half (%v)", err)
	}
	store.Budget(onePage * 2)
	both, err := store.Invoke(Caller{}, "blobs.twice", 0, nil)
	if err != nil {
		t.Fatalf("two pages were refused by a budget with room for exactly two: %v", err)
	}
	if len(both.Rows) != 8 {
		t.Fatalf("the batch came back with %d rows, not 8", len(both.Rows))
	}
}

// TestTheMemoryARefusedReadUsesDoesNotReachTheAnswerItRefuses is ISS-37's own
// acceptance criterion, and the reason the ticket exists.
//
// The measured defect was not that an oversized read was allowed — it was
// refused, every time, by protocol.Encode. It was that the refusal came from
// a check on len() of a byte slice that already existed, so the server had to
// read every row and marshal all of them before it could say no. Eighty-eight
// megabytes of rows cost three hundred and fifty-two megabytes of process and
// an OOM kill on a 512 MiB machine.
//
// So the assertion is not "the read is refused". It is "the heap does not
// reach the size of the answer being refused", and it needs the unbudgeted
// run alongside it — without that control this is satisfied by a fixture too
// small to have been a problem in the first place.
//
// Sampled from outside, the way this package already measures hashRange's
// walk, and read the same way: a peak sampled about once a millisecond is a
// lower bound on the true peak, which only makes the assertion harder to
// pass, never easier.
func TestTheMemoryARefusedReadUsesDoesNotReachTheAnswerItRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("this one builds a fifty megabyte fixture on disk")
	}

	const (
		chunk = 1536  // 2048 bytes once base64 makes text of it: ISS-37's row
		rows  = 24576 // about fifty megabytes of answer
		// The budget under test, which is the number the server really uses:
		// protocol.MaxPayload. Written out rather than imported so that this
		// package does not take a dependency on the wire to measure the store.
		cap16 = 16 << 20
	)

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

	body := base64.StdEncoding.EncodeToString(madeUpFile(chunk))
	for part := 0; part < rows; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"part": float64(part),
			"body": body,
		}); err != nil {
			t.Fatal(err)
		}
		if part%512 == 511 {
			if err := store.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// The declaration ISS-37 describes: a limit written down once, far above
	// what the collection held at the time, invoked again after it grew.
	declareOp(t, store, Operation{
		Name: "blobs.everything", Collection: "blobs", Action: ActionScan, Limit: 1_000_000,
	})

	// The control, and it has to run first: what the answer actually comes to
	// when nothing stops it being built. Without this number the assertion
	// below is a claim about a fixture nobody has measured.
	store.Budget(0)
	unbudgeted, _ := peakOf(t, func() {
		whole, err := store.Invoke(Caller{}, "blobs.everything", 0, nil)
		if err != nil {
			t.Errorf("the unbudgeted control was refused: %v", err)
			return
		}
		sent, err := json.Marshal(whole)
		if err != nil {
			t.Errorf("marshalling the control: %v", err)
			return
		}
		t.Logf("unbudgeted: %d rows, %d MiB of payload", len(whole.Rows), len(sent)>>20)
	})
	if t.Failed() {
		return
	}
	runtime.GC()

	// The same call, with the budget the server sets.
	store.Budget(cap16)
	budgeted, _ := peakOf(t, func() {
		_, err := store.Invoke(Caller{}, "blobs.everything", 0, nil)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("a fifty megabyte answer under a sixteen megabyte budget was not refused (%v)", err)
		}
	})
	if t.Failed() {
		return
	}

	t.Logf("answer about %d MiB: heap grew %d MiB with no budget, %d MiB with the %d MiB budget",
		(chunk*4/3*rows)>>20, unbudgeted>>20, budgeted>>20, cap16>>20)
	// The control must really have built the thing, or nothing below means
	// anything.
	if unbudgeted < 32<<20 {
		t.Fatalf("the unbudgeted run only grew the heap by %d MiB, so this fixture never demonstrated the defect", unbudgeted>>20)
	}

	// The claim. The budget is sixteen megabytes; the rows that fit under it
	// are live while the refusal is decided, and the transient bytes of
	// measuring each row are not. Double the budget is generous and still
	// nowhere near the answer.
	if budgeted > 2*cap16 {
		t.Fatalf("a refused read grew the heap by %d MiB against a %d MiB budget: something is still building the whole answer",
			budgeted>>20, cap16>>20)
	}
	// And it is meaningfully less than the unbudgeted run, which is the
	// sentence the ticket asks for: the server no longer builds what it
	// cannot send.
	if budgeted >= unbudgeted {
		t.Fatalf("the budgeted run grew the heap by %d MiB and the unbudgeted one by %d MiB, so the budget bought nothing",
			budgeted>>20, unbudgeted>>20)
	}
}

// TestARollupReadIsChargedAgainstTheBudgetToo covers the third place rows are
// accumulated, which is neither the scan ISS-37 measured nor the get.
//
// A rollup's rows are small — a group and its totals — so this is not where
// an answer runs away either. It is here because a budget with an action
// missing from it is a budget somebody has to remember the exception to, and
// the exception is always discovered by the person it costs.
func TestARollupReadIsChargedAgainstTheBudgetToo(t *testing.T) {
	_, store := fresh(t, 17)
	lines := takings(t, store)

	for i, account := range []string{"cash", "card", "credit", "voucher"} {
		if _, err := lines.Put(map[string]any{
			"account": account, "amount": float64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	declareOp(t, store, Operation{
		Name: "lines.by_account", Collection: "lines", Action: ActionTotals,
		Rollup: "per_account", Limit: 10,
	})
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	store.Budget(0)
	whole := invoke(t, store, "lines.by_account", nil)
	if len(whole.Rows) != 4 {
		t.Fatalf("the rollup came back with %d rows, not the 4 accounts written", len(whole.Rows))
	}

	// Room for one group and not for four.
	written, err := json.Marshal(whole.Rows[:1])
	if err != nil {
		t.Fatal(err)
	}
	store.Budget(len(written) - 1)
	if _, err := store.Invoke(Caller{}, "lines.by_account", 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("four groups ran inside a budget with room for one (%v)", err)
	}
}

// peakOf runs something and hands back how much the live heap grew while it
// ran, sampled from another goroutine because the thing being measured cannot
// see its own worst moment.
func peakOf(t *testing.T, run func()) (grew uint64, samples int) {
	t.Helper()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var peak, taken, stop atomic.Uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		var at runtime.MemStats
		for stop.Load() == 0 {
			runtime.ReadMemStats(&at)
			taken.Add(1)
			if at.HeapAlloc > peak.Load() {
				peak.Store(at.HeapAlloc)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	run()

	stop.Store(1)
	<-done

	if taken.Load() == 0 {
		t.Fatal("the sampler never read the heap, so there is no measurement here")
	}
	if peak.Load() > before.HeapAlloc {
		grew = peak.Load() - before.HeapAlloc
	}
	return grew, int(taken.Load())
}

// TestTheShellIsBoundedInBytesAndNotOnlyInRows is the half of ISS-37's own
// finding that the ticket states as a virtue and that is only half true.
//
// Explore caps a typed scan at MostRows whether the operator asked or not,
// which the ticket reads — correctly — as the ad-hoc path being the one that
// cannot hurt the server. But MostRows is a count of ROWS. At the two
// kilobyte rows ISS-37 measured it comes to about two megabytes; there is no
// maximum size for a single stored value (SAPE-31), so at a megabyte a row it
// is a gigabyte, and the cap would not have noticed.
//
// Explore runs its access down perform() like every declared call, so it
// inherits the budget without a line of its own. This is the measurement that
// says so, because "it goes down the same path" is the kind of sentence that
// stops being true in a diff that looks like tidying.
func TestTheShellIsBoundedInBytesAndNotOnlyInRows(t *testing.T) {
	_, store := fresh(t, 13)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}

	body := base64.StdEncoding.EncodeToString(madeUpFile(4096))
	for part := 0; part < 8; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"part": float64(part),
			"body": body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// Eight rows: a tenth of a per cent of MostRows, so nothing measured here
	// is the shell's own cap doing the work.
	access := Access{Kind: "scan", Collection: "blobs"}

	store.Budget(0)
	_, whole, err := store.Explore(Caller{}, access)
	if err != nil {
		t.Fatalf("an eight row typed scan was refused with no budget: %v", err)
	}
	if len(whole.Rows) != 8 {
		t.Fatalf("the shell came back with %d rows, not 8", len(whole.Rows))
	}

	store.Budget(4096)
	if _, _, err := store.Explore(Caller{}, access); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the shell built more than the budget allowed and was not refused (%v)", err)
	}
}
