package store

import (
	"testing"
)

// sharing is the world the classification below is measured against: one
// ordinary collection, one partitioned collection, and the flat operations a
// composed one is built out of. Nothing here is composed; this is the
// vocabulary.
func sharingWorld(t *testing.T) *Store {
	t.Helper()
	_, _, s := partitioned(t, 91)

	if _, err := s.Declare(Caller{}, Spec{
		Name: "items",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{{
			Name:   "by_shelf",
			Fields: []Field{{Path: "shelf", Type: TypeString, Missing: MissingSkip}},
		}},
	}); err != nil {
		t.Fatalf("declare items: %v", err)
	}
	// A partitioned collection, for the leg of the rule that is not about
	// writing: a get here opens a partition file, which is a write wearing a
	// reader's name.
	if _, err := s.Declare(Caller{}, Spec{
		Name:      "entries",
		Key:       Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Partition: &Partition{By: ByTime, Every: EveryMonth},
	}); err != nil {
		t.Fatalf("declare entries: %v", err)
	}

	for _, operation := range []Operation{
		{
			Name: "items.get", Collection: "items", Action: ActionGet,
			Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
			Key:   &Term{Arg: "id"},
		},
		{
			Name: "items.page", Collection: "items", Action: ActionScan,
			Index: "by_shelf", Limit: 10,
		},
		{
			Name: "items.add", Collection: "items", Action: ActionInsert,
			Input:    []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
			Document: map[string]Term{"shelf": {Arg: "shelf"}},
		},
		{
			Name: "items.digest", Collection: "items", Action: ActionHashRange,
			Field: "shelf", Limit: 10,
		},
		{
			Name: "items.sweep", Collection: "items", Action: ActionDeleteRange,
			Limit: 10,
		},
		{
			Name: "entries.get", Collection: "entries", Action: ActionGet,
			Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
			Key:   &Term{Arg: "id"},
		},
	} {
		declareOp(t, s, operation)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	return s
}

// shares is the answer SharedRead gives for an operation, which is the answer
// the server turns into a read lock or a write lock.
func shares(t *testing.T, s *Store, name string) bool {
	t.Helper()
	_, shared, err := s.SharedRead(Caller{}, name, 0)
	if err != nil {
		t.Fatalf("SharedRead %q: %v", name, err)
	}
	return shared
}

// TestWhichOperationsMayRunBesideOtherReaders is the classification half of
// ISS-9: a composed operation that only reads is a read, and everything that
// was a write before this still is.
//
// It asks SharedRead rather than the walk underneath it because SharedRead is
// the function the server calls and its bool is the whole of what decides
// between db.mutex.RLock and db.mutex.Lock (internal/server/server.go). The
// other half of the ticket — that two of these really are inside one database
// at once — is not answerable here and is measured where it happens, in
// internal/server's TestAComposedReadSharesTheDatabase.
func TestWhichOperationsMayRunBesideOtherReaders(t *testing.T) {
	s := sharingWorld(t)

	// Composed operations, declared in dependency order because a step pins a
	// version that must already exist.
	declareOp(t, s, Operation{
		Name: "items.three_pages", Collection: "items", Action: ActionBatch, Limit: 30,
		Steps: []Step{
			{Operation: "items.page", Version: 1},
			{Operation: "items.page", Version: 1},
			{Operation: "items.page", Version: 1},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.two_gets", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Steps: []Step{
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "id"}},
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "id"}},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.plain_write", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Steps: []Step{
			{Action: ActionInsert, Collection: "items", Document: map[string]Term{"shelf": {Arg: "shelf"}}},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.read_then_write", Collection: "items", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "shelf", Type: TypeString, Required: true},
		},
		Steps: []Step{
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "id"}},
			{Action: ActionInsert, Collection: "items", Document: map[string]Term{"shelf": {Arg: "shelf"}}},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.calls_a_write", Collection: "items", Action: ActionBatch, Limit: 11,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Steps: []Step{
			{Operation: "items.page", Version: 1},
			{Operation: "items.add", Version: 1, With: map[string]Term{"shelf": {Arg: "shelf"}}},
		},
	})
	// The action ISS-9 could not have known about: deleteRange landed after
	// the ticket was written, it is a write, and a batch that calls one must
	// not be shareable.
	declareOp(t, s, Operation{
		Name: "items.calls_a_sweep", Collection: "items", Action: ActionBatch, Limit: 20,
		Steps: []Step{
			{Operation: "items.page", Version: 1},
			{Operation: "items.sweep", Version: 1},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.reads_a_partition", Collection: "items", Action: ActionBatch,
		Input: []Parameter{{Name: "id", Type: TypeString, Required: true}},
		Steps: []Step{
			{Action: ActionGet, Collection: "items", Key: &Term{Arg: "id"}},
			{Action: ActionGet, Collection: "entries", Key: &Term{Arg: "id"}},
		},
	})
	// Two levels, so the walk is measured at depth rather than at the first
	// step of the outermost operation.
	declareOp(t, s, Operation{
		Name: "items.nested_read", Collection: "items", Action: ActionBatch, Limit: 60,
		Steps: []Step{
			{Operation: "items.three_pages", Version: 1},
			{Operation: "items.three_pages", Version: 1},
		},
	})
	declareOp(t, s, Operation{
		Name: "items.nested_write", Collection: "items", Action: ActionBatch, Limit: 41,
		Input: []Parameter{{Name: "shelf", Type: TypeString, Required: true}},
		Steps: []Step{
			{Operation: "items.three_pages", Version: 1},
			{Operation: "items.calls_a_write", Version: 1, With: map[string]Term{"shelf": {Arg: "shelf"}}},
		},
	})
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		name   string
		shared bool
		why    string
	}{
		{"items.get", true, "a get of an unpartitioned collection is the plainest read there is"},
		{"items.page", true, "a scan reads"},
		{"items.digest", true, "a hashRange walks rows and writes nothing"},
		{"items.add", false, "an insert writes"},
		{"items.sweep", false, "a deleteRange removes rows"},
		{"entries.get", false, "a get of a partitioned collection opens a partition file"},

		{"items.three_pages", true, "three scans in one call read and nothing else — this is the case ISS-9 is about"},
		{"items.two_gets", true, "a batch of plain get steps reads"},
		{"items.plain_write", false, "a batch with an insert step writes"},
		{"items.read_then_write", false, "one writing step is enough"},
		{"items.calls_a_write", false, "a batch that calls an insert writes through the call"},
		{"items.calls_a_sweep", false, "a batch that calls a deleteRange writes through the call"},
		{"items.reads_a_partition", false, "a get step on a partitioned collection is a writer wearing a reader's name"},
		{"items.nested_read", true, "two levels of reading is reading"},
		{"items.nested_write", false, "a write two levels down is still a write"},
	} {
		if got := shares(t, s, want.name); got != want.shared {
			t.Errorf("%s: shared=%v, want %v — %s", want.name, got, want.shared, want.why)
		}
	}
}

// TestRedeclaringACalleeAsAWriteDoesNotChangeWhatTheCallerIs is the case that
// looked like it decided where this answer should live: derive it per call, or
// work it out once when the operation is declared and store it.
//
// It decides nothing, and that is the finding. A step pins a version (N1),
// DeclareOperation only ever hands out latest+1 and never writes over a
// version that already exists, so redeclaring the callee as a write makes a
// NEW version and the batch goes on naming the old one. Derived per call and
// stored at declare time give the same answer here — so the choice was made on
// the two places they do differ, which shareable's godoc (compose.go) sets
// out.
//
// What this pins is that the per-call walk does not quietly follow the callee
// to its newest version. A walk that looked the callee up with version 0
// ("whichever is latest", the meaning everywhere else in this package) would
// pass every other test in this file and turn this one red — and in
// production it would classify a batch by a declaration that batch never runs.
func TestRedeclaringACalleeAsAWriteDoesNotChangeWhatTheCallerIs(t *testing.T) {
	s := sharingWorld(t)

	declareOp(t, s, Operation{
		Name: "items.leg", Collection: "items", Action: ActionScan,
		Index: "by_shelf", Limit: 10,
	})
	declareOp(t, s, Operation{
		Name: "items.calls_the_leg", Collection: "items", Action: ActionBatch, Limit: 10,
		Steps: []Step{{Operation: "items.leg", Version: 1}},
	})
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	if !shares(t, s, "items.calls_the_leg") {
		t.Fatal("a batch calling one scan is not shareable before anything is redeclared, so the rest of this test would measure nothing")
	}

	// The same name, now a write. Version 2.
	written := declareOp(t, s, Operation{
		Name: "items.leg", Collection: "items", Action: ActionInsert,
		Document: map[string]Term{"shelf": {Value: "x", Constant: true}},
	})
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if written.Version != 2 {
		t.Fatalf("redeclaring items.leg made version %d, want 2 — this test assumes versions are handed out rather than overwritten", written.Version)
	}

	if shares(t, s, "items.leg") {
		t.Error("the newest items.leg is an insert and SharedRead calls it shareable")
	}
	if !shares(t, s, "items.calls_the_leg") {
		t.Error("items.calls_the_leg pins items.leg@1, which is still a scan, and it is no longer shareable: " +
			"the classification followed the callee to a version this batch does not call")
	}

	// And the control, so the line above is about pinning rather than about
	// nothing having changed: a batch declared NOW against version 2 is a
	// write.
	declareOp(t, s, Operation{
		Name: "items.calls_the_new_leg", Collection: "items", Action: ActionBatch, Limit: 10,
		Steps: []Step{{Operation: "items.leg", Version: 2}},
	})
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if shares(t, s, "items.calls_the_new_leg") {
		t.Error("a batch pinning items.leg@2, which is an insert, is shareable")
	}
}
