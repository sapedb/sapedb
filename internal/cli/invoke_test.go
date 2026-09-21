package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// declarations is what the shell reads an `invoke` line against.
//
// Written by hand here, and deliberately NOT read from examples/library or
// from anything the code under test also reads: a test whose expectation
// comes out of the same place as the behaviour is a string compared against
// itself. Every argument name, type and required flag below is typed out, and
// the assertions further down name them again in literals.
func declarations() store.Catalogue {
	return store.Catalogue{
		Operations: []store.Operation{
			{
				Name:       "library:books.get",
				Collection: "books",
				Action:     store.ActionGet,
				Input: []store.Parameter{
					{Name: "id", Type: store.TypeString, Required: true},
				},
			},
			{
				Name:       "library:books.shelve",
				Collection: "books",
				Action:     store.ActionInsert,
				Input: []store.Parameter{
					{Name: "id", Type: store.TypeString, Required: true},
					{Name: "copies", Type: store.TypeNumber, Required: true},
					{Name: "on_loan", Type: store.TypeBool, Required: true},
					{Name: "note", Type: store.TypeAny},
				},
			},
			{
				Name:       "books.count",
				Collection: "books",
				Action:     store.ActionCount,
				Input:      []store.Parameter{},
			},
			{
				Name:       "books.tidy",
				Collection: "books",
				Action:     store.ActionUpdate,
				Input: []store.Parameter{
					{Name: "id", Type: store.TypeString, Required: true},
					{Name: "shelf", Type: store.TypeString},
				},
			},
		},
	}
}

// TestAnInvokeIsTheCallTheDeclarationDescribes is the positive half, and it
// is what makes every refusal below mean something: a suite that only
// asserted refusals would stay green if invoke refused everything.
//
// It checks the call that went out, not the line that came in — the name and
// the arguments as a map, with the Go types the wire will carry. A value's
// TYPE is the whole point of reading it through the declaration: "1" and 1
// are a string and a float64 and the store's own matches() tells them apart.
func TestAnInvokeIsTheCallTheDeclarationDescribes(t *testing.T) {
	for _, one := range []struct {
		line      string
		name      string
		arguments map[string]any
	}{
		// A namespaced name is one word with a colon in it, and the arguments
		// after it are read the same as for any other name. This is the row
		// that breaks if the colon is ever treated as a separator.
		{
			`invoke library:books.get id=bk-1`,
			"library:books.get",
			map[string]any{"id": "bk-1"},
		},
		// A declared string is written as it stands. No quotes, because the
		// declaration already said string.
		{
			`invoke books.tidy id=bk-1 shelf=history`,
			"books.tidy",
			map[string]any{"id": "bk-1", "shelf": "history"},
		},
		// An optional argument nobody wrote is not sent as null. It is not
		// sent, which is what lets the declaration's own default stand.
		{
			`invoke books.tidy id=bk-1`,
			"books.tidy",
			map[string]any{"id": "bk-1"},
		},
		// The three coerced types, and "any" as JSON. float64 and bool, not
		// the strings "2" and "false".
		{
			`invoke library:books.shelve id=bk-9 copies=2 on_loan=false note={"by":"ada"}`,
			"library:books.shelve",
			map[string]any{
				"id": "bk-9", "copies": 2.0, "on_loan": false,
				"note": map[string]any{"by": "ada"},
			},
		},
		// A string that looks like a number stays a string, because that is
		// what was declared. The store's matches() would refuse the float.
		{
			`invoke library:books.get id=12`,
			"library:books.get",
			map[string]any{"id": "12"},
		},
		// Order is the operator's, not the declaration's.
		{
			`invoke library:books.shelve on_loan=true copies=0 note=null id=bk-2`,
			"library:books.shelve",
			map[string]any{"id": "bk-2", "copies": 0.0, "on_loan": true, "note": nil},
		},
		// A value may hold the separators: the name ends at the FIRST "=",
		// and a colon in a value is just a character.
		{
			`invoke books.tidy id=a=b shelf=x:y`,
			"books.tidy",
			map[string]any{"id": "a=b", "shelf": "x:y"},
		},
		// An empty value is the empty string, which is a string.
		{
			`invoke books.tidy id=bk-1 shelf=`,
			"books.tidy",
			map[string]any{"id": "bk-1", "shelf": ""},
		},
		// An operation that declares no arguments takes none, and still runs.
		{
			`invoke books.count`,
			"books.count",
			map[string]any{},
		},
	} {
		t.Run(one.line, func(t *testing.T) {
			look := &looked{here: declarations()}
			printed := typed(t, look, one.line, "exit")

			if len(look.ran) != 1 {
				t.Fatalf("%q made %d calls, want 1; it printed:\n%s", one.line, len(look.ran), printed)
			}
			if look.ran[0].name != one.name {
				t.Errorf("%q called %q, want %q", one.line, look.ran[0].name, one.name)
			}
			if !reflect.DeepEqual(look.ran[0].arguments, one.arguments) {
				t.Errorf("%q sent %#v, want %#v", one.line, look.ran[0].arguments, one.arguments)
			}
		})
	}
}

// TestAnInvokeIsRefusedByName is the refusal half.
//
// Each case asserts two things, and it needs both. The sentence says which
// argument and what was expected, which is the whole complaint against
// "the arguments do not match"; and NOTHING went out, which is what says the
// refusal happened here rather than being a message the server sent back
// after doing something. The positive test above is the other half: without
// it, refusing every line would pass this one.
func TestAnInvokeIsRefusedByName(t *testing.T) {
	for _, one := range []struct {
		what  string
		line  string
		wants []string
	}{
		{
			"a missing required argument",
			`invoke library:books.get`,
			[]string{`"library:books.get"`, `needs "id"`, "string"},
		},
		{
			"a missing required argument among others",
			`invoke library:books.shelve id=bk-1 on_loan=false`,
			[]string{`needs "copies"`, "number"},
		},
		{
			"an argument the declaration does not name",
			`invoke library:books.get id=bk-1 shelf=history`,
			[]string{`does not take "shelf"`, "id=<string>"},
		},
		{
			"a number that is not one",
			`invoke library:books.shelve id=bk-1 copies=lots on_loan=false`,
			[]string{`"copies" is a number`, `"lots" is not one`},
		},
		{
			"a bool that is not one",
			`invoke library:books.shelve id=bk-1 copies=1 on_loan=yes`,
			[]string{`"on_loan" is a bool`, `"yes" is not one`},
		},
		{
			"an any that is not JSON",
			`invoke library:books.shelve id=bk-1 copies=1 on_loan=true note=ada`,
			[]string{`"note" is declared any`, "written as JSON", "a string goes in quotes"},
		},
		{
			"a word that is not an argument at all",
			`invoke library:books.get bk-1`,
			[]string{`there is no "bk-1" in an invoke`, "name=value", "id=<string>"},
		},
		{
			"the same argument twice",
			`invoke library:books.get id=bk-1 id=bk-2`,
			[]string{`"id" was given twice`},
		},
		{
			"an operation nobody declared",
			`invoke library:books.burn id=bk-1`,
			[]string{`there is no operation called "library:books.burn"`, "ls"},
		},
		{
			"a name that is nearly one that is declared",
			`invoke library:books.ge id=bk-1`,
			[]string{`there is no operation called "library:books.ge"`},
		},
		{
			"no name at all",
			`invoke`,
			[]string{"invoke what?", "name an operation"},
		},
	} {
		t.Run(one.what, func(t *testing.T) {
			look := &looked{here: declarations()}
			printed := typed(t, look, one.line, "exit")

			for _, wanted := range one.wants {
				if !strings.Contains(printed, wanted) {
					t.Errorf("%q was not refused with %q:\n%s", one.line, wanted, printed)
				}
			}
			// The other half. A refusal that has already run the operation is
			// a refusal about something that happened.
			if len(look.ran) != 0 {
				t.Errorf("%q was refused and still called %#v", one.line, look.ran)
			}
		})
	}
}

// TestWhatAnInvokePrintsIsWhatTheOperationAnswered: a read prints through the
// same function `get`, `scan` and `count` print through, and a write prints
// what a write has to say. An insert that printed "nothing" — which is what a
// read's empty answer prints — would read as a failure that was a success.
func TestWhatAnInvokePrintsIsWhatTheOperationAnswered(t *testing.T) {
	for _, one := range []struct {
		what   string
		line   string
		gave   store.Result
		wants  []string
		unseen []string
	}{
		{
			what:  "a get with a row",
			line:  `invoke library:books.get id=bk-1`,
			gave:  store.Result{Rows: []map[string]any{{"id": "bk-1", "title": "the first"}}},
			wants: []string{`{"id":"bk-1","title":"the first"}`},
		},
		{
			what:  "a get with nothing",
			line:  `invoke library:books.get id=bk-1`,
			gave:  store.Result{},
			wants: []string{"nothing"},
		},
		{
			what:  "a count",
			line:  `invoke books.count`,
			gave:  store.Result{Count: 3},
			wants: []string{"  3\n"},
			// Not the empty-scan sentence: a count of zero is a number.
			unseen: []string{"nothing"},
		},
		{
			what:   "an insert",
			line:   `invoke library:books.shelve id=bk-1 copies=1 on_loan=false`,
			gave:   store.Result{Key: "bk-1", Changed: 1},
			wants:  []string{`key "bk-1"`, "changed 1"},
			unseen: []string{"nothing"},
		},
		{
			what:   "an update that changed nothing",
			line:   `invoke books.tidy id=bk-1`,
			gave:   store.Result{},
			wants:  []string{"changed 0"},
			unseen: []string{"nothing", "key "},
		},
	} {
		t.Run(one.what, func(t *testing.T) {
			look := &looked{here: declarations(), gave: one.gave}
			printed := typed(t, look, one.line, "exit")

			for _, wanted := range one.wants {
				if !strings.Contains(printed, wanted) {
					t.Errorf("the session did not print %q:\n%s", wanted, printed)
				}
			}
			for _, banned := range one.unseen {
				if strings.Contains(printed, banned) {
					t.Errorf("the session printed %q, which it must not:\n%s", banned, printed)
				}
			}
		})
	}
}

// TestAnInvokeThatTheServerRefusesIsNotASession: the far side refuses too,
// and its sentence has to reach the operator instead of being swallowed into
// a blank line. The session carries on afterwards, the same as it does for a
// mistyped bound.
func TestAnInvokeTheServerRefusesIsSaidOutLoud(t *testing.T) {
	look := &looked{here: declarations(), refused: store.ErrNotAllowed}
	printed := typed(t, look, `invoke library:books.get id=bk-1`, `ls`, "exit")

	if !strings.Contains(printed, "may not run this operation") {
		t.Errorf("the server's refusal did not reach the operator:\n%s", printed)
	}
	if len(look.ran) != 1 {
		t.Errorf("the call did not go out: %#v", look.ran)
	}
}

// TestTheCatalogueIsReadAtEveryInvoke: a module installed while a session is
// open is reachable without restarting the shell. The shell asks once at
// startup for completion; an invoke asks again, because the answer can have
// changed since.
func TestTheCatalogueIsReadAtEveryInvoke(t *testing.T) {
	look := &looked{}
	out := &strings.Builder{}

	// Nothing declared when the shell opens: the first line is refused.
	if err := Shell(look, "main", strings.NewReader("invoke library:books.get id=bk-1\n"), out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `there is no operation called "library:books.get"`) {
		t.Fatalf("an operation nobody declared was not refused:\n%s", out)
	}

	// The same line, against a shell whose catalogue arrives only after the
	// session has started.
	arriving := &installing{here: declarations()}
	out = &strings.Builder{}
	if err := Shell(arriving, "main", strings.NewReader("invoke library:books.get id=bk-1\n"), out); err != nil {
		t.Fatal(err)
	}
	if len(arriving.ran) != 1 {
		t.Fatalf("the operation declared after the shell opened was not reachable:\n%s", out)
	}
}

// installing is a Looking whose catalogue is empty the first time it is
// asked — the read the shell does at startup — and full afterwards. That is
// exactly the shape of `install` running in another window.
type installing struct {
	here  store.Catalogue
	asked int
	ran   []invoked
}

func (i *installing) WhatIsHere() (store.Catalogue, error) {
	i.asked++
	if i.asked == 1 {
		return store.Catalogue{}, nil
	}
	return i.here, nil
}

func (i *installing) Explore(store.Access) (wire.Explored, error) { return wire.Explored{}, nil }

func (i *installing) Invoke(name string, arguments map[string]any) (store.Result, error) {
	i.ran = append(i.ran, invoked{name: name, arguments: arguments})
	return store.Result{}, nil
}

// TestCheckInvokeRefusesBeforeAnythingIsOpened pins the non-interactive
// command's argument check, which runs before connect() and therefore before
// anything is dialled. The shapes it must accept are here too: without them
// a check that refused everything would pass.
func TestCheckInvokeRefusesBeforeAnythingIsOpened(t *testing.T) {
	for _, one := range []struct {
		args  []string
		wants string
	}{
		{[]string{}, "invoke takes host:port"},
		{[]string{"localhost:7433"}, "invoke takes host:port"},
		{[]string{"localhost:7433", "library:books.get", "bk-1"}, `not "bk-1"`},
		{[]string{"localhost:7433", "library:books.get", "-insecur"}, `not "-insecur"`},
		// -insecure counts as a flag wherever it is written, so it is not
		// the operation and its presence does not make the count up.
		{[]string{"-insecure", "localhost:7433"}, "invoke takes host:port"},
	} {
		err := checkInvoke(options{}, one.args)
		if err == nil {
			t.Fatalf("%v was accepted", one.args)
		}
		if !strings.Contains(err.Error(), one.wants) {
			t.Errorf("%v was refused with %q, want %q in it", one.args, err, one.wants)
		}
	}

	for _, args := range [][]string{
		{"localhost:7433", "library:books.get"},
		{"localhost:7433", "library:books.get", "id=bk-1"},
		{"localhost:7433", "library:books.get", "-insecure", "id=bk-1"},
		{"localhost:7433", "-insecure", "library:books.get", "id=bk-1"},
		{"-insecure", "localhost:7433", "library:books.get", "id=bk-1"},
		{"localhost:7433", "library:books.get", "id=a=b"},
	} {
		if err := checkInvoke(options{}, args); err != nil {
			t.Errorf("%v was refused: %v", args, err)
		}
	}
}
