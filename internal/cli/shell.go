package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// The shell an operator types at.
//
// What somebody can type here is a list of keywords in fixed positions. That
// is not a simplification of something richer that comes later — it is the
// whole design. A shell with an expression language is a second interface into
// the database, more powerful than the one the application uses and reachable
// by anyone who reaches the port, and the industry's own answer to having
// built one is a configuration flag that turns it off in production.
//
// So: a document by its key, a stretch of a declared index, a count of one, a
// SHA-256 of one, and a list of what is here. The digest is here because an
// operator checking whether a stored file is still intact is exactly the
// person standing at a prompt, and the alternative — copying ten megabytes of
// rows out to somewhere that can hash them — is the thing splitting the file
// up was for. Values are written as JSON, because that is the
// one notation where "12" and 12 are visibly different things, which matters
// when a key might be either.
//
// Every session ends the same way: `declare` prints the operation that would
// do what you just did. Exploring that leaves nothing behind becomes a habit
// of exploring; exploring that ends in a declaration becomes a schema.

const shellHelp = `  ls                                 what this database holds
  invoke <name> [arg=value]...       run a declared operation
  get <collection> <key>             one document by its key
  scan <collection> [index] [...]    a stretch of an index
  count <collection> [index] [...]   how many are in that stretch
  hash <collection> field <p> [...]  SHA-256 of that stretch, joined
  delete <collection> limit <n> [..] remove that stretch, at most n rows
  declare [name]                     the operation that would do the last thing
  help                               this
  exit                               leave

  A scan or a count takes, in any order:
    from <value>...       where to start   (after <value>... to exclude it)
    to <value>...         where to stop    (before <value>... to exclude it)
    limit <n>             how many rows at most
    fields <a> <b>...     which fields to show

  A hash takes the same bounds, in key order and with no index, plus:
    field <path>          which field of each document goes into the digest
    decode base64         read each value as base64 first
    limit <n>             how many rows it may read — required, and it
                          refuses at that number rather than hashing part

  A delete takes the same bounds, in key order and with no index, plus:
    limit <n>             how many rows it may remove — required, because a
                          number this shell picked would be this shell
                          deciding how much of your data goes

  It removes in key order and stops at the limit, printing how many went,
  the last key removed, and whether more remain. Each row removed is one
  entry in the change log, the same entry deleting it on its own would
  write — so a range of ten thousand is ten thousand entries.

  Values are JSON: "a string", 42, true, null.

` + invokeHelp

// Looking is the part of a connection the shell uses. An interface because the
// shell is worth testing without a server, and because what it needs from a
// connection is exactly this much.
type Looking interface {
	WhatIsHere() (store.Catalogue, error)
	Explore(store.Access) (wire.Explored, error)
	// Invoke runs a declared operation, which is the only thing everybody
	// else's client can do and the one thing this shell could not. It is the
	// client's own method rather than a second path built here: the shell
	// composes nothing, it reads a declaration the database already holds and
	// hands the name and the arguments over.
	Invoke(name string, arguments map[string]any) (store.Result, error)
}

// Shell reads lines and runs them until the input ends.
func Shell(look Looking, dbname string, in io.Reader, out io.Writer) error {
	// Asked for once, at the start, because completion needs it and because an
	// operator's first question is always what is here. Best effort: a shell
	// that could not fetch it still works, it just has nothing to offer, and
	// guessing names would be worse than silence.
	here, _ := look.WhatIsHere()

	prompt := dbname + "> "
	next, restore := editing(in, out, prompt, here)
	defer restore()

	// The last draft, which is what `declare` prints. Kept rather than
	// recomputed so that what is printed is what ran, not what a second pass
	// through the parser thinks ran.
	var drafted *store.Operation

	fmt.Fprintf(out, "sapedb %s — type help, or exit when you are done\n", dbname)

	for {
		line, err := next()
		if err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		done, draft, err := one(look, line, drafted, out)
		if err != nil {
			// A mistake is not the end of a session. An operator who mistypes
			// a bound at two in the morning should see why and try again.
			fmt.Fprintf(out, "  %v\n", err)
		}
		if draft != nil {
			drafted = draft
		}
		if done {
			return nil
		}
	}
}

// newScanner is the line reader the plain path uses, with a buffer big enough
// for a pasted line.
func newScanner(in io.Reader) *bufio.Scanner {
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return lines
}

// one runs a single line, and says whether the session is over and what draft
// it produced.
func one(look Looking, line string, drafted *store.Operation, out io.Writer) (bool, *store.Operation, error) {
	words := strings.Fields(line)

	// Extra words are refused everywhere, not only where they could have
	// meant something. `ls something` ran as `ls` until somebody typed it by
	// accident: a shell that ignores what it does not understand does a
	// different thing from the one that was typed, and says nothing about it.
	if most, counted := taken[words[0]]; counted && len(words) > most {
		return false, nil, fmt.Errorf("%q takes nothing after it, and got %q", words[0], words[1])
	}

	switch words[0] {
	case "exit", "quit":
		return true, nil, nil

	case "help", "?":
		fmt.Fprint(out, shellHelp)
		return false, nil, nil

	case "ls":
		here, err := look.WhatIsHere()
		if err != nil {
			return false, nil, err
		}
		showCatalogue(here, out)
		return false, nil, nil

	case "declare":
		if drafted == nil {
			return false, nil, fmt.Errorf("nothing has been looked at yet, so there is nothing to declare")
		}
		named := *drafted
		if len(words) > 1 {
			named.Name = words[1]
		}
		named.Version = 0
		encoded, err := json.MarshalIndent(named, "", "  ")
		if err != nil {
			return false, nil, err
		}
		fmt.Fprintln(out, string(encoded))
		return false, nil, nil

	case "invoke":
		// No draft comes back and `drafted` is left alone on purpose. The
		// other three commands end in a declaration because they ran an
		// access nobody had declared; this one started from one, so there is
		// nothing for `declare` to print that is not already stored.
		return false, nil, invoking(look, words, out)

	case "get", "scan", "count", "hash", "delete":
		access, err := access(words)
		if err != nil {
			return false, nil, err
		}
		answer, err := look.Explore(access)
		if err != nil {
			return false, nil, err
		}
		showResult(answer.Draft.Action, answer.Result, out)
		return false, &answer.Draft, nil
	}

	return false, nil, fmt.Errorf("there is no %q here; type help", words[0])
}

// taken is how many words each command may be, for the ones that are not a
// whole little grammar of their own. A command missing from here takes as many
// as it likes and checks them itself.
var taken = map[string]int{
	"exit": 1, "quit": 1, "help": 1, "?": 1, "ls": 1, "declare": 2,
}

// access turns a typed line into the access it asks for.
//
// Everything this does not understand is refused by name. A shell that
// silently ignores a word it does not know is one that runs a different query
// from the one somebody typed.
func access(words []string) (store.Access, error) {
	asked := store.Access{Kind: words[0]}

	if len(words) < 2 {
		return store.Access{}, fmt.Errorf("%s what? name a collection", words[0])
	}
	asked.Collection = words[1]

	rest := words[2:]

	if asked.Kind == "get" {
		if len(rest) != 1 {
			return store.Access{}, fmt.Errorf("get takes one key, written as JSON: get %s \"some-id\"", asked.Collection)
		}
		key, err := literal(rest[0])
		if err != nil {
			return store.Access{}, err
		}
		asked.Key = key
		return asked, nil
	}

	// A bare word straight after the collection is the index, for the two
	// commands that walk one. A hash and a delete walk key order and nothing
	// else, so for those a bare word here is a mistake and is reported as
	// one rather than carried to a refusal further in. Anything else is a
	// keyword, and the clustered index is what you get by saying nothing.
	//
	// Which means a line that starts like another database's query language —
	// "scan books where shelf = ..." — has its first wrong word read as an
	// index name, and the complaint lands on the second one. So a failure says
	// what the line was understood to be. A parser that reports the wrong word
	// sends somebody looking at the wrong half of what they typed.
	read := asked.Kind + " " + asked.Collection
	if asked.Kind != "hash" && asked.Kind != "delete" && len(rest) > 0 && !keyword(rest[0]) {
		asked.Index = rest[0]
		rest = rest[1:]
		read += ", index " + quoted(asked.Index)
	}

	for len(rest) > 0 {
		word := rest[0]
		rest = rest[1:]

		switch word {
		case "from", "after", "to", "before":
			values, remaining, err := until(rest)
			if err != nil {
				return store.Access{}, fmt.Errorf("%s: %w", word, err)
			}
			if len(values) == 0 {
				return store.Access{}, fmt.Errorf("%s what? give it at least one value", word)
			}
			bound := &store.Bound{Values: values, Exclusive: word == "after" || word == "before"}
			if word == "from" || word == "after" {
				asked.From = bound
			} else {
				asked.To = bound
			}
			rest = remaining

		case "limit":
			if len(rest) == 0 {
				return store.Access{}, fmt.Errorf("limit what? give it a number")
			}
			value, err := literal(rest[0])
			if err != nil {
				return store.Access{}, err
			}
			// literal hands back a json.Number now rather than a float64
			// (ISS-35), so the whole-number check reads the value out of it
			// instead of asserting float64. A limit is a row count this shell
			// turns into an int before anything sees it — not a `number` the
			// database stores — so ISS-35's refusal has nothing to say about
			// it, and what matters here is only that `limit 100` still means
			// a hundred rows, and `limit 1e2` still does too.
			written, isNumber := value.(json.Number)
			if !isNumber {
				return store.Access{}, fmt.Errorf("limit takes a whole number of rows, not %s", rest[0])
			}
			count, notANumber := written.Float64()
			if notANumber != nil || count != float64(int(count)) || count < 1 {
				return store.Access{}, fmt.Errorf("limit takes a whole number of rows, not %s", rest[0])
			}
			asked.Limit = int(count)
			rest = rest[1:]

		case "fields":
			for len(rest) > 0 && !keyword(rest[0]) {
				asked.Projection = append(asked.Projection, rest[0])
				rest = rest[1:]
			}
			if len(asked.Projection) == 0 {
				return store.Access{}, fmt.Errorf("fields what? name at least one")
			}

		case "field":
			if len(rest) == 0 || keyword(rest[0]) {
				return store.Access{}, fmt.Errorf("field what? name the one field that goes into the digest")
			}
			asked.Field = rest[0]
			rest = rest[1:]

		case "decode":
			if len(rest) == 0 || keyword(rest[0]) {
				return store.Access{}, fmt.Errorf("decode what? the only encoding this store reads is base64")
			}
			asked.Decode = rest[0]
			rest = rest[1:]

		default:
			return store.Access{}, fmt.Errorf("there is no %q in a %s — this was read as: %s; type help",
				word, asked.Kind, read)
		}
	}

	return asked, nil
}

// until reads JSON values up to the next keyword.
func until(words []string) ([]any, []string, error) {
	values := []any{}
	for len(words) > 0 && !keyword(words[0]) {
		value, err := literal(words[0])
		if err != nil {
			return nil, nil, err
		}
		values = append(values, value)
		words = words[1:]
	}
	return values, words, nil
}

// quoted is a word as it should appear inside a message about itself.
func quoted(word string) string { return "\"" + word + "\"" }

// keyword is every word that ends a run of values, for every command, rather
// than a set per command. One list because until() and the bare-word-is-an-
// index rule both have to agree with the switch in access() about where one
// clause stops, and three lists that have to agree are two chances to
// disagree.
//
// "field" and "decode" joined it with the hash command, and that narrows the
// other two commands by exactly as much: an index or a projected field
// literally called "field" or "decode" can no longer be typed at this shell.
// Said out loud rather than left to be discovered, and accepted because
// "fields" was already reserved next door to one of them — but it is a
// narrowing, and it is the shell's grammar and not a declaration, which is
// the only reason it is allowed to happen at all.
func keyword(word string) bool {
	switch word {
	case "from", "after", "to", "before", "limit", "fields", "field", "decode":
		return true
	}
	return false
}

// literal reads one value the way it was written.
//
// JSON rather than bare words, so that a key of "12" and a key of 12 are
// different things on the screen as well as in the store — which is exactly
// the confusion somebody debugging at two in the morning does not need.
// Numbers are carried as written, not turned into float64 here (ISS-35), for
// exactly the reason invoke.go's valueFor is: this shell is a courier. It used
// to unmarshal into `any`, which rounded the digits before they reached the
// wire — typing `get ids 9007199254740993` sent the daemon 9007199254740992,
// and the daemon then fetched the document under that key and printed it,
// which is a row belonging to somebody else. The server's refusal is the
// authority, and a refusal the shipped shell can never reach is not one.
//
// A courier that rewrites the parcel is the bug, not the fix.
func literal(word string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(word))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, notAValue(word)
	}
	// json.Unmarshal refuses trailing bytes and json.Decoder does not, so
	// without this `limit 1 2` and `get ids 1 2` would quietly read as their
	// first word. That refusal is not something to lose while fixing a
	// different silence.
	if decoder.More() {
		return nil, notAValue(word)
	}
	return value, nil
}

func notAValue(word string) error {
	return fmt.Errorf("%s is not a value; write a string in quotes, or a number, true, false or null", word)
}

func showResult(action string, result store.Result, out io.Writer) {
	switch action {
	case store.ActionCount:
		fmt.Fprintf(out, "  %d\n", result.Count)

	case store.ActionHashRange:
		// The digest first, on a line of its own, because it is what somebody
		// is about to compare with what `sha256sum` printed and a line with
		// nothing else on it is a line you can select.
		fmt.Fprintf(out, "  %s\n", result.Digest)
		// Then the cost, because the digest is thirty-two bytes whether it
		// walked four rows or four million and nothing else here would ever
		// say which. An operator who typed a limit of 5000 and reads "4 rows"
		// has found the missing file, not a working one.
		fmt.Fprintf(out, "  %d rows\n", result.Count)

	case store.ActionDeleteRange:
		// How many went, always, including zero — an access that destroyed
		// nothing is a fact, and an absent line would read as a success
		// nobody counted.
		fmt.Fprintf(out, "  %d removed\n", result.Changed)
		// Then where it got to, which with the "and more" line below is the
		// cursor: the next line an operator types is the same one with
		// `after <this key>` in place of `from`.
		if result.Key != nil {
			encoded, err := json.Marshal(result.Key)
			if err != nil {
				fmt.Fprintf(out, "  %v\n", err)
			} else {
				fmt.Fprintf(out, "  last %s\n", encoded)
			}
		}

	default:
		if len(result.Rows) == 0 {
			// Not "0". A count answers with a number and a scan answers with
			// rows, and printing a number for an empty scan reads as though
			// the question asked for one.
			fmt.Fprintln(out, "  nothing")
		}
	}
	for _, row := range result.Rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			fmt.Fprintf(out, "  %v\n", err)
			continue
		}
		fmt.Fprintf(out, "  %s\n", encoded)
	}
	if result.Truncated {
		fmt.Fprintf(out, "  ... and more: this stopped at the limit\n")
	}
}

func showCatalogue(here store.Catalogue, out io.Writer) {
	for _, spec := range here.Collections {
		fmt.Fprintf(out, "  collection %s (key %s %s)\n", spec.Name, spec.Key.Path, spec.Key.Type)
		for _, index := range spec.Indexes {
			fields := make([]string, 0, len(index.Fields))
			for _, field := range index.Fields {
				fields = append(fields, field.Path)
			}
			fmt.Fprintf(out, "    index %s (%s)\n", index.Name, strings.Join(fields, ", "))
		}
	}

	// Keyed by name and version rather than read by position: Catalogue's own
	// doc only promises Envelopes rides alongside Operations in the same
	// order, and the fake Looking this package's own tests hand the shell is
	// not bound by that promise either. A catalogue with no envelope for an
	// operation (a Looking that predates SAPE-8, or simply none supplied)
	// still shows the operation, just without the lines below it.
	envelopes := make(map[string]store.Envelope, len(here.Envelopes))
	for _, envelope := range here.Envelopes {
		envelopes[envelopeKey(envelope.Operation, envelope.Version)] = envelope
	}

	for _, operation := range here.Operations {
		fmt.Fprintf(out, "  operation %s v%d %s %s\n",
			operation.Name, operation.Version, operation.Action, operation.Collection)

		envelope, found := envelopes[envelopeKey(operation.Name, operation.Version)]
		if !found {
			continue
		}
		// The SAPE-8 facts a caller needs before running an operation it did
		// not write, without running it: the real ceiling (never the
		// declaration's own Limit field, which is ambiguous or absent for
		// exactly the actions this matters most for — see Envelope's own
		// doc), and everything it reaches through composed steps rather than
		// only what its own Collection/Index fields name.
		fmt.Fprintf(out, "    limit %d, collections [%s], indexes [%s]\n",
			envelope.Limit, strings.Join(envelope.Collections, ", "), strings.Join(envelope.Indexes, ", "))
		switch {
		case envelope.WholeDocument:
			fmt.Fprintln(out, "    whole document escapes")
		case len(envelope.Projection) > 0:
			fmt.Fprintf(out, "    projection [%s] escapes\n", strings.Join(envelope.Projection, ", "))
		default:
			fmt.Fprintln(out, "    nothing escapes")
		}
		if len(envelope.Scopes) > 0 {
			fmt.Fprintf(out, "    scopes %s\n", strings.Join(envelope.Scopes, ", "))
		}
	}
}

// envelopeKey is how showCatalogue matches an Envelope to the Operation it
// describes: the same (name, version) pair Envelope.Operation/Version and
// Operation.Name/Version both carry.
func envelopeKey(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}

// connect makes this tool's own connection string and opens it.
//
// Nothing is typed and nothing is passed as an argument. The tool already
// holds the secret — that is what makes it the operator — so it signs a string
// for itself with a password it just generated, which lives for one
// connection. A password on a command line is visible to anyone who can run
// ps, and a shell that asked for one would be teaching the habit of pasting
// credentials into a terminal.
func connect(opts options, address string, insecure bool) (*wire.Client, error) {
	password, err := makePassword()
	if err != nil {
		return nil, err
	}
	signature, err := signing.Sign(
		signing.Parts{AccountID: opts.account, Password: password, DBName: opts.db},
		opts.secret, opts.label,
	)
	if err != nil {
		return nil, err
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrUsage, address)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a port", ErrUsage, port)
	}

	client, err := wire.Dial(connection.Connection{
		Account: opts.account, Password: password, Host: host, Port: number,
		DBName: opts.db, Signature: signature,
	}, wire.Options{Insecure: insecure})
	if err != nil {
		return nil, err
	}

	// Being able to reach a database is not being allowed to explore it. This
	// proves the secret over a challenge the server just chose.
	if err := client.Operate(opts.secret); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// checkShell is shell's argument check, moved here verbatim from what used
// to be shell()'s own body: everything after the host, if anything, has to
// be "-insecure". Guarded on len(args) > 0 because args[1:] on an empty
// slice is one index past its length — a case this function has to survive
// now that check runs for every command, including one given no arguments
// at all, and not only for the two shapes ("shell host" and "shell host
// -insecure") this package's own tests happened to call it with before.
func checkShell(_ options, args []string) error {
	if len(args) == 0 {
		return nil
	}
	for _, arg := range args[1:] {
		if arg != "-insecure" {
			return fmt.Errorf("%w: shell takes host:port and optionally -insecure, not %q", ErrUsage, arg)
		}
	}
	return nil
}

// connectedLine is what a session says about the server it reached, given the
// product version that server declared in its welcome.
//
// A function of a string, separate from shell(), because the interesting case
// is the one shell() cannot be made to produce from inside this repository: a
// server that sends no product version at all. Every server this tree can
// start sends one (see server.New), so the empty case would otherwise be a
// branch nothing here could reach — and it is exactly the case that matters,
// because it is what a build from before that field looks like.
func connectedLine(version string) string {
	if version == "" {
		// Not an error. A server from before the welcome carried this is a
		// server this shell can still talk to, and saying so is more use
		// than printing nothing and letting the absence read as a version.
		return "connected to a server that does not say which build it is"
	}
	return "connected to sapedb " + version
}

// shell is the command.
func shell(opts options, args []string, in io.Reader, out io.Writer) error {
	address := "localhost:7433"
	if len(args) > 0 {
		address = args[0]
	}
	// checkShell has already refused anything in args[1:] that is not
	// "-insecure", so its mere presence is enough to turn this on.
	insecure := len(args) > 1

	client, err := connect(opts, address, insecure)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Said before the shell's own banner, because it is the one fact about
	// this session that the operator cannot get from anywhere else once the
	// session is over. The server's build, not this tool's: the two are
	// deployed separately and a report that names only the tool names the
	// wrong half. Printed rather than merely available on the client, so
	// that it is in the transcript an operator pastes into a bug report
	// without having had to think of asking for it.
	fmt.Fprintln(out, connectedLine(client.Welcome().ProductVersion))

	return Shell(client, opts.db, in, out)
}
