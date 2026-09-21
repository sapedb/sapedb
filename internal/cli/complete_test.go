package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

func library() store.Catalogue {
	return store.Catalogue{
		Collections: []store.Spec{
			{
				Name: "books",
				Key:  store.Key{Path: "id", Type: store.TypeString},
				Indexes: []store.Index{
					{Name: "by_shelf", Fields: []store.Field{{Path: "shelf"}, {Path: "at"}}, Include: []string{"title"}},
					{Name: "by_author", Fields: []store.Field{{Path: "author"}}},
				},
			},
			{Name: "borrowers", Key: store.Key{Path: "id", Type: store.TypeString}},
		},
		Operations: []store.Operation{
			{Name: "books.add"},
			// A namespaced name with arguments, because that is what an
			// installed module looks like and it is the shape the colon
			// could have broken.
			{Name: "library:books.get", Input: []store.Parameter{
				{Name: "id", Type: store.TypeString, Required: true},
			}},
			{Name: "library:books.by_author", Input: []store.Parameter{
				{Name: "author", Type: store.TypeString, Required: true},
				{Name: "shelf", Type: store.TypeString},
			}},
		},
	}
}

// TestWhatTheShellOffersIsWhatWillWork: completion here is exact rather than
// approximate, because every access path is declared. Offering something that
// would be refused is worse than offering nothing — it reads as permission.
func TestWhatTheShellOffersIsWhatWillWork(t *testing.T) {
	for _, one := range []struct {
		line   string
		wanted []string
	}{
		// Nothing typed: the commands.
		{``, []string{"count", "declare", "exit", "get", "help", "invoke", "ls", "scan"}},
		{`s`, []string{"scan"}},
		{`c`, []string{"count"}},

		// A collection, and only the ones that are here.
		{`scan `, []string{"books", "borrowers"}},
		{`scan bo`, []string{"books", "borrowers"}},
		{`scan boo`, []string{"books"}},
		{`get `, []string{"books", "borrowers"}},
		{`count `, []string{"books", "borrowers"}},

		// The indexes of that collection, and the keywords.
		{`scan books `, []string{"after", "before", "by_author", "by_shelf", "fields", "from", "limit", "to"}},
		{`scan books by_`, []string{"by_author", "by_shelf"}},
		{`scan borrowers by_`, nil},

		// An index is only offered where an index can go.
		{`scan books by_shelf `, []string{"after", "before", "fields", "from", "limit", "to"}},

		// The fields this collection is known to have: its key, what its
		// indexes are over, and what they carry.
		{`scan books fields `, []string{"after", "at", "author", "before", "from", "id", "limit", "shelf", "title", "to"}},
		{`scan books by_shelf fields sh`, []string{"shelf"}},
		{`scan books fields title `, []string{"after", "at", "author", "before", "from", "id", "limit", "shelf", "title", "to"}},

		// A value is never completed: the shell does not know what keys exist
		// and will not run a query nobody typed to find out.
		{`get books `, nil},
		{`scan books from `, []string{"after", "before", "fields", "from", "limit", "to"}},
		{`scan books limit `, []string{"after", "before", "fields", "from", "limit", "to"}},

		// A collection that is not here completes to nothing rather than to
		// the keywords, which would read as though the name were fine.
		{`scan nothing `, nil},

		// An invoke completes to the names the database holds, and then to
		// the arguments the named declaration holds. A namespaced name
		// completes as one word: the colon is inside it, not a separator
		// this grammar reads.
		{`invoke `, []string{"books.add", "library:books.by_author", "library:books.get"}},
		{`invoke library:`, []string{"library:books.by_author", "library:books.get"}},
		{`invoke library:books.g`, []string{"library:books.get"}},
		{`invoke library:books.by_author `, []string{"author=", "shelf="}},
		{`invoke library:books.by_author author=poe `, []string{"shelf="}},
		{`invoke library:books.by_author author=poe shelf=a `, nil},

		// An operation nobody declared completes to nothing rather than to
		// the arguments of some other one — the same rule a collection that
		// is not here follows, two rows up.
		{`invoke nothing `, nil},

		// And a value is never completed here either: once the "=" has a
		// value being typed after it, there is nothing to offer.
		{`invoke library:books.get id=bk`, nil},

		// And nothing follows the commands that take nothing.
		{`ls `, nil},
		{`help `, nil},
		{`declare `, nil},
	} {
		got := suggest(one.line, library())
		if len(got) == 0 && len(one.wanted) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, one.wanted) {
			t.Errorf("%q\n  offers %v\n  want   %v", one.line, got, one.wanted)
		}
	}
}

// TestOneTabMovesAsFarAsEverybodyAgrees: pressing tab on an ambiguous word
// should fill in the part that is not a choice, and stop where the choice
// begins.
func TestOneTabMovesAsFarAsEverybodyAgrees(t *testing.T) {
	for _, one := range []struct {
		all    []string
		wanted string
	}{
		{[]string{"by_shelf", "by_author"}, "by_"},
		{[]string{"books", "borrowers"}, "bo"},
		{[]string{"books"}, "books"},
		{[]string{"from", "to"}, ""},
		{nil, ""},
	} {
		if got := shared(one.all); got != one.wanted {
			t.Errorf("%v shares %q, want %q", one.all, got, one.wanted)
		}
	}
}

// TestAnEmptyCatalogueOffersTheCommandsAndNothingElse: a shell that could not
// reach the catalogue still works; it just has nothing to offer. Guessing
// names would be worse than silence.
func TestAnEmptyCatalogueOffersTheCommandsAndNothingElse(t *testing.T) {
	empty := store.Catalogue{}
	if got := suggest("", empty); len(got) != 8 {
		t.Errorf("with no catalogue, the commands are %v", got)
	}
	if got := suggest("scan ", empty); len(got) != 0 {
		t.Errorf("with no catalogue, collections are %v", got)
	}
	if got := suggest("scan books ", empty); len(got) != 0 {
		t.Errorf("with no catalogue, a collection nobody declared offered %v", got)
	}
	// And the same for an invoke: a catalogue with no operations in it has
	// no names to offer, and offering the keywords of some other command
	// would read as though a name had been accepted.
	if got := suggest("invoke ", empty); len(got) != 0 {
		t.Errorf("with no catalogue, operations are %v", got)
	}
}

// TestOnePressOfTab is what the terminal callback does, without a terminal.
// The wiring in term.go is deliberately too thin to hold a decision, so this
// is where the decisions are checked.
func TestOnePressOfTab(t *testing.T) {
	for _, one := range []struct {
		line    string
		wanted  string
		at      int
		printed string
	}{
		// One candidate finishes the word and adds the space, because there is
		// nothing left to decide.
		{`scan boo`, `scan books `, 11, ""},
		{`s`, `scan `, 5, ""},

		// Several move as far as they agree and stop where the choice starts.
		{`scan bo`, `scan bo`, 7, "books  borrowers"},
		{`scan books by_`, `scan books by_`, 14, "by_author  by_shelf"},
		{`scan books by`, `scan books by_`, 14, ""},

		// Nothing to fill in: the choices are printed and the line is left
		// exactly as it was.
		{`scan books `, `scan books `, 11, "after  before  by_author  by_shelf  fields  from  limit  to"},
	} {
		screen := &strings.Builder{}
		line, at, ok := completing(one.line, len(one.line), library(), screen)
		if !ok {
			t.Errorf("%q: tab did nothing", one.line)
			continue
		}
		if line != one.wanted || at != one.at {
			t.Errorf("%q became %q at %d, want %q at %d", one.line, line, at, one.wanted, one.at)
		}
		if one.printed == "" && screen.Len() != 0 {
			t.Errorf("%q printed %q when it should have just filled in", one.line, screen.String())
		}
		if one.printed != "" && !strings.Contains(screen.String(), one.printed) {
			t.Errorf("%q printed %q, want something with %q", one.line, screen.String(), one.printed)
		}
	}

	// Nothing to offer at all: tab is not taken, so the terminal does whatever
	// it would otherwise do rather than the shell pretending to have helped.
	if _, _, ok := completing(`get books `, 10, library(), &strings.Builder{}); ok {
		t.Error("tab claimed to complete a value")
	}
}

// TestTabInTheMiddleOfALine: what comes after the cursor is left alone, so
// going back to fix a word does not delete the rest of the line.
func TestTabInTheMiddleOfALine(t *testing.T) {
	line := `scan boo from "history"`
	cursor := len(`scan boo`)

	got, at, ok := completing(line, cursor, library(), &strings.Builder{})
	if !ok {
		t.Fatal("tab did nothing")
	}
	if got != `scan books  from "history"` {
		t.Errorf("the line became %q", got)
	}
	if at != len(`scan books `) {
		t.Errorf("the cursor is at %d, want %d", at, len(`scan books `))
	}
}
