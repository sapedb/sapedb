package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestATypedHashSaysHowFarToWalkOrIsRefused is the shell's half of the ceiling
// rule, and the one place this action does NOT borrow the shell's usual
// answer.
//
// Explore caps a scan's limit and a count's at MostRows rather than refusing,
// because an operator who does not say is not asking for everything and a
// capped scan still answers: a page, and a flag saying there was more. A
// capped hash does not answer, it refuses — so capping at a thousand rows
// would refuse every ten-megabyte file this command exists to check, on a
// limit the operator never chose. Asking is the honest version of the same
// reasoning, and the last case below is the control that keeps it a statement
// about this action rather than about the shell.
func TestATypedHashSaysHowFarToWalkOrIsRefused(t *testing.T) {
	_, store := fresh(t, 37)
	collection, err := store.Declare(Caller{}, blobs())
	if err != nil {
		t.Fatal(err)
	}
	file := madeUpFile(4 * 64)
	first, last := split(t, collection, file, 64)
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	asked := Access{
		Kind: "hash", Collection: "blobs", Field: "body", Decode: DecodeBase64,
		From: &Bound{Values: []any{first}}, To: &Bound{Values: []any{last}},
	}

	// With a limit it runs, and hands back a draft somebody can declare.
	withLimit := asked
	withLimit.Limit = 100
	draft, result, err := store.Explore(Caller{Actor: "an-operator"}, withLimit)
	if err != nil {
		t.Fatalf("a typed hash with a limit: %v", err)
	}
	outside := sha256.Sum256(file)
	if result.Digest != hex.EncodeToString(outside[:]) {
		t.Errorf("the shell answered %s and the file hashes to %s",
			result.Digest, hex.EncodeToString(outside[:]))
	}
	if draft.Action != ActionHashRange || draft.Field != "body" ||
		draft.Decode != DecodeBase64 || draft.Limit != 100 {
		t.Errorf("the draft is %+v", draft)
	}
	if draft.Index != "" {
		t.Errorf("the draft carries index %q, and a hashRange walks key order", draft.Index)
	}

	// Without one it is refused rather than capped, and says why.
	_, _, err = store.Explore(Caller{Actor: "an-operator"}, asked)
	if !errors.Is(err, ErrDeclaration) {
		t.Fatalf("a typed hash with no limit answered %v", err)
	}
	if !strings.Contains(err.Error(), "how far to walk") {
		t.Errorf("the refusal reads %q", err)
	}

	// The control: the same shell, the same collection, a scan with no limit —
	// capped and run, exactly as before. So the refusal above is this action's
	// and not a shell that stopped filling limits in.
	if _, result, err := store.Explore(Caller{Actor: "an-operator"}, Access{
		Kind: "scan", Collection: "blobs",
	}); err != nil {
		t.Fatalf("a typed scan with no limit was refused: %v", err)
	} else if len(result.Rows) != 4 {
		t.Errorf("the capped scan read %d rows", len(result.Rows))
	}
}
