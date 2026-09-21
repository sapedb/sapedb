package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// TestADeleteRangeDeclaredOverTheWireMustSayHowManyRowsItMayRemove is the
// fourth member of the family TestAScanDeclaredOverTheWireMustSayHowManyRows-
// ItMayReturn, TestACountDeclaredOverTheWireMustSayHowFarItWalks and
// TestAHashRangeDeclaredOverTheWireMustSayHowFarItWalks belong to, and the one
// where the number is not a ceiling on what comes back.
//
// A scan's limit bounds the rows it hands you, a count's and a hashRange's
// bound the walk they pay for. This one bounds what is destroyed, and how many
// change-log entries every follower of this database replays. There is no
// default this store may pick, because a default would be this store deciding
// on somebody's behalf how much of their data goes.
//
// Measured against the Declare frame for the same reason its three siblings
// are: the Explore path cannot measure it. A typed delete is refused for a
// missing limit by asOperation, before validateOperation is reached — a
// different refusal, in different words, and a test written through Explore
// would be green whether this rule existed or not.
func TestADeleteRangeDeclaredOverTheWireMustSayHowManyRowsItMayRemove(t *testing.T) {
	const rule = "a deleteRange must declare how many rows it may remove"

	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	db, release, err := server.Store("acct", "declare-db")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "chunk",
		Key:  store.Key{Path: "id", Type: store.TypeString},
	}); err != nil {
		t.Fatal(err)
	}

	withLimit := store.Operation{
		Name: "chunk.purge", Collection: "chunk", Action: store.ActionDeleteRange,
		Input: []store.Parameter{
			{Name: "from", Type: store.TypeString, Required: true},
			{Name: "to", Type: store.TypeString, Required: true},
		},
		From:  &store.Endpoint{Terms: []store.Term{{Arg: "from"}}, Exclusive: true},
		To:    &store.Endpoint{Terms: []store.Term{{Arg: "to"}}},
		Limit: 500,
	}
	stored, err := db.DeclareOperation(store.Caller{}, withLimit)
	if err != nil {
		t.Fatalf("a deleteRange that declares its limit was refused: %v", err)
	}
	if stored.Limit != 500 {
		t.Fatalf("the stored declaration says %d, want the 500 it was declared with", stored.Limit)
	}

	limitless := withLimit
	limitless.Limit = 0
	_, err = db.DeclareOperation(store.Caller{}, limitless)
	if !errors.Is(err, store.ErrDeclaration) {
		t.Fatalf("a deleteRange with no limit was accepted (%v)", err)
	}
	if got := codeFor(err); got != "declaration" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "declaration")
	}
	if !strings.Contains(err.Error(), rule) {
		t.Errorf("the refusal reads %q, and does not say %q", err, rule)
	}
}

// TestADeleteRangeIsRefusedOnAFollower is the guard that needed no new code
// and would be a silent hole if store.Writes had been left alone.
//
// A follower writes no entries of its own, and the server decides whether a
// call writes by asking store.Writes about the action the DECLARATION names —
// not about the frame, and not about a list kept a second time somewhere else.
// An action missing from that list reads as a read: it would be allowed to run
// on a follower, and it would be served under the shared read lock while it
// removed rows. This is what says deleteRange is on the list, through the
// server rather than by inspecting the list.
func TestADeleteRangeIsRefusedOnAFollower(t *testing.T) {
	if !store.Writes(store.ActionDeleteRange) {
		t.Fatal("store.Writes does not call a deleteRange a write")
	}

	follower, err := New(Options{Dir: t.TempDir(), Secret: secret, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Close() })

	// The refusal the server gives, reached directly, because that is the one
	// thing this test is about: what a read-only server answers about an
	// action declared to remove rows.
	err = follower.readOnly(`"chunk.purge" is declared to ` + store.ActionDeleteRange)
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a follower did not refuse a deleteRange (%v)", err)
	}
	if got := codeFor(err); got != "read_only" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "read_only")
	}
}
