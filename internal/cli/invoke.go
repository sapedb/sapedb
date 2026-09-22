package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sapedb/sapedb/internal/store"
)

// Running a declared operation from the operator's side.
//
// This closes the one hole the rest of this tool left open. The product's
// whole argument is that the only way into a database is a named operation
// somebody declared; until this existed, the tool that ships with it could
// list those operations and could not run one. `get`, `scan` and `count` are
// not a substitute: they are typed accesses the shell composes on the spot,
// they cannot insert, update, delete or run a batch, and they answer a
// question the installed module never declared. So a stranger who installed
// examples/library and typed `ls` saw nine operations and had nowhere to go
// without writing Go or TypeScript.
//
// It is a thin thing on purpose: find the declaration, read the arguments the
// way that declaration says to read them, and hand the pair to the Invoke the
// client already has. Nothing here decides anything — the server binds and
// checks the arguments again on the far side, and it is the authority. What
// this adds is that the operator does not have to guess an encoding, and that
// a mistake is refused here, by name, with the declaration in hand to say what
// was expected, instead of coming back as a sentence about a map.

// invokeHelp is the `invoke` half of the shell's help, kept next to the code
// it describes rather than inside shellHelp's block, which is about a
// different grammar.
const invokeHelp = `  An invoke takes the arguments its declaration names, written arg=value,
  in any order. A string is written as it stands (id=bk-1); a number, a
  true or false, and an argument declared "any" are written as JSON.
  The name is one word, so a namespaced one is typed whole: library:books.get.
`

// invoking runs one `invoke` line.
//
// The catalogue is fetched here rather than reused from the one the shell
// opened with, because a module can be installed while a session is open and
// an operator whose first move is `install` in another window should not have
// to restart the shell to reach what arrived. It costs one round trip per
// invoke, which is the same order as the invoke itself.
func invoking(look Looking, words []string, out io.Writer) error {
	if len(words) < 2 {
		return fmt.Errorf("invoke what? name an operation — `ls` says which are here")
	}
	name := words[1]

	here, err := look.WhatIsHere()
	if err != nil {
		return err
	}
	operation, found := declared(here, name)
	if !found {
		// By name, and pointing at the one command that answers it. A shell
		// that sent an unknown name to the server anyway would get the same
		// refusal a beat later, with a Go package prefix on it.
		return fmt.Errorf("there is no operation called %q here; `ls` says which there are", name)
	}

	arguments, err := argumentsFor(operation, words[2:])
	if err != nil {
		return err
	}

	result, err := look.Invoke(name, arguments)
	if err != nil {
		return err
	}
	showInvoked(operation, result, out)
	return nil
}

// checkInvoke is the non-interactive command's argument check: a host, an
// operation, and after those only NAME=VALUE or -insecure.
//
// The host is required here although `shell` defaults it, and that is the
// colon earning its keep rather than an inconsistency. An operation name may
// hold a colon — library:books.get — so an optional leading host would make
// the first word ambiguous between a host:port and a namespaced name, decided
// by whichever one happened to be declared. A script names the server it
// writes to; making that explicit costs one word and removes the only place
// in this grammar where a colon could have meant two things.
func checkInvoke(_ options, args []string) error {
	rest, _ := withoutInsecure(args)
	if len(rest) < 2 {
		return fmt.Errorf("%w: invoke takes host:port, an operation, and its arguments written NAME=VALUE", ErrUsage)
	}
	for _, arg := range rest[2:] {
		if strings.Contains(arg, "=") {
			continue
		}
		return fmt.Errorf("%w: invoke takes arguments written NAME=VALUE, not %q", ErrUsage, arg)
	}
	return nil
}

// withoutInsecure takes the one flag this command has out of the argument
// list, wherever it was written, and hands back the rest in order.
//
// Anywhere, because `shell` takes it after the host and an operator who has
// typed that will type it here. The three remaining words are then
// positional — host, operation, then NAME=VALUE — which is what keeps a
// namespaced name unambiguous: nothing here has to guess whether a word
// holding a colon is a host or an operation, because its position says.
//
// The price is that an operation whose declared name is literally
// "-insecure" cannot be run from this command. It can from the shell, where
// there are no flags at all.
func withoutInsecure(args []string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	insecure := false
	for _, arg := range args {
		if arg == "-insecure" {
			insecure = true
			continue
		}
		rest = append(rest, arg)
	}
	return rest, insecure
}

// invokeOnce is the whole of the non-interactive command: connect, run the one
// line, print what came back.
//
// It builds the same words the shell would have read and calls the same
// function, so there is one argument grammar and one printer rather than two
// that agree today. The only thing this adds over typing it is the exit
// status.
func invokeOnce(opts options, args []string, out io.Writer) error {
	rest, insecure := withoutInsecure(args)
	words := append([]string{"invoke"}, rest[1:]...)

	client, err := connect(opts, rest[0], insecure)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	return invoking(client, words, out)
}

// declared finds an operation in the catalogue by its exact name.
//
// Exact, never a prefix and never case-folded: two declarations whose names
// differ by a character are two operations, and a shell that picked the
// nearest would run something nobody typed.
func declared(here store.Catalogue, name string) (store.Operation, bool) {
	for _, operation := range here.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return store.Operation{}, false
}

// argumentsFor reads the typed words as the arguments this declaration names.
//
// Three refusals, and all three are the point of the function rather than
// politeness around it:
//
//   - a word that is not name=value, or a name the declaration does not
//     have, is refused rather than dropped. An argument silently ignored is
//     a call that did something other than what was typed, which is the
//     failure mode this repository keeps finding in other people's code and
//     is not about to add one of its own.
//   - a value that is not the declared type is refused here, saying which
//     argument and what it is, rather than sent and refused there.
//   - a required argument that is missing is named. Not "the arguments do
//     not match": which one.
//
// The server checks all three again — see store.bind, which is the authority
// and stays it. This is not a substitute for that check. It is what makes
// `id=bk-1` mean the string "bk-1" in the first place: the type comes from the
// declaration, so the operator never writes an encoding.
func argumentsFor(operation store.Operation, words []string) (map[string]any, error) {
	declared := make(map[string]store.Parameter, len(operation.Input))
	for _, parameter := range operation.Input {
		declared[parameter.Name] = parameter
	}

	given := make(map[string]any, len(words))
	for _, word := range words {
		// The FIRST separator, so a value may hold one: `q=a=b` is the
		// argument q carrying "a=b". The name may not hold one, which is
		// what makes this unambiguous in the direction that matters.
		at := strings.Index(word, "=")
		if at < 0 {
			return nil, fmt.Errorf("there is no %q in an invoke — an argument is written name=value, and %s takes %s",
				word, quoted(operation.Name), takes(operation))
		}
		name, written := word[:at], word[at+1:]

		parameter, names := declared[name]
		if !names {
			return nil, fmt.Errorf("%s does not take %s; it takes %s",
				quoted(operation.Name), quoted(name), takes(operation))
		}
		if _, twice := given[name]; twice {
			// Last-one-wins would mean a line that names an argument twice
			// runs with a value the operator can see was overruled.
			return nil, fmt.Errorf("%s was given twice", quoted(name))
		}

		value, err := valueFor(parameter, written)
		if err != nil {
			return nil, err
		}
		given[name] = value
	}

	// In declared order, so the same incomplete line always names the same
	// missing argument.
	for _, parameter := range operation.Input {
		if !parameter.Required {
			continue
		}
		if _, ok := given[parameter.Name]; !ok {
			return nil, fmt.Errorf("%s needs %s, which is a %s",
				quoted(operation.Name), quoted(parameter.Name), parameter.Type)
		}
	}

	return given, nil
}

// valueFor reads one written value as the type its parameter declares.
//
// The declaration is what decides the encoding, which is the whole reason
// this is not the JSON-everywhere rule `get` and `scan` use. There, a key
// could be the string "12" or the number 12 and nothing on the line says
// which, so the operator has to. Here the declaration already said, so asking
// again would be asking somebody to repeat something the database knows —
// and quoting inside a shell is where people make the mistake.
func valueFor(parameter store.Parameter, written string) (any, error) {
	notOne := func() error {
		return fmt.Errorf("%s is a %s, and %s is not one",
			quoted(parameter.Name), parameter.Type, quoted(written))
	}

	switch parameter.Type {
	case store.TypeString:
		// Verbatim. A declared string needs no quotes and gets none: the
		// declaration said string, so there is nothing left to disambiguate.
		return written, nil

	case store.TypeNumber:
		// Carried as written, not turned into a float64 here (ISS-35).
		//
		// This used to unmarshal into a float64, and that rounded the value
		// before it ever reached the wire: typing n=9007199254740993 handed
		// the daemon 9007199254740992, which the daemon then quite correctly
		// accepted, because by then it was told nothing else. The server's
		// refusal is the authority — this file's own opening comment says so
		// — and a refusal the shipped tool can never reach is not one. So the
		// digits go out as the operator typed them and the far side decides.
		//
		// A courier that rewrites the parcel is the bug, not the fix.
		value, err := carried(written)
		if err != nil {
			return nil, notOne()
		}
		switch value.(type) {
		case json.Number:
			return value, nil
		case nil:
			// `null` unmarshalled into a float64 left that float64 at its
			// zero value, so zero is what this branch has always sent for it.
			// Unchanged on purpose: ISS-35 is about numbers that are quietly
			// rounded, and widening this one would be a second change hiding
			// inside the first.
			return float64(0), nil
		}
		return nil, notOne()

	case store.TypeBool:
		var value bool
		if err := json.Unmarshal([]byte(written), &value); err != nil {
			return nil, notOne()
		}
		return value, nil

	case store.TypeAny:
		// The one type where the operator does have to say: "any" is exactly
		// the declaration that did not.
		//
		// Read with the same courier rule as a declared number, and for the
		// stronger version of the same reason: an "any" argument is usually a
		// whole document, so the numbers at risk here are nested ones nobody
		// is looking at while they type.
		value, err := carried(written)
		if err != nil {
			return nil, fmt.Errorf("%s is declared any, so its value is written as JSON, and %s is not: a string goes in quotes",
				quoted(parameter.Name), quoted(written))
		}
		return value, nil
	}

	// Unreachable through a declaration this database stored — store's own
	// validateOperation refuses any other type at declare time — and here
	// anyway, because the alternative is a silent nil going out as a value.
	return nil, fmt.Errorf("%s is declared %s, which this shell does not know how to write",
		quoted(parameter.Name), quoted(parameter.Type))
}

// carried reads one written JSON value without turning its numbers into
// float64s, so that what goes on the wire is what was typed (ISS-35).
//
// Every number in the result — top level or nested — is a json.Number, which
// encoding/json writes back out as the digits it was given. The server decodes
// the same way and is the one that decides whether a value it cannot store is
// refused; see internal/server/number.go.
//
// It keeps json.Unmarshal's refusal of trailing bytes, which json.Decoder does
// not have on its own: `n=1 2` was two things and one of them was being
// dropped, and that is not something to lose while fixing a different silence.
func carried(written string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(written))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("%s is more than one value", quoted(written))
	}
	return value, nil
}

// takes is the arguments a declaration names, as they would be typed.
func takes(operation store.Operation) string {
	if len(operation.Input) == 0 {
		return "no arguments"
	}
	written := make([]string, 0, len(operation.Input))
	for _, parameter := range operation.Input {
		one := parameter.Name + "=<" + parameter.Type + ">"
		if !parameter.Required {
			one = "[" + one + "]"
		}
		written = append(written, one)
	}
	return strings.Join(written, " ")
}

// showInvoked prints what an operation answered.
//
// A read goes through showResult, the same function `get`, `scan`, `count`
// and `hash` print through, so there is one table style in this shell rather
// than two that agree until somebody changes one of them. A write has nothing showResult
// can say — it returns no rows, and printing "nothing" for an insert that
// worked would read as though it had not — so those two lines are here.
func showInvoked(operation store.Operation, result store.Result, out io.Writer) {
	switch operation.Action {
	case store.ActionGet, store.ActionScan, store.ActionCount, store.ActionTotals,
		store.ActionHashRange, store.ActionDeleteRange:
		// A deleteRange is in this list although it writes, because it is the
		// one write that has something to print beyond "changed N": a cursor.
		// Printing it through the lines below would drop the "and more"
		// sentence, and an operator who could not tell a finished range from
		// a stopped one would stop paging one page early.
		showResult(operation.Action, result, out)
		return
	}

	if result.Key != nil {
		encoded, err := json.Marshal(result.Key)
		if err != nil {
			fmt.Fprintf(out, "  %v\n", err)
		} else {
			// JSON, unlike the values above: a key that came back is a value
			// somebody may have to type into a `get`, and it is the place
			// where "12" and 12 are a real difference again.
			fmt.Fprintf(out, "  key %s\n", encoded)
		}
	}
	// Always, including zero. An operation that declared itself a write and
	// changed nothing is a fact worth seeing, and an absent line reads as a
	// success nobody counted.
	fmt.Fprintf(out, "  changed %d\n", result.Changed)
}
