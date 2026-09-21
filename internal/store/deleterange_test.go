package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stretched is a collection shaped like the thing a deleteRange is pointed at: a
// key that sorts in the order rows were written, an index that is not the key,
// and a rollup that has to be decremented when a row goes. Both of the last
// two exist so that a fast path which skipped them would have somewhere to be
// caught — a range delete that leaves an index entry behind is a corruption
// that reads as success.
func stretched(t *testing.T, store *Store) *Collection {
	t.Helper()

	made, err := store.Declare(Caller{}, Spec{
		Name: "ledger",
		Key:  Key{Path: "id", Type: TypeString},
		Indexes: []Index{{
			Name:   "by_account",
			Fields: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
		}},
		Rollups: []Rollup{{
			Name:  "per_account",
			Group: []Field{{Path: "account", Type: TypeString, Missing: MissingSkip}},
			Count: true,
			Sum:   []string{"amount"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	return made
}

// rowKey is the key of row n, written so that string order is row order.
func rowKey(n int) string { return fmt.Sprintf("row/%06d", n) }

// fillRows writes n rows, alternating between two accounts so that both the index
// and the rollup have more than one group to get wrong.
func fillRows(t *testing.T, collection *Collection, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		account := "cash"
		if i%2 == 1 {
			account = "bank"
		}
		if _, err := collection.Put(map[string]any{
			"id": rowKey(i), "account": account, "amount": float64(i),
		}); err != nil {
			t.Fatalf("writing row %d: %v", i, err)
		}
	}
	if err := collection.store.Commit(); err != nil {
		t.Fatal(err)
	}
}

// purging is the declaration under test, spelled the way the ticket spells it:
// the bounds come from the caller, the limit is written into it, and there is
// no expression anywhere.
func purging(name string, limit int) Operation {
	return Operation{
		Name: name, Collection: "ledger", Action: ActionDeleteRange,
		Input: []Parameter{
			{Name: "from", Type: TypeString, Required: true},
			{Name: "to", Type: TypeString, Required: true},
		},
		From:  &Endpoint{Terms: []Term{{Arg: "from"}}, Exclusive: true},
		To:    &Endpoint{Terms: []Term{{Arg: "to"}}},
		Limit: limit,
	}
}

// living is every key still in the collection, in key order.
func living(t *testing.T, collection *Collection) []string {
	t.Helper()
	keys := []string{}
	if err := collection.Walk(func(key any, _ map[string]any) bool {
		keys = append(keys, key.(string))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

// entriesOf is every primary key an index entry points at, in index order.
func entriesOf(t *testing.T, collection *Collection, index string) []string {
	t.Helper()
	keys := []string{}
	if err := collection.Scan(index, Range{}, func(entry Found) bool {
		keys = append(keys, entry.Key.(string))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

// deletions is every ChangeDelete in the log, in order.
//
// The LSNs come back as they were written. Two stores that declared different
// operations before the deletes reach the same delete by a different number,
// so the comparison below zeroes the field rather than this function rebasing
// it — the raw number is still wanted here, to read the rest of the log from.
func deletions(t *testing.T, store *Store) []Change {
	t.Helper()

	entries := []Change{}
	if err := store.Changes(0, func(change Change) bool {
		if change.Kind == ChangeDelete {
			entries = append(entries, change)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

// fixedClock makes two stores agree about time, so that a comparison of their
// logs is a comparison of what they recorded rather than of when.
func fixedClock(store *Store) {
	store.Clock(func() time.Time { return time.UnixMilli(1700000000000).UTC() })
}

// TestAThousandRowsGoInPagesAndTheCollectionReadsBackEmpty is the ticket's
// first acceptance criterion, measured exactly as it words it.
//
// Four calls of two hundred and fifty against one declaration. Each one has to
// say how many went, which key it got to, and whether more remain — and the
// three of those together are the cursor, because the next call is the same
// call with the last key as its exclusive lower bound. A run that did not hand
// the key back would leave a caller with a thousand rows and no way to ask for
// the next two hundred and fifty of them.
func TestAThousandRowsGoInPagesAndTheCollectionReadsBackEmpty(t *testing.T) {
	_, store := fresh(t, 21)
	collection := stretched(t, store)
	fillRows(t, collection, 1000)

	declareOp(t, store, purging("ledger.purge", 250))

	// "row/" sorts below every key this fixture writes, so the first page's
	// exclusive lower bound is the beginning of the collection.
	cursor := "row/"
	pages := 0
	removed := 0
	for {
		pages++
		if pages > 10 {
			t.Fatalf("a thousand rows in pages of 250 took more than ten calls")
		}
		result := invoke(t, store, "ledger.purge",
			map[string]any{"from": cursor, "to": rowKey(999)})

		if result.Changed != 250 {
			t.Fatalf("page %d removed %d rows, want 250", pages, result.Changed)
		}
		removed += result.Changed

		last, isKey := result.Key.(string)
		if !isKey {
			t.Fatalf("page %d named no last key, so there is nothing to continue from", pages)
		}
		if want := rowKey(removed - 1); last != want {
			t.Fatalf("page %d says it got to %q, want %q", pages, last, want)
		}
		cursor = last

		if !result.Truncated {
			break
		}
	}

	if pages != 4 || removed != 1000 {
		t.Fatalf("a thousand rows took %d pages and removed %d", pages, removed)
	}
	if left := living(t, collection); len(left) != 0 {
		t.Fatalf("the collection reads back with %d rows still in it: %v", len(left), left[:1])
	}
}

// TestADeleteRangeStopsAtItsLimitRatherThanRunningPastIt pins the boundary,
// which is the half of the rule an off-by-one would quietly move.
//
// Exactly at the limit is not over it: ten rows and a limit of ten is the
// whole range and nothing more remains. One short is a stop, and a stop says
// so — which is the difference between this and its sibling hashRange, and the
// reason it is a flag here rather than a refusal. A digest of a prefix cannot
// be told from a digest of a whole range, so SAPE-34 has no honest short
// answer to give; this one does, it is in Truncated and Key, and the ticket's
// own first criterion needs it there.
func TestADeleteRangeStopsAtItsLimitRatherThanRunningPastIt(t *testing.T) {
	_, store := fresh(t, 22)
	collection := stretched(t, store)
	fillRows(t, collection, 10)

	declareOp(t, store, purging("ledger.short", 4))
	stopped := invoke(t, store, "ledger.short",
		map[string]any{"from": "row/", "to": rowKey(9)})

	if stopped.Changed != 4 {
		t.Fatalf("a limit of four removed %d rows", stopped.Changed)
	}
	if !stopped.Truncated {
		t.Fatal("a run that stopped at its limit did not say so, so a caller reading it would stop paging a page early")
	}
	if stopped.Key != rowKey(3) {
		t.Fatalf("it got to %v, want %v", stopped.Key, rowKey(3))
	}
	if left := living(t, collection); len(left) != 6 || left[0] != rowKey(4) {
		t.Fatalf("six rows should be left starting at %v, and %d are starting at %v",
			rowKey(4), len(left), left[0])
	}

	// And the exact boundary: the six that are left, under a limit of six.
	declareOp(t, store, purging("ledger.exact", 6))
	whole := invoke(t, store, "ledger.exact",
		map[string]any{"from": "row/", "to": rowKey(9)})
	if whole.Changed != 6 {
		t.Fatalf("a limit of six removed %d of six rows", whole.Changed)
	}
	if whole.Truncated {
		t.Fatal("six rows under a limit of six reported that more remained")
	}
	if left := living(t, collection); len(left) != 0 {
		t.Fatalf("%d rows are left", len(left))
	}
}

// TestTheLogADeleteRangeWritesIsTheLogTheSameDeletesWriteOneAtATime is the
// assertion that keeps a follower working, and it is why this ticket does not
// touch the change log at all.
//
// N deletions write N entries. That is a decision rather than an omission: one
// entry carrying a list of keys would be a log shape every follower, every
// dump and every subscriber has to learn before the first one could be
// written, and ISS-23 — which this makes matter more rather than fixes — is
// about retention, not about shape. So the entries this writes are compared
// against the entries the same deletes produce one at a time, field for field,
// and the two operations are given the same name and land at the same version
// so that even the attribution matches.
func TestTheLogADeleteRangeWritesIsTheLogTheSameDeletesWriteOneAtATime(t *testing.T) {
	_, ranged := fresh(t, 23)
	fixedClock(ranged)
	inRange := stretched(t, ranged)
	fillRows(t, inRange, 12)

	_, singly := fresh(t, 23)
	fixedClock(singly)
	oneByOne := stretched(t, singly)
	fillRows(t, oneByOne, 12)

	// The same name, so that By.Operation matches; the first declaration in
	// each store, so that By.Version matches too. Everything else about the
	// two declarations differs, which is the point.
	declareOp(t, ranged, purging("ledger.purge", 8))
	invoke(t, ranged, "ledger.purge", map[string]any{"from": "row/", "to": rowKey(11)})

	declareOp(t, singly, Operation{
		Name: "ledger.purge", Collection: "ledger", Action: ActionDelete,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	})
	for i := 0; i < 8; i++ {
		invoke(t, singly, "ledger.purge", map[string]any{"id": rowKey(i)})
	}

	together, apart := deletions(t, ranged), deletions(t, singly)
	if len(together) != 8 || len(apart) != 8 {
		t.Fatalf("eight rows went and the logs hold %d and %d delete entries", len(together), len(apart))
	}
	for i := range together {
		// Everything but the LSN, which counts from the beginning of each
		// store's own log and so differs by however many entries the two
		// declarations took. That the deletes are consecutive is measured
		// below instead, by reading the log from the first of them.
		one, other := together[i], apart[i]
		one.LSN, other.LSN = 0, 0
		if !reflect.DeepEqual(one, other) {
			t.Fatalf("entry %d differs:\n  deleteRange   %+v\n  one at a time %+v", i, one, other)
		}
	}

	// And nothing else was written among them. An entry of a new kind sitting
	// between the deletes — a summary, a marker, anything — is exactly what a
	// follower would not know how to replay, and comparing only the deletes
	// above would not see it.
	between := []string{}
	if err := ranged.Changes(together[0].LSN, func(change Change) bool {
		between = append(between, change.Kind)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range between {
		if kind != ChangeDelete {
			t.Fatalf("a deleteRange wrote a %q entry among its deletes, which a follower replaying it has never seen before", kind)
		}
	}
}

// TestEveryIndexEntryAndRollupOfARemovedRowIsGone is the ticket's second
// acceptance criterion, and it is the one that makes this more than a loop
// somebody writes in an afternoon.
//
// A range delete that leaves an index entry behind is a corruption that reads
// as success: the delete returns a number, the collection looks right, and a
// scan of that index hands back an entry pointing at a document that is not
// there. A rollup that was not decremented is the same failure with a number
// instead of a row. Both are compared against a second store that deleted the
// identical rows one at a time, because "the same as if every row had been
// deleted one at a time" is what the ticket asks for and is stronger than any
// figure written into this test by hand.
func TestEveryIndexEntryAndRollupOfARemovedRowIsGone(t *testing.T) {
	_, ranged := fresh(t, 24)
	inRange := stretched(t, ranged)
	fillRows(t, inRange, 40)

	_, singly := fresh(t, 24)
	oneByOne := stretched(t, singly)
	fillRows(t, oneByOne, 40)

	declareOp(t, ranged, purging("ledger.purge", 25))
	result := invoke(t, ranged, "ledger.purge",
		map[string]any{"from": rowKey(9), "to": rowKey(34)})
	if result.Changed != 25 {
		t.Fatalf("rows 10 through 34 are 25 rows and %d went", result.Changed)
	}

	for i := 10; i <= 34; i++ {
		if _, err := oneByOne.Delete(rowKey(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := singly.Commit(); err != nil {
		t.Fatal(err)
	}

	// The documents.
	if left, want := living(t, inRange), living(t, oneByOne); !reflect.DeepEqual(left, want) {
		t.Fatalf("the rows left differ: %d after a deleteRange, %d after the same deletes one at a time",
			len(left), len(want))
	}

	// The index. Compared against the other store AND against the documents
	// themselves, because two stores that both leaked the same entry would
	// agree with each other and be wrong together.
	entries, want := entriesOf(t, inRange, "by_account"), entriesOf(t, oneByOne, "by_account")
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("the index entries differ: %v vs %v", entries, want)
	}
	here := map[string]bool{}
	for _, key := range living(t, inRange) {
		here[key] = true
	}
	if len(entries) != len(here) {
		t.Fatalf("%d index entries for %d documents", len(entries), len(here))
	}
	for _, key := range entries {
		if !here[key] {
			t.Fatalf("by_account still points at %q, which was removed — an entry with no document behind it is damage that reads as success", key)
		}
	}

	// The rollups, both groups, against the store that deleted one at a time
	// AND against the documents that are actually left.
	//
	// The second of those is not belt and braces, it is the one that works.
	// Measured: removing contribute() from Collection.remove entirely leaves
	// the comparison below green on its own, because both stores go through
	// remove() and both are wrong by the same amount — two wrong answers that
	// agree. A total is only worth checking against the rows it claims to
	// total.
	for _, account := range []string{"cash", "bank"} {
		count, sum, found := totalsOf(t, inRange, account)
		wantCount, wantSum, wantFound := totalsOf(t, oneByOne, account)
		if found != wantFound || count != wantCount || sum != wantSum {
			t.Fatalf("per_account[%s] is %d/%v after a deleteRange and %d/%v after the same deletes one at a time — something skipped contribute()",
				account, count, sum, wantCount, wantSum)
		}

		byHand, sumByHand := 0, 0.0
		if err := inRange.Walk(func(_ any, document map[string]any) bool {
			if document["account"] == account {
				byHand++
				sumByHand += document["amount"].(float64)
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if count != byHand || sum != sumByHand {
			t.Fatalf("per_account[%s] says %d rows totalling %v, and the rows still there are %d totalling %v — the rollup was not decremented for everything that went",
				account, count, sum, byHand, sumByHand)
		}
	}
}

// TestADeleteRangeCeilingIsCountedPerRunAndNotPerPartition is the W7 shape,
// measured on the action where getting it wrong destroys data rather than
// returning it.
//
// The gap task 0071 recorded is a host that checks every individual call and
// never the total: an operation declaring `limit 50` served fifty million rows
// because nobody added them up. A partitioned collection is where that gap
// could open here without anybody writing a loop, because one run of this
// operation is several walks of several files. Six rows over two partitions
// under a limit of five removes five and says more remain; a counter that
// started again at each file would see three and three, remove all six, and
// report that it had finished.
//
// A single-partition fixture cannot tell the two apart, which is why this test
// exists next to the ones above rather than instead of them.
func TestADeleteRangeCeilingIsCountedPerRunAndNotPerPartition(t *testing.T) {
	_, _, store := partitioned(t, 301)
	entries := entriesByMonth(t, store, 0)

	for _, month := range []time.Month{time.January, time.February} {
		atMonth(t, store, time.Date(2026, month, 15, 0, 0, 0, 0, time.UTC))
		for i := 0; i < 3; i++ {
			if _, err := entries.Put(map[string]any{"account": "cash"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if names := store.Parts(); len(names) != 2 {
		t.Fatalf("this test needs two partitions and has %v", names)
	}

	declareOp(t, store, Operation{
		Name: "entries.purge", Collection: "entries", Action: ActionDeleteRange,
		Limit: 5,
	})
	result := invoke(t, store, "entries.purge", nil)

	if result.Changed != 5 {
		t.Fatalf("six rows over two partitions under a limit of five removed %d — the ceiling is being counted per file rather than per run", result.Changed)
	}
	if !result.Truncated {
		t.Fatal("five of six rows went and it reported that nothing more remained")
	}
	if left := living(t, entries); len(left) != 1 {
		t.Fatalf("%d rows are left of six, want 1", len(left))
	}

	// The control: a limit that covers the total finishes, so the stop above
	// is the ceiling and not the partitioning.
	declareOp(t, store, Operation{
		Name: "entries.rest", Collection: "entries", Action: ActionDeleteRange,
		Limit: 6,
	})
	rest := invoke(t, store, "entries.rest", nil)
	if rest.Changed != 1 || rest.Truncated {
		t.Fatalf("the last row came back as %d removed, more=%v", rest.Changed, rest.Truncated)
	}
}

// TestWhatADeleteRangeMayAndMayNotDeclare is every refusal this action adds,
// with the declaration that passes sitting next to each one.
//
// One table rather than one test per rule, because each row is the same shape:
// a field changed on a declaration that is otherwise known to work, so a row
// that stops failing is a rule that stopped being enforced rather than a test
// that needs rewriting.
func TestWhatADeleteRangeMayAndMayNotDeclare(t *testing.T) {
	_, store := fresh(t, 25)
	stretched(t, store)

	if _, err := store.DeclareOperation(Caller{}, purging("ledger.ok", 10)); err != nil {
		t.Fatalf("a deleteRange that declares its limit was refused: %v", err)
	}

	for _, one := range []struct {
		why    string
		change func(*Operation)
		saying string
	}{
		{
			// The number bounds what is destroyed and how many entries every
			// follower replays. There is no default this store may pick.
			why:    "no limit",
			change: func(op *Operation) { op.Limit = 0 },
			saying: "how many rows it may remove",
		},
		{
			why:    "a negative limit",
			change: func(op *Operation) { op.Limit = -1 },
			saying: "how many rows it may remove",
		},
		{
			// An index is a second order over the same rows, and removing a
			// document removes every entry it has.
			why:    "an index",
			change: func(op *Operation) { op.Index = "by_account" },
			saying: "in key order",
		},
		{
			// With a limit smaller than the stretch, a direction would let
			// the caller choose which end of somebody's data is destroyed.
			why:    "a direction",
			change: func(op *Operation) { op.Direction = &Term{Value: DirectionReverse, Constant: true} },
			saying: "which end of it is destroyed",
		},
		{
			why:    "a projection",
			change: func(op *Operation) { op.Projection = []string{"amount"} },
			saying: "removes rows rather than returning them",
		},
		{
			// Field and Decode are a hashRange's words. An action that writes
			// one down has written a word this store will not read.
			why:    "a field",
			change: func(op *Operation) { op.Field = "amount" },
			saying: "does not digest a field",
		},
		{
			why:    "a decode",
			change: func(op *Operation) { op.Decode = DecodeBase64 },
			saying: "decodes nothing",
		},
	} {
		t.Run(one.why, func(t *testing.T) {
			operation := purging("ledger.refused", 10)
			one.change(&operation)
			_, err := store.DeclareOperation(Caller{}, operation)
			if !errors.Is(err, ErrDeclaration) {
				t.Fatalf("%s was accepted (%v)", one.why, err)
			}
			if !strings.Contains(err.Error(), one.saying) {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
}

// TestADeleteRangeOfAHashPartitionedCollectionIsRefused matches the answer
// scan, count and hashRange already give rather than deciding it again.
//
// The ticket flagged partition crossing as a decision rather than an
// implementation detail, and it is a decision scanAcross already made: hash
// order is no order, so nothing that crosses those partitions can be ordered.
// It binds harder here than on a read — without key order, "up to N of them,
// and here is the last key" is not a cursor, and a second page would remove
// rows from wherever the hash happened to put them.
func TestADeleteRangeOfAHashPartitionedCollectionIsRefused(t *testing.T) {
	_, _, store := partitioned(t, 302)
	if _, err := store.Declare(Caller{}, Spec{
		Name:      "spread",
		Key:       Key{Path: "id", Type: TypeString},
		Partition: &Partition{By: ByHash, Into: 4},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := store.DeclareOperation(Caller{}, Operation{
		Name: "spread.purge", Collection: "spread", Action: ActionDeleteRange, Limit: 10,
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a deleteRange over a hash-partitioned collection was accepted (%v)", err)
	}
	if !strings.Contains(err.Error(), "no order") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheEnvelopeOfADeleteRangeSaysHowManyRowsItMayRemove is the ticket's
// third acceptance criterion: a caller can read the declared limit before
// invoking.
//
// The number that matters here is not the one a count or a hashRange reports.
// Those two walk many rows and hand back one answer, so ceiling() reports one
// for each — what they contribute to an enclosing batch's declared sum. A
// deleteRange touches every row it walks, so its ceiling is its declared
// limit, and that is the number somebody deciding "may I run this" has to see.
func TestTheEnvelopeOfADeleteRangeSaysHowManyRowsItMayRemove(t *testing.T) {
	_, store := fresh(t, 26)
	stretched(t, store)
	declareOp(t, store, purging("ledger.purge", 500))

	envelope, err := store.Envelope("ledger.purge", 0)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Limit != 500 {
		t.Fatalf("the envelope says a limit of %d, want the declared 500", envelope.Limit)
	}
	if envelope.WholeDocument || len(envelope.Projection) > 0 {
		t.Errorf("the envelope says fields escape from an action that returns none: %+v", envelope)
	}
	if !reflect.DeepEqual(envelope.Collections, []string{"ledger"}) {
		t.Errorf("the envelope names collections %v", envelope.Collections)
	}
	if !reflect.DeepEqual(envelope.Indexes, []string{ClusteredIndex}) {
		t.Errorf("the envelope names indexes %v, and a deleteRange walks key order", envelope.Indexes)
	}
}

// TestTheShellDeleteAndTheDeclaredOperationRemoveTheSameRange is the ticket's
// fourth acceptance criterion: two spellings of one primitive that disagree is
// worse than one spelling.
//
// They cannot drift, because the typed one is not a second path — Explore
// builds the Operation the access would have to be declared as, holds it to
// validateOperation, and runs it down perform() like any other. This measures
// that rather than trusting it, on two stores filled identically.
func TestTheShellDeleteAndTheDeclaredOperationRemoveTheSameRange(t *testing.T) {
	_, typedStore := fresh(t, 27)
	typedIn := stretched(t, typedStore)
	fillRows(t, typedIn, 20)

	_, declaredStore := fresh(t, 27)
	declaredIn := stretched(t, declaredStore)
	fillRows(t, declaredIn, 20)

	draft, typed, err := typedStore.Explore(Caller{}, Access{
		Kind: "delete", Collection: "ledger",
		From:  &Bound{Values: []any{rowKey(4)}, Exclusive: true},
		To:    &Bound{Values: []any{rowKey(15)}},
		Limit: 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	declareOp(t, declaredStore, purging("ledger.purge", 7))
	declared := invoke(t, declaredStore, "ledger.purge",
		map[string]any{"from": rowKey(4), "to": rowKey(15)})

	if typed.Changed != declared.Changed || typed.Truncated != declared.Truncated ||
		typed.Key != declared.Key {
		t.Fatalf("the two spellings disagree:\n  typed    %d removed, last %v, more %v\n  declared %d removed, last %v, more %v",
			typed.Changed, typed.Key, typed.Truncated,
			declared.Changed, declared.Key, declared.Truncated)
	}
	if !reflect.DeepEqual(living(t, typedIn), living(t, declaredIn)) {
		t.Fatal("the two spellings left different rows behind")
	}

	// And the draft is a declaration, not a description of one: exploring
	// ends in something to commit rather than in a habit of poking at
	// production.
	if draft.Action != ActionDeleteRange || draft.Limit != 7 {
		t.Fatalf("the draft is %+v", draft)
	}
	draft.Name = "ledger.from_the_shell"
	if _, err := declaredStore.DeclareOperation(Caller{}, draft); err != nil {
		t.Fatalf("the draft the shell handed back cannot be declared: %v", err)
	}
}

// TestATypedDeleteMustSayHowFarItGoes is the third answer this shell's limit
// rule gives to three actions, and the reason it is not the scan's.
//
// A scan and a count are capped at MostRows when an operator does not say,
// because a capped read still answers: a page, and a flag saying there was
// more. A capped delete would also answer perfectly well — and the answer
// would be that this shell picked how many of somebody's rows to destroy
// because they did not say.
func TestATypedDeleteMustSayHowFarItGoes(t *testing.T) {
	_, store := fresh(t, 28)
	collection := stretched(t, store)
	fillRows(t, collection, 5)

	_, _, err := store.Explore(Caller{}, Access{Kind: "delete", Collection: "ledger"})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a typed delete with no limit was accepted (%v)", err)
	}
	if !strings.Contains(err.Error(), "how many rows it may remove") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if left := living(t, collection); len(left) != 5 {
		t.Fatalf("a refused delete removed %d rows anyway", 5-len(left))
	}
}

// TestADeleteRangeIsAWrite guards the three things that ask store.Writes
// rather than asking the action by name: the server refusing a write on a
// follower, SharedRead choosing the write lock over the read lock, and
// perform() answering a repeated write id. An action missing from that list
// reads as a read, and a read on a follower is allowed to run.
func TestADeleteRangeIsAWrite(t *testing.T) {
	if !Writes(ActionDeleteRange) {
		t.Fatal("a deleteRange is not in Writes(), so a follower would let one run and a reader's lock would be taken for it")
	}
}

// TestADeleteRangeCannotBeAStepThatTouchesACollection: a step carries a key, a
// document or a set, and never a stretch — so there is nowhere in one to write
// the bounds this action needs, and a step that names it is refused where it
// is written rather than found at call time.
func TestADeleteRangeCannotBeAStepThatTouchesACollection(t *testing.T) {
	_, store := fresh(t, 29)
	stretched(t, store)

	_, err := store.DeclareOperation(Caller{}, Operation{
		Name: "ledger.batch", Collection: "ledger", Action: ActionBatch,
		Steps: []Step{{Collection: "ledger", Action: ActionDeleteRange}},
	})
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a deleteRange step was accepted (%v)", err)
	}
}

// TestABatchThatCallsADeleteRangeMustCoverItsWholeLimit is N5 reaching the
// action where the number means rows destroyed rather than rows returned.
//
// A deleteRange's ceiling is its declared limit, not one, because it touches
// every row it walks. So an operation that calls one has to declare a limit
// that covers it — which is what keeps the number written on the outermost
// declaration an honest bound on what the whole thing does.
func TestABatchThatCallsADeleteRangeMustCoverItsWholeLimit(t *testing.T) {
	_, store := fresh(t, 30)
	stretched(t, store)
	declareOp(t, store, purging("ledger.purge", 50))

	calling := func(limit int) Operation {
		return Operation{
			Name: "ledger.cleanup", Collection: "ledger", Action: ActionBatch, Limit: limit,
			Input: []Parameter{
				{Name: "from", Type: TypeString, Required: true},
				{Name: "to", Type: TypeString, Required: true},
			},
			Steps: []Step{{
				Operation: "ledger.purge", Version: 1,
				With: map[string]Term{"from": {Arg: "from"}, "to": {Arg: "to"}},
			}},
		}
	}

	_, err := store.DeclareOperation(Caller{}, calling(10))
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a batch declaring 10 that calls a deleteRange of 50 was accepted (%v)", err)
	}
	if !strings.Contains(err.Error(), "50") {
		t.Errorf("the refusal does not say what the steps may reach: %v", err)
	}
	if _, err := store.DeclareOperation(Caller{}, calling(50)); err != nil {
		t.Fatalf("a batch declaring 50 that calls a deleteRange of 50 was refused: %v", err)
	}
}
