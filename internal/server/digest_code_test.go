package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// TestAHashRangeReachesTheWireWithACodeOfItsOwn is SAPE-34's half of the
// lesson ISS-21 is this project's record of: a refusal that arrives as
// "failed" is a refusal a client cannot act on.
//
// The two this action adds are worth separating, and separating them is what
// this test measures. "digest" says a row in the range could not go into the
// hash, and the message names it — the data is wrong, or the declaration
// points at the wrong field, and retrying changes nothing. "ceiling" says the
// answer would have been correct and is being withheld, because a digest of a
// prefix is indistinguishable from one of the whole range; what the reader
// has to do is decide whether they want to pay for a longer walk and
// redeclare. One code for both would tell half of those readers to do the
// other one's job.
func TestAHashRangeReachesTheWireWithACodeOfItsOwn(t *testing.T) {
	server, err := New(Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	db, release, err := server.Store("acct", "digest-db")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	collection, err := db.Declare(store.Caller{}, store.Spec{
		Name: "blobs",
		Key:  store.Key{Path: "id", Type: store.TypeString},
	})
	if err != nil {
		t.Fatal(err)
	}
	for part := 0; part < 4; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("f/%06d", part),
			"body": base64.StdEncoding.EncodeToString([]byte{byte(part)}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}

	digest := store.Operation{
		Name: "blobs.digest", Collection: "blobs", Action: store.ActionHashRange,
		Field: "body", Decode: store.DecodeBase64, Limit: 4,
	}
	if _, err := db.DeclareOperation(store.Caller{}, digest); err != nil {
		t.Fatal(err)
	}

	// The control. Without it, every assertion below is satisfied by a server
	// on which nothing works at all.
	whole, err := db.Invoke(store.Caller{}, "blobs.digest", 0, nil)
	if err != nil || len(whole.Digest) != 64 {
		t.Fatalf("the range that should hash answered %v %+v", err, whole)
	}

	// The ceiling: the same range, one row short.
	short := digest
	short.Name = "blobs.short"
	short.Limit = 3
	if _, err := db.DeclareOperation(store.Caller{}, short); err != nil {
		t.Fatal(err)
	}
	_, ceilingErr := db.Invoke(store.Caller{}, "blobs.short", 0, nil)
	if !errors.Is(ceilingErr, store.ErrCeiling) {
		t.Fatalf("errors.Is(err, store.ErrCeiling) is false for %v", ceilingErr)
	}
	if got := codeFor(ceilingErr); got != "ceiling" {
		t.Errorf("codeFor(%v) = %q, want %q", ceilingErr, got, "ceiling")
	}

	// The row that cannot go in: a fifth one whose body is not base64.
	if _, err := collection.Put(map[string]any{"id": "f/000004", "body": "not base64!"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
	wider := digest
	wider.Name = "blobs.wider"
	wider.Limit = 10
	if _, err := db.DeclareOperation(store.Caller{}, wider); err != nil {
		t.Fatal(err)
	}
	_, digestErr := db.Invoke(store.Caller{}, "blobs.wider", 0, nil)
	if !errors.Is(digestErr, store.ErrDigest) {
		t.Fatalf("errors.Is(err, store.ErrDigest) is false for %v", digestErr)
	}
	if got := codeFor(digestErr); got != "digest" {
		t.Errorf("codeFor(%v) = %q, want %q", digestErr, got, "digest")
	}

	// And they really are two codes, not one word reached twice.
	if codeFor(ceilingErr) == codeFor(digestErr) {
		t.Fatalf("both refusals arrive as %q", codeFor(digestErr))
	}
}

// TestAHashRangeDeclaredOverTheWireMustSayHowFarItWalks is the third member of
// the family TestAScanDeclaredOverTheWireMustSayHowManyRowsItMayReturn and
// TestACountDeclaredOverTheWireMustSayHowFarItWalks belong to, and the one
// where the rule does the most work.
//
// A scan's cost is roughly the rows it hands you, and a count's answer at
// least grows with the walk. A hashRange answers thirty-two bytes whether it
// read four rows or forty million, so the declaration is the only place its
// cost can be written down at all.
//
// Measured against the Declare frame for the same reason its two siblings are:
// the Explore path cannot measure it. A typed hash is refused for a missing
// limit by asOperation, before validateOperation is reached — a different
// refusal, in different words, and a test written through Explore would be
// green whether this rule existed or not.
func TestAHashRangeDeclaredOverTheWireMustSayHowFarItWalks(t *testing.T) {
	const rule = "a hashRange must declare how far it walks"

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
		Name: "blobs",
		Key:  store.Key{Path: "id", Type: store.TypeString},
	}); err != nil {
		t.Fatal(err)
	}

	withLimit := store.Operation{
		Name: "blobs.digest", Collection: "blobs", Action: store.ActionHashRange,
		Field: "body", Decode: store.DecodeBase64, Limit: 5000,
	}
	stored, err := db.DeclareOperation(store.Caller{}, withLimit)
	if err != nil {
		t.Fatalf("a hashRange that declares its limit was refused: %v", err)
	}
	if stored.Limit != 5000 {
		t.Fatalf("the stored declaration says %d, want the 5000 it was declared with", stored.Limit)
	}

	limitless := withLimit
	limitless.Limit = 0
	_, err = db.DeclareOperation(store.Caller{}, limitless)
	if !errors.Is(err, store.ErrDeclaration) {
		t.Fatalf("a hashRange with no limit was accepted (%v)", err)
	}
	if got := codeFor(err); got != "declaration" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "declaration")
	}
	if !strings.Contains(err.Error(), rule) {
		t.Errorf("the refusal reads %q, and does not say %q", err, rule)
	}
}
