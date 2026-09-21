package sapedb

// The fourth thing the worked module has to hold (SAPE-12, criterion 4).
//
// library_live_test.go carries the other three and every helper this file
// uses. This one is on its own because it is the only test in the set that
// has to do something BEFORE the daemon exists, and burying that ordering in
// the middle of a file where every other test opens with three lines of setup
// is how it would stop being true.

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/bundle"
	"github.com/sapedb/sapedb/internal/store"
)

// TestTheEnvelopeReadFromTheBundleIsTheEnvelopeReadFromTheStore is SAPE-12's
// fourth criterion, the half that did not hold: "its cost envelope is
// readable BEFORE installation".
//
// The other half — "matches what it does" —  is
// TestTheWorkedModulesCostEnvelopesSayWhatItActuallyDoes, which pins the
// STORE's envelopes against hand-written expectations and then against what
// the operations actually hand back when they are run. This test is the link
// that makes that mean something for a file nobody has installed: it reads
// every envelope out of examples/library/module.json with no database in
// existence, then installs that same module and reads every envelope back out
// of the store, and requires them to be equal field for field.
//
// Why that is a measurement and not a tautology, stated plainly because a
// test of the pre-install path ALONE would be self-confirming. The two sides
// share the derivation on purpose — that is the design, and two derivations
// would be two answers that can drift — but they share nothing else. The
// left-hand side is the bytes of a file. The right-hand side has been through
// bundle.Seal, verification, nine `declare` frames over a TCP connection, a
// JSON round trip into a B-tree, a commit, and a `WhatIsHere` that reads the
// operation records back off disk. Anything that changed a declaration on the
// way through — a field the wire encoding drops, a default filled in at
// declare time, a limit normalised somewhere — arrives here as an inequality.
// And the chain closes: file equals store (here), store equals what actually
// runs (the test named above).
//
// Three of the left-hand values are also written out by hand below, so that
// the pre-install path is pinned to numbers a person worked out from the
// declaration and not only to whatever the store happens to agree with.
func TestTheEnvelopeReadFromTheBundleIsTheEnvelopeReadFromTheStore(t *testing.T) {
	// BEFORE. No daemon, no database, no install: a file on disk and nothing
	// else. Deliberately the first thing this test does, so that nothing
	// further down could have left the answer somewhere for it to find.
	raw, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("reading the published module: %v", err)
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		t.Fatalf("the published module is not a bundle this version can read: %v", err)
	}

	fromFile := map[string]store.Envelope{}
	for _, reading := range bundle.Envelopes(b) {
		if reading.Err != nil {
			t.Errorf("%s: the worked module composes nothing, so every envelope in it has to read out of the file: %v",
				reading.Operation, reading.Err)
			continue
		}
		fromFile[reading.Operation] = reading.Envelope
	}
	if len(fromFile) != len(libraryOperations) {
		t.Fatalf("read %d envelopes out of %s and the module declares %d — this comparison would be running on a subset",
			len(fromFile), modulePath, len(libraryOperations))
	}

	// Worked out from the declaration by hand, on the side of the comparison
	// that has never touched a database. `borrow` is the one worth writing
	// out: three collections nobody listed, read off its three steps, and a
	// ceiling of 3 that is the sum of those steps rather than a number
	// written anywhere in the file.
	handWritten := map[string]store.Envelope{
		"library:loans.borrow": {
			Operation:   "library:loans.borrow",
			Collections: []string{"library_books", "library_loans", "library_members"},
			Indexes:     []string{},
			Limit:       3,
		},
		"library:books.by_author": {
			Operation:   "library:books.by_author",
			Collections: []string{"library_books"},
			Indexes:     []string{"by_author"},
			Limit:       50,
			Projection:  []string{"id", "on_loan", "shelf", "title"},
		},
		"library:books.on_shelf": {
			Operation:   "library:books.on_shelf",
			Collections: []string{"library_books"},
			Indexes:     []string{"by_shelf"},
			Limit:       1000,
		},
	}
	for name, want := range handWritten {
		got, found := fromFile[name]
		if !found {
			t.Errorf("%s is declared in %s and no envelope was read for it", name, modulePath)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the envelope read out of the file for %s is\n  %+v\nand a reader of the declaration would work out\n  %+v",
				name, got, want)
		}
	}

	// AFTER. The same module, installed, read back through the catalogue.
	dir := t.TempDir()
	signature := seedAnEmptyDatabase(t, dir)
	_, address := startDaemon(t, dir)

	client := dialLibrary(t, address, signature)
	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}
	installLibraryOverTheWire(t, client)

	here, err := client.WhatIsHere()
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	fromStore := map[string]store.Envelope{}
	for _, envelope := range here.Envelopes {
		fromStore[envelope.Operation] = envelope
	}
	if len(fromStore) != len(libraryOperations) {
		t.Fatalf("the catalogue carries %d envelopes and the module declares %d",
			len(fromStore), len(libraryOperations))
	}

	// Field for field, for every operation the module declares.
	for name := range libraryOperations {
		before, inFile := fromFile[name]
		after, inStore := fromStore[name]
		if !inFile {
			t.Errorf("%s: no envelope was readable from %s", name, modulePath)
			continue
		}
		if !inStore {
			t.Errorf("%s: no envelope came back from the catalogue", name)
			continue
		}

		// Version is the one field REQUIRED to differ, and it is asserted on
		// rather than skipped. A bundle carries no version — declarable()
		// refuses one, because a version is the receiving store's own account
		// of when a declaration arrived — and the store assigns 1 to the
		// first. Anything else on either side means one of those two rules
		// has quietly changed.
		if before.Version != 0 {
			t.Errorf("%s: the envelope read from the file carries version %d, and a bundle carries no version",
				name, before.Version)
		}
		if after.Version != 1 {
			t.Errorf("%s: the store assigned version %d to the first declaration of it, want 1",
				name, after.Version)
		}

		if fmt.Sprint(before.Collections) != fmt.Sprint(after.Collections) {
			t.Errorf("%s: the file says it touches %v and the store says %v",
				name, before.Collections, after.Collections)
		}
		if fmt.Sprint(before.Indexes) != fmt.Sprint(after.Indexes) {
			t.Errorf("%s: the file says it uses indexes %v and the store says %v",
				name, before.Indexes, after.Indexes)
		}
		if before.Limit != after.Limit {
			t.Errorf("%s: the file says at most %d rows and the store says %d",
				name, before.Limit, after.Limit)
		}
		if fmt.Sprint(before.Projection) != fmt.Sprint(after.Projection) {
			t.Errorf("%s: the file says fields %v escape and the store says %v",
				name, before.Projection, after.Projection)
		}
		if before.WholeDocument != after.WholeDocument {
			t.Errorf("%s: the file says whole-document %v and the store says %v",
				name, before.WholeDocument, after.WholeDocument)
		}
		if fmt.Sprint(before.Scopes) != fmt.Sprint(after.Scopes) {
			t.Errorf("%s: the file says scopes %v and the store says %v",
				name, before.Scopes, after.Scopes)
		}

		// The catch-all, and it is not redundant with the seven above. Each
		// of those names one field, so a ninth field added to store.Envelope
		// tomorrow would be compared by none of them and this criterion would
		// quietly stop covering it. This compares the whole value, with the
		// one field that is allowed to differ set equal first.
		normalised := after
		normalised.Version = before.Version
		if !reflect.DeepEqual(before, normalised) {
			t.Errorf("%s: the envelope read from the file and the envelope read from the store are not the same value\n  file  %+v\n  store %+v",
				name, before, after)
		}
	}
}

// TestABundleCannotReadTheCeilingOfAStepThatCallsAnOperation is the hard case
// criterion 4 does not reach, kept honest rather than kept quiet.
//
// A composed step pins a version (N1 in compose.go, and store's
// validateComposedStep refuses `version <= 0` outright), and a bundle's own
// operations carry no version (declarable() refuses one, because the version
// is the receiving store's record of when a declaration arrived). So a
// reference inside a bundle can only ever name a declaration the RECEIVING
// STORE holds, and there is no way to work out its cost from the file.
//
// The answer is to say so. This test is what stops that from becoming a
// guess: the two obvious guesses — match the reference by name against the
// bundle's own operations, or fall back to the enclosing operation's declared
// limit — would both produce a number here, and both are numbers that can
// disagree with what the store reports afterwards, which is the one thing
// this criterion exists to rule out.
func TestABundleCannotReadTheCeilingOfAStepThatCallsAnOperation(t *testing.T) {
	composed := bundle.Bundle{
		Format:  "sapedb/bundle:v1",
		Name:    "composed",
		Version: "1.0.0",
		Signer:  "a test",
		Operations: []store.Operation{
			{
				Name: "reports.one", Collection: "things", Action: store.ActionGet,
				Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
				Key:   &store.Term{Arg: "id"},
			},
			{
				// Calls an operation this same bundle declares, and pins a
				// version, because a step must. The bundle's own
				// "reports.one" has no version, so this reference is not to
				// it — it is to whatever the receiving store holds under that
				// name at version 1.
				Name: "reports.both", Collection: "things", Action: store.ActionBatch, Limit: 2,
				Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
				Steps: []store.Step{
					{Name: "a", Operation: "reports.one", Version: 1,
						With: map[string]store.Term{"id": {Arg: "id"}}},
					{Name: "b", Operation: "elsewhere.thing", Version: 3,
						With: map[string]store.Term{"id": {Arg: "id"}}},
				},
			},
		},
	}

	readings := bundle.Envelopes(composed)
	if len(readings) != 2 {
		t.Fatalf("read %d envelopes out of a two-operation bundle", len(readings))
	}

	// The flat one still reads. A bundle with one unreadable envelope in it
	// must not lose the rest — an operator deciding about a module reads the
	// ones that ARE readable.
	if readings[0].Err != nil {
		t.Errorf("%s calls nothing and its envelope still did not read: %v",
			readings[0].Operation, readings[0].Err)
	}
	if readings[0].Envelope.Limit != 1 {
		t.Errorf("%s is a get, so at most 1 row, and the file says %d",
			readings[0].Operation, readings[0].Envelope.Limit)
	}

	// The composed one does not, and says why.
	if readings[1].Err == nil {
		t.Fatalf("%s calls two operations this file cannot resolve, and an envelope came back anyway: %+v — a number here is a number that can disagree with the store",
			readings[1].Operation, readings[1].Envelope)
	}
	if !errors.Is(readings[1].Err, bundle.ErrNotInThisFile) {
		t.Errorf("%s was refused with %v, which is not ErrNotInThisFile — a caller cannot tell this apart from a broken bundle",
			readings[1].Operation, readings[1].Err)
	}
	// It names the reference it could not follow. "this cost is not readable"
	// with no name in it sends an operator to read the whole file.
	if !strings.Contains(readings[1].Err.Error(), "reports.one") {
		t.Errorf("the refusal does not name the reference it stopped at: %v", readings[1].Err)
	}
	// And it is not the enclosing limit dressed up as an error: the
	// declaration says 2, and nothing in the refusal may be offering it as
	// the answer.
	if readings[1].Envelope.Limit != 0 {
		t.Errorf("an unreadable envelope came back carrying a limit of %d", readings[1].Envelope.Limit)
	}
}
