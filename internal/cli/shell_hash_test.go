package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// TestWhatAHashLineGoesOutAs is the hash command's half of
// TestWhatWasTypedIsWhatGoesOut: a line somebody typed becomes exactly one
// access with exactly the bounds, the field and the decode they meant.
//
// It matters more here than for a scan. A scan that went out with a bound
// dropped hands back rows somebody can look at and disbelieve; a hash that
// went out with a bound dropped hands back thirty-two characters that look
// exactly like the right thirty-two characters.
func TestWhatAHashLineGoesOutAs(t *testing.T) {
	for _, one := range []struct {
		line   string
		wanted store.Access
	}{
		{`hash books field body limit 10`, store.Access{
			Kind: "hash", Collection: "books", Field: "body", Limit: 10,
		}},
		{`hash books field body decode base64 limit 5000`, store.Access{
			Kind: "hash", Collection: "books", Field: "body",
			Decode: "base64", Limit: 5000,
		}},
		// The same bounds a scan takes, in any order, inclusive and not.
		{`hash books from "f/000000" to "f/004999" field body decode base64 limit 5000`, store.Access{
			Kind: "hash", Collection: "books", Field: "body", Decode: "base64", Limit: 5000,
			From: &store.Bound{Values: []any{"f/000000"}},
			To:   &store.Bound{Values: []any{"f/004999"}},
		}},
		{`hash books after "f/000000" before "f/004999" field body limit 5000`, store.Access{
			Kind: "hash", Collection: "books", Field: "body", Limit: 5000,
			From: &store.Bound{Values: []any{"f/000000"}, Exclusive: true},
			To:   &store.Bound{Values: []any{"f/004999"}, Exclusive: true},
		}},
	} {
		look := &looked{}
		typed(t, look, one.line)
		if len(look.asked) != 1 {
			t.Errorf("%q asked for %d accesses", one.line, len(look.asked))
			continue
		}
		if !reflect.DeepEqual(look.asked[0], one.wanted) {
			t.Errorf("%q\n  went out as %+v\n  wanted      %+v", one.line, look.asked[0], one.wanted)
		}
	}
}

// TestAHashTakesNoIndex: a hashRange walks key order, so the bare word a scan
// reads as an index has nowhere to go in a hash line.
//
// Read as an index it would have gone out as an access the store then refuses,
// with a message about index order, for a line whose actual mistake is that
// the operator forgot the word "field". The complaint has to land where the
// mistake is.
func TestAHashTakesNoIndex(t *testing.T) {
	look := &looked{}
	out := typed(t, look, `hash books by_shelf field body limit 10`)
	if len(look.asked) != 0 {
		t.Fatalf("a hash with an index in it went out as %+v", look.asked[0])
	}
	if !strings.Contains(out, `there is no "by_shelf" in a hash`) {
		t.Errorf("the complaint reads: %s", out)
	}
	if !strings.Contains(out, "read as: hash books") {
		t.Errorf("the complaint does not say what the line was understood to be: %s", out)
	}
}

// TestAHashSaysWhatItIsMissing: every word of this command that takes exactly
// one value says so when it is given none, rather than reading the next
// keyword as its value.
func TestAHashSaysWhatItIsMissing(t *testing.T) {
	for _, one := range []struct {
		line   string
		saying string
	}{
		{`hash books field limit 10`, "field what?"},
		{`hash books field body decode limit 10`, "decode what?"},
		{`hash`, "hash what? name a collection"},
	} {
		look := &looked{}
		out := typed(t, look, one.line)
		if len(look.asked) != 0 {
			t.Errorf("%q went out as %+v", one.line, look.asked[0])
		}
		if !strings.Contains(out, one.saying) {
			t.Errorf("%q complained %q, and does not say %q", one.line, strings.TrimSpace(out), one.saying)
		}
	}
}

// TestAHashPrintsTheDigestAndWhatItCost: the digest on a line of its own,
// because it is about to be compared with what sha256sum printed, and the row
// count under it, because thirty-two bytes say nothing about how far the walk
// went and nothing else here would.
func TestAHashPrintsTheDigestAndWhatItCost(t *testing.T) {
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	look := &looked{answer: wire.Explored{
		Draft:  store.Operation{Action: store.ActionHashRange},
		Result: store.Result{Digest: digest, Count: 5000},
	}}
	out := typed(t, look, `hash books field body decode base64 limit 5000`)

	if !strings.Contains(out, "  "+digest+"\n") {
		t.Errorf("the digest is not on a line of its own: %q", out)
	}
	if !strings.Contains(out, "5000 rows") {
		t.Errorf("the shell does not say how far it walked: %q", out)
	}
	// And it does not fall through to the empty-scan branch, which would tell
	// an operator holding a correct digest that there was nothing there.
	if strings.Contains(out, "nothing") {
		t.Errorf("a digest was printed as an empty read: %q", out)
	}
}

// TestAHashLineEndsInADeclaration: the shell's promise is that looking around
// produces something to commit, and it holds for this command too — including
// the two words that are the whole reason the action needs a declaration.
func TestAHashLineEndsInADeclaration(t *testing.T) {
	look := &looked{answer: wire.Explored{
		Draft: store.Operation{
			Name: "books.hash", Collection: "books", Action: store.ActionHashRange,
			Field: "body", Decode: "base64", Limit: 5000,
		},
	}}
	out := typed(t, look, `hash books field body decode base64 limit 5000`, `declare`)

	for _, want := range []string{
		`"action": "hashRange"`,
		`"field": "body"`,
		`"decode": "base64"`,
		`"limit": 5000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the draft does not carry %s:\n%s", want, out)
		}
	}
}

// TestTheHelpSaysTheHashRefusesRatherThanStoppingShort: the one thing an
// operator has to know before typing this that they do not have to know for a
// scan.
//
// A scan's limit is where it stops; a hash's limit is where it gives up. An
// operator who reads this command as "a scan that prints a hash" will type
// limit 100 out of habit, get a refusal, and have no idea why.
func TestTheHelpSaysTheHashRefusesRatherThanStoppingShort(t *testing.T) {
	look := &looked{}
	out := typed(t, look, "help")

	if !strings.Contains(out, "hash <collection>") {
		t.Errorf("the help does not list the command:\n%s", out)
	}
	if !strings.Contains(out, "refuses at that number rather than hashing part") {
		t.Errorf("the help does not say the limit refuses:\n%s", out)
	}
}
