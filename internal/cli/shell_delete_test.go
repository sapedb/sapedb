package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// TestWhatADeleteLineGoesOutAs is the delete command's half of
// TestWhatWasTypedIsWhatGoesOut, and it matters more here than for any other
// command in this shell.
//
// A scan that went out with a bound dropped hands back rows somebody can look
// at and disbelieve. A delete that went out with a bound dropped has already
// removed them.
func TestWhatADeleteLineGoesOutAs(t *testing.T) {
	for _, one := range []struct {
		line   string
		wanted store.Access
	}{
		{`delete books limit 10`, store.Access{
			Kind: "delete", Collection: "books", Limit: 10,
		}},
		// The same bounds every other walking command takes, in any order,
		// inclusive and not.
		{`delete books from "b/000000" to "b/000099" limit 100`, store.Access{
			Kind: "delete", Collection: "books", Limit: 100,
			From: &store.Bound{Values: []any{"b/000000"}},
			To:   &store.Bound{Values: []any{"b/000099"}},
		}},
		{`delete books after "b/000000" before "b/000099" limit 100`, store.Access{
			Kind: "delete", Collection: "books", Limit: 100,
			From: &store.Bound{Values: []any{"b/000000"}, Exclusive: true},
			To:   &store.Bound{Values: []any{"b/000099"}, Exclusive: true},
		}},
		// `after` is the word the second page of a paged delete is typed
		// with, so it is here as the shape an operator actually repeats.
		{`delete books after "b/000249" limit 250`, store.Access{
			Kind: "delete", Collection: "books", Limit: 250,
			From: &store.Bound{Values: []any{"b/000249"}, Exclusive: true},
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

// TestADeleteTakesNoIndex: a deleteRange removes a stretch of keys, so the
// bare word a scan reads as an index has nowhere to go in a delete line.
//
// Read as an index it would go out as an access the store then refuses, with a
// message about index order, for a line whose actual mistake is something
// else. The complaint has to land where the mistake is — and on this command
// the complaint has to land before anything goes.
func TestADeleteTakesNoIndex(t *testing.T) {
	look := &looked{}
	out := typed(t, look, `delete books by_shelf limit 10`)
	if len(look.asked) != 0 {
		t.Fatalf("a delete with an index in it went out as %+v", look.asked[0])
	}
	if !strings.Contains(out, `there is no "by_shelf" in a delete`) {
		t.Errorf("the complaint reads: %s", out)
	}
	if !strings.Contains(out, "read as: delete books") {
		t.Errorf("the complaint does not say what the line was understood to be: %s", out)
	}
}

// TestADeletePrintsHowManyWentAndWhereItGotTo: the three things that make the
// answer a cursor rather than a number.
//
// How many went, always, including zero. The last key removed, as JSON,
// because it is the value the operator is about to paste after `after` on the
// next line. And the sentence that says there is a next line at all — without
// which somebody pages once and believes they are done.
func TestADeletePrintsHowManyWentAndWhereItGotTo(t *testing.T) {
	look := &looked{answer: wire.Explored{
		Draft:  store.Operation{Action: store.ActionDeleteRange},
		Result: store.Result{Changed: 250, Key: "b/000249", Truncated: true},
	}}
	out := typed(t, look, `delete books limit 250`)

	if !strings.Contains(out, "250 removed") {
		t.Errorf("the shell does not say how many went: %q", out)
	}
	if !strings.Contains(out, `last "b/000249"`) {
		t.Errorf("the shell does not say where it got to, so there is nothing to continue from: %q", out)
	}
	if !strings.Contains(out, "and more: this stopped at the limit") {
		t.Errorf("the shell does not say more remained: %q", out)
	}
	// And it does not fall through to the empty-scan branch, which would tell
	// an operator who just removed two hundred and fifty rows that there was
	// nothing there.
	if strings.Contains(out, "nothing") {
		t.Errorf("a delete was printed as an empty read: %q", out)
	}
}

// TestADeleteThatRemovedNothingSaysSo: an absent line reads as a success
// nobody counted, and on this command the difference between "removed 0" and
// silence is the difference between a range that was already clear and a
// bound that was typed wrong.
func TestADeleteThatRemovedNothingSaysSo(t *testing.T) {
	look := &looked{answer: wire.Explored{
		Draft:  store.Operation{Action: store.ActionDeleteRange},
		Result: store.Result{},
	}}
	out := typed(t, look, `delete books limit 10`)

	if !strings.Contains(out, "0 removed") {
		t.Errorf("a delete that removed nothing printed: %q", out)
	}
	if strings.Contains(out, "and more") {
		t.Errorf("a finished delete said more remained: %q", out)
	}
}

// TestADeleteLineEndsInADeclaration: the shell's promise is that looking
// around produces something to commit, and cleaning up is the case where that
// matters most — the stretch somebody removed by hand at two in the morning is
// the stretch that will need removing again.
func TestADeleteLineEndsInADeclaration(t *testing.T) {
	look := &looked{answer: wire.Explored{
		Draft: store.Operation{
			Name: "books.delete", Collection: "books", Action: store.ActionDeleteRange,
			Limit: 250,
		},
	}}
	out := typed(t, look, `delete books limit 250`, `declare`)

	for _, want := range []string{
		`"action": "deleteRange"`,
		`"limit": 250`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the draft does not carry %s:\n%s", want, out)
		}
	}
}

// TestTheHelpSaysADeleteCostsALogEntryPerRow: the one thing an operator has to
// know before typing this that the name does not tell them.
//
// "Delete a range" sounds like moving a boundary. It is not: every row removed
// is one entry in the change log, the same entry deleting it on its own would
// write, and no operator can cap that retention today. Somebody clearing ten
// thousand rows should read that here rather than discover it in the log.
func TestTheHelpSaysADeleteCostsALogEntryPerRow(t *testing.T) {
	look := &looked{}
	out := typed(t, look, "help")

	// Searched flat rather than line by line. The help is wrapped to a
	// terminal, so every sentence in it is broken across lines at a column
	// nobody chose on purpose — and a test that greps for a phrase by line
	// passes or fails on where the wrap happened to land rather than on
	// whether the sentence is there.
	flat := strings.Join(strings.Fields(out), " ")

	if !strings.Contains(flat, "delete <collection>") {
		t.Errorf("the help does not list the command:\n%s", out)
	}
	if !strings.Contains(flat, "is one entry in the change log") {
		t.Errorf("the help does not say what a range delete costs:\n%s", out)
	}
	if !strings.Contains(flat, "deciding how much of your data goes") {
		t.Errorf("the help does not say why the limit is required:\n%s", out)
	}
}
