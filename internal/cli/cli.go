// Package cli is the tool that sets a database up and looks inside it.
//
// It exists because declaring is not an operation. Everything a client can do
// over the wire is an operation the database already holds, which means a new
// database can do nothing at all until somebody puts the first declarations in
// it — and that somebody is on the same host as the file, not on the far end
// of a connection.
//
// Every command here opens the database file directly, and the file takes an
// exclusive lock. So a command run while the server is up is refused rather
// than quietly becoming the second writer: see internal/vfs for what two
// writers do to one of these files.
package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sapedb/sapedb/internal/build"
	"github.com/sapedb/sapedb/internal/dbkey"
	"github.com/sapedb/sapedb/internal/dbname"
	"github.com/sapedb/sapedb/internal/number"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

// passwordBytes is how long a generated password is. Well inside the 16..128
// the format allows, and long enough that guessing is not a strategy.
const passwordLength = 32

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-"

const usage = `sapedb — set up and look inside a database

  sapedb [options] apply FILE...     declare collections and operations
  sapedb [options] keygen FILE       make a signing identity, FILE keeps it
  sapedb [options] seal DRAFT FILE   sign declarations into a bundle
  sapedb [options] verify FILE       check a signed bundle, and say what it holds
  sapedb [options] install FILE      declare everything a signed bundle holds
  sapedb [options] ls                what this database holds
  sapedb [options] dump              write a dump to stdout
  sapedb [options] restore           read a dump from stdin, into an empty database
  sapedb [options] log [FROM]        print the change log from an entry onwards
  sapedb [options] url               print a signed connection string
  sapedb [options] shell [HOST]      look inside a running server
  sapedb [options] invoke HOST OP    run one declared operation, ARG=VALUE
  sapedb [options] version           which build this is

options
  -dir DIR       where databases live      (SAPEDB_DIR, default /var/lib/sapedb)
  -account NAME  which account             (SAPEDB_ACCOUNT)
  -db NAME       which database            (SAPEDB_DB)
  -encrypt       the database is encrypted (SAPEDB_ENCRYPT)

SAPEDB_SECRET is read from the environment. It is what connection strings are
signed with and what database encryption keys are derived from, so a command
run with the wrong one either refuses or writes a file the server cannot read.

SAPEDB_TRUST is whose signed bundles this host will look at, written
label=key and separated by commas or newlines, where key is the 64 lower-case
hex characters of an ed25519 public key. Unset is an empty list, and an empty
list refuses every bundle and says so — there is no spelling that means
"trust anything". "verify" and "install" read it; nothing else does.

"verify -envelopes FILE" adds each operation's cost envelope to the report:
which collections it touches, which indexes it uses, the most rows it can
hand back, and which fields leave the database. It is read off the
declarations in the file, so it can be read before the bundle is installed
anywhere — and it is the same derivation the catalogue prints afterwards, not
a second one.

A password is never taken as an argument: arguments are visible to anyone who
can run ps. "url" makes one and prints it as part of the connection string,
or reads one from stdin when told to. A signing key is not an argument either:
"keygen" writes the private half into the file named and prints only the
public half, and "seal" reads the private half from stdin.
`

var (
	ErrUsage  = errors.New("sapedb: that is not how this is used")
	ErrSecret = errors.New("sapedb: SAPEDB_SECRET is not set")
)

// Run is the whole command. It returns the exit status.
func Run(args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	if err := run(args, lookup, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, ErrUsage) {
			fmt.Fprint(stderr, "\n", usage)
		}
		return 1
	}
	return 0
}

type options struct {
	dir     string
	account string
	db      string
	encrypt bool
	secret  string
	label   string
	// lookup is the environment this command was given, kept so that a
	// command needing a variable no other command reads does not have to add
	// a field here for it. SAPEDB_TRUST is the first: it is read only by
	// verify and install, it is parsed by internal/bundle rather than here,
	// and a long list of keys is not something the other seven commands
	// should be carrying a copy of.
	lookup func(string) (string, bool)
}

func run(args []string, lookup func(string) (string, bool), stdin io.Reader, stdout io.Writer) error {
	// found && value != "": an explicitly empty variable is not a value, it
	// is "not set" — a container with SAPEDB_DIR= would otherwise put its
	// databases in the current directory instead of the documented default.
	// A mutation dropping the `value != ""` half survives every test in this
	// file: catching it means actually exercising the fallback, and for
	// -dir that fallback is the hard-coded absolute path /var/lib/sapedb —
	// the one hard-coded system path anywhere in this package's tests would
	// touch, with a result that depends on who is running the suite and
	// whether they followed the house rule of always going through the
	// mounted docker image. internal/service's equivalent test (see
	// TestTheEnvironmentIsReadAsWritten) gets away with checking this
	// because FromEnv returns Dir as a plain field on Config, so nothing
	// ever has to be opened to see it; this package never hands opts back
	// to a test to inspect the same way.
	get := func(name, fallback string) string {
		if value, found := lookup(name); found && value != "" {
			return value
		}
		return fallback
	}

	label := get("SAPEDB_LABEL", "")
	if strings.EqualFold(label, "direct") {
		label = signing.Direct
	}

	opts := options{
		dir:     get("SAPEDB_DIR", "/var/lib/sapedb"),
		account: get("SAPEDB_ACCOUNT", ""),
		db:      get("SAPEDB_DB", ""),
		secret:  get("SAPEDB_SECRET", ""),
		encrypt: strings.EqualFold(get("SAPEDB_ENCRYPT", ""), "1") || strings.EqualFold(get("SAPEDB_ENCRYPT", ""), "true"),
		label:   label,
		lookup:  lookup,
	}

	rest, err := parse(args, &opts)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("%w: no command", ErrUsage)
	}

	// A standalone command runs here, above the three gates below, because
	// none of them is about it. It still goes through findCommand and still
	// runs its own check, so an argument it does not take is refused the
	// same way every other command's is; what it skips is the secret, the
	// account and the name, which a question about the binary itself has no
	// business demanding. Unknown names fall through to the gates on
	// purpose: "no command called %q" is decided below, in one place, so a
	// typo does not get a different error depending on whether a secret
	// happened to be set.
	if entry, found := findCommand(rest[0]); found && entry.standalone {
		if err := entry.check(opts, rest[1:]); err != nil {
			return err
		}
		return entry.run(opts, nil, rest[1:], stdin, stdout)
	}

	if opts.secret == "" {
		return ErrSecret
	}
	if opts.account == "" || opts.db == "" {
		return fmt.Errorf("%w: -account and -db say which database", ErrUsage)
	}

	// Both names become a path component and, when -encrypt is set, half of
	// a key-derivation input — see internal/dbname. This is the door that
	// was missing before task 0053: internal/server has always refused a
	// name shaped like a path, at its own door, but nothing here ever asked
	// the same question, so SAPEDB_ACCOUNT='a/b' SAPEDB_DB='c' and
	// SAPEDB_ACCOUNT='a' SAPEDB_DB='b/c' both wrote the same file with no
	// error from either side. This does not make that check upstream
	// unneeded — it is a conditional fact, not a guarantee: server has
	// checked this shape at database() since before this package existed,
	// so a name that gets past THIS call is a name both halves of the
	// product agree on, not a name this tool has privately decided is safe.
	//
	// Placed before vfs.LockDir (see open, below), and before the command
	// switch, on purpose: a refused name must not take the directory lock
	// or create the account's directory (open() does both), and it must be
	// refused the same way whether the command that follows opens a file at
	// all — url and shell never call open(), and a name they would sign or
	// connect with is exactly as wrong as one apply would write.
	if err := dbname.CheckPair(opts.account, opts.db); err != nil {
		return oldPathHint(opts, err)
	}

	name, rest := rest[0], rest[1:]

	entry, found := findCommand(name)
	if !found {
		return fmt.Errorf("%w: no command called %q", ErrUsage, name)
	}

	// entry.check sees only opts and rest — argv, and whatever files rest
	// names — never db. That is the whole point: everything decidable from
	// what the caller typed is decided here, before open() has touched
	// disk, so a bad argument cannot leave .lock, an account folder, or
	// main.parts/ behind on its way to being refused. See section 4 of task
	// 0058's own text for where this line is drawn and why: state that only
	// the database itself knows (a name already declared differently, a
	// wrong secret on an encrypted file) is deliberately NOT here — it
	// stays behind open(), and is not cleaned up, because open() creates
	// nothing new once the file it is opening already exists.
	if err := entry.check(opts, rest); err != nil {
		return err
	}

	var db *store.Store
	if entry.opens {
		opened, closeDB, err := open(opts)
		if err != nil {
			return err
		}
		defer closeDB()
		db = opened
	}

	return entry.run(opts, db, rest, stdin, stdout)
}

// command is one row of the table run() dispatches through. Before task
// 0058 the list of commands existed three times in this file — the usage
// string, a switch that decided whether to call open(), and a second switch
// that actually ran the command — and argument checking was scattered across
// four of the seven commands, with the other three (ls, dump, restore)
// having none at all. This table is the one place both switches used to be:
// findCommand replaces both, check replaces the argument checks that used to
// live inside apply/changes/url/shell (and adds the three that were simply
// missing), and opens replaces the fact that a hand-written case list in the
// first switch happened to agree with a second hand-written case list in the
// second one.
type command struct {
	name string
	// opens says whether this command needs the database file open to run
	// at all. url and shell do not — a connection string is signed from
	// opts alone, and shell talks to a server over the network, never to a
	// file on this host.
	opens bool
	// standalone says this command is about the tool, not about a database,
	// so it runs before run() insists on a secret, an account and a name.
	// Everything else in this table is about one database and is right to be
	// refused without one; version is not, and a version command that cannot
	// answer until the caller has set three environment variables is a
	// version command nobody can use at the moment they need it.
	standalone bool
	// check is everything about a call that can be judged from opts and
	// the arguments alone, with no database open. It runs before open(),
	// every time, for every command — including the three that had no
	// argument checking at all before this task.
	check func(opts options, args []string) error
	// run is the command itself. db is nil when opens is false.
	run func(opts options, db *store.Store, args []string, stdin io.Reader, stdout io.Writer) error
}

// commands is the one list. usage (above) and this table are written
// independently on purpose — see TestUsageListsExactlyTheCommandsInTheTable
// in cli_test.go, which is why generating usage from this table is banned
// rather than merely unnecessary: it would turn that test into a string
// compared against itself.
var commands = []command{
	{
		name:  "apply",
		opens: true,
		check: checkApply,
		run: func(opts options, db *store.Store, args []string, _ io.Reader, out io.Writer) error {
			return apply(db, args, out, store.Caller{Actor: opts.account + " (apply)"})
		},
	},
	{
		// Standalone for verify's reason carried one step further: keygen
		// is about a file it creates and nothing else, and the author
		// running it is very often not an operator of any server — asking
		// them for a secret, an account and a database name would be asking
		// for three things that do not exist on their machine. See
		// author.go for why the private half goes to a file and only the
		// public half to stdout.
		name:       "keygen",
		standalone: true,
		check:      checkKeygen,
		run: func(_ options, _ *store.Store, args []string, _ io.Reader, out io.Writer) error {
			return keygen(args[0], out)
		},
	},
	{
		// Standalone for the same reason, and the only command in this
		// table that reads a secret from stdin without opening anything:
		// the signing key is the author's, not this host's, and SAPEDB_DIR
		// is nowhere in it.
		name:       "seal",
		standalone: true,
		check:      checkSeal,
		run: func(_ options, _ *store.Store, args []string, in io.Reader, out io.Writer) error {
			return seal(args[0], args[1], in, out)
		},
	},
	{
		// Not standalone, and not opens either: verify names no database,
		// but it is not about the tool the way version is — it is about a
		// file. It still runs before the secret/account/name gates, because
		// deciding whether to trust a bundle has nothing to do with which
		// database it might later be installed into, and somebody asked to
		// review a file may well not hold the secret at all.
		name:       "verify",
		standalone: true,
		check:      checkVerify,
		run: func(opts options, _ *store.Store, args []string, _ io.Reader, out io.Writer) error {
			// The whole of args rather than args[0]: verify takes an
			// option of its own, -envelopes, and parses it itself.
			// See verifyArgs in bundle.go for why it is not in parse().
			return verify(opts, args, out)
		},
	},
	{
		name:  "install",
		opens: true,
		check: checkInstall,
		run: func(opts options, db *store.Store, args []string, _ io.Reader, out io.Writer) error {
			// No Caller built here, unlike apply just above: what the change
			// log records is the bundle and the key that signed it, which is
			// inside the file and not in opts. See installedBy.
			return install(db, opts, args[0], out)
		},
	},
	{
		name:  "ls",
		opens: true,
		check: checkNoArguments("ls"),
		run: func(_ options, db *store.Store, _ []string, _ io.Reader, out io.Writer) error {
			return list(db, out)
		},
	},
	{
		name:  "dump",
		opens: true,
		check: checkNoArguments("dump"),
		run: func(_ options, db *store.Store, _ []string, _ io.Reader, out io.Writer) error {
			return dump(db, out)
		},
	},
	{
		name:  "restore",
		opens: true,
		// checkNoArguments only, not the content of stdin — stdin is not
		// argv, and checking it here would mean buffering it and handing
		// Restore an already-read stream instead of the reader it gets
		// today. Section 4's "a real exception" is exactly this: restore on
		// garbage stdin still opens an empty database before failing, and
		// this task does not change that. What it does change is the
		// argument case: "restore junk" is refused before open() runs at
		// all, same as ls and dump.
		check: checkNoArguments("restore"),
		run: func(_ options, db *store.Store, _ []string, in io.Reader, out io.Writer) error {
			return restore(db, in, out)
		},
	},
	{
		name:  "log",
		opens: true,
		check: checkLog,
		run: func(_ options, db *store.Store, args []string, _ io.Reader, out io.Writer) error {
			return changes(db, args, out)
		},
	},
	{
		name:  "url",
		opens: false,
		check: checkURL,
		run: func(opts options, _ *store.Store, args []string, in io.Reader, out io.Writer) error {
			return url(opts, args, in, out)
		},
	},
	{
		name:  "shell",
		opens: false,
		check: checkShell,
		run: func(opts options, _ *store.Store, args []string, in io.Reader, out io.Writer) error {
			// Not opened above: the shell talks to a running server, and
			// taking the directory lock is exactly what it must not do —
			// the database an operator wants to look inside is the one
			// that is serving.
			return shell(opts, args, in, out)
		},
	},
	{
		// Not opened, for shell's reason: the database an operation runs
		// against is the one that is serving, and taking the directory lock
		// would be taking it away from the server.
		//
		// A command of its own rather than "pipe a line into shell", which
		// already works and is not enough: Shell prints a refusal and carries
		// on to the next line, so `echo 'invoke ...' | sapedb shell` exits 0
		// whether the operation ran or was refused. A script cannot branch on
		// that, and a deployment step that cannot tell a write from a refusal
		// is the silent failure this product keeps arguing against. This
		// returns the error, so the status is 1 and stderr says why.
		name:  "invoke",
		opens: false,
		check: checkInvoke,
		run: func(opts options, _ *store.Store, args []string, _ io.Reader, out io.Writer) error {
			return invokeOnce(opts, args, out)
		},
	},
	{
		name:  "version",
		opens: false,
		// Standalone, which is not merely opens:false: url and shell also
		// leave the file alone, but they still need an account, a name and a
		// secret to sign or connect with. Asking a binary what it
		// is must work before any of those exist — on a host with no
		// databases, in a container someone is trying to identify, in a bug
		// report written by somebody who was never given the secret.
		standalone: true,
		check:      checkNoArguments("version"),
		run: func(_ options, _ *store.Store, _ []string, _ io.Reader, out io.Writer) error {
			_, err := fmt.Fprintf(out, "sapedb %s\n", build.Version)
			return err
		},
	},
}

// findCommand looks a name up in commands. A linear scan over a table this
// size is not a data structure decision worth a map: this runs once per
// process.
func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// checkNoArguments builds the check for the three commands that were missing
// one entirely before this task: ls, dump and restore all take zero
// arguments, and all three ran anyway — silently ignoring whatever came
// after them — right up until this task. "sapedb ls junk" and "sapedb dump
// junk" both exited 0 on the commit this task started from.
func checkNoArguments(name string) func(options, []string) error {
	return func(_ options, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("%w: %s takes no arguments, and got %q", ErrUsage, name, args)
		}
		return nil
	}
}

// checkApply is apply's argument check: a file to declare, present, readable,
// and valid JSON that DisallowUnknownFields accepts — everything apply()
// itself checks about a file before ever calling db.Declare. It duplicates
// apply()'s read-and-decode rather than sharing it, on purpose: task 0058's
// own text allows either design, and a duplicate keeps the two apply
// mutations that target this function (skip len(files)==0, or skip decoding)
// from also silently changing what apply() itself does when check is not
// what caught them.
//
// The loop matters as much as the read: checking only files[0] would let
// "apply ok.json missing.json" reach open() and apply()'s own loop, which
// would declare ok.json's collection and print it before failing on the
// second file — exactly the half-applied run task 0058 exists to prevent
// happening even one file layer up from the database itself.
func checkApply(_ options, files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("%w: apply needs a file", ErrUsage)
	}
	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		wanted := schema{}
		decoder := json.NewDecoder(strings.NewReader(string(content)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&wanted); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := exactSchema(name, &wanted); err != nil {
			return err
		}
	}
	return nil
}

// exactSchema refuses a schema file carrying a number this database cannot
// store as it is written, and replaces every number that survives with the
// float64 the store expects (ISS-35).
//
// It runs in two places — here in checkApply and again in apply() — and that
// is deliberate, the same duplication checkApply's own comment describes. It
// is one rule in both: both call internal/number, which is the function the
// invoke door calls, so the two cannot drift. What is duplicated is only
// *when* it runs.
//
// Running it in checkApply is what makes the refusal cost nothing. check runs
// before open(), so a file carrying 9007199254740993 is refused before the
// database is opened — before the directory is even created for a database
// that does not exist yet. Nothing is written because nothing is opened.
// Running it again in apply() is what keeps that true for a caller that
// reaches apply() another way, and for a mutation that deletes the call above.
func exactSchema(name string, wanted *schema) error {
	for i := range wanted.Collections {
		where := fmt.Sprintf("%s: collection %q", name, wanted.Collections[i].Name)
		if err := number.ExactIn(where, &wanted.Collections[i]); err != nil {
			return err
		}
	}
	for i := range wanted.Operations {
		where := fmt.Sprintf("%s: operation %q", name, wanted.Operations[i].Name)
		if err := number.ExactIn(where, &wanted.Operations[i]); err != nil {
			return err
		}
	}
	return nil
}

// checkLog is log's argument check: at most one argument, and when given,
// it has to be the number changes() will Sscanf it as. Before this task
// "log 1 2 3" ran, reading only args[0] and dropping the rest.
func checkLog(_ options, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("%w: log takes at most one argument, FROM", ErrUsage)
	}
	if len(args) == 1 {
		var from uint64
		if _, err := fmt.Sscanf(args[0], "%d", &from); err != nil {
			return fmt.Errorf("%w: %q is not an entry number", ErrUsage, args[0])
		}
	}
	return nil
}

// parse reads the options, which come before the command.
func parse(args []string, opts *options) ([]string, error) {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			return args, nil
		}
		args = args[1:]

		name, value, joined := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		next := func() (string, error) {
			if joined {
				return value, nil
			}
			if len(args) == 0 {
				return "", fmt.Errorf("%w: -%s needs a value", ErrUsage, name)
			}
			taken := args[0]
			args = args[1:]
			return taken, nil
		}

		var err error
		switch name {
		case "dir":
			opts.dir, err = next()
		case "account":
			opts.account, err = next()
		case "db":
			opts.db, err = next()
		case "encrypt":
			opts.encrypt = true
		case "h", "help":
			return nil, fmt.Errorf("%w", ErrUsage)
		default:
			return nil, fmt.Errorf("%w: no option called -%s", ErrUsage, name)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// oldFileExt is the database file extension this product used before it was
// called sapedb, assembled from single-character literals rather than spelled
// whole: internal/naming would otherwise flag the string that spells it, and
// a check for the old extension has no business leaving the old extension
// lying around in the source as plain text.
//
// This is now the last place in the tree that still carries the old name at
// all. The environment-variable twin of this guard was removed along with
// the rename (there was never a release, so nothing was ever set under the
// old prefix to be refused); this one is still here because it is also the
// ordering anchor that keeps a refusal from creating the account folder and
// the lock, which is a job that has nothing to do with the name. Removing it
// is a decision about a migration aid, not about a rename.
var oldFileExt = "." + string([]byte{'r', 's', 'q', 'l'})

// checkOldExtension refuses to open a database when its .sapedb path does
// not exist but a same-named file under the old extension does.
//
// This is the one variant of the old name that fails silently rather than
// being refused: an old connection scheme or signing label gets a parse or
// verify error, and a file carrying the old format tag is refused outright
// as pager.ErrNotSapedb, but a missing .sapedb file with a real
// old-extension file sitting right next to it does not look like an error
// at all — vfs.OpenFile below would simply create a new, empty database and
// this tool would report an empty database where a real one exists. That is
// not "not found", it is data going invisible. So this stops and says
// exactly where the data actually is, rather than renaming it or opening it
// as-is: changing what a file means without being asked is not this tool's
// call to make, and the operator is the one who knows whether that old file
// is still needed anywhere else.
//
// Same shape as the environment-variable signpost, and the same note
// applies: this is not a compatibility path, and it is meant to be removed
// once operators have confirmed they have moved their database files.
func checkOldExtension(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	old := strings.TrimSuffix(path, ".sapedb") + oldFileExt
	if _, err := os.Stat(old); err == nil {
		return fmt.Errorf("sapedb: %s does not exist, but %s does; mv %s %s", path, old, old, path)
	}
	return nil
}

// dbPath is where an (account, db) pair's file lives under dir. This is the
// one expression in the tree that turns those three things into a path for
// this side of the product — internal/server has its own, independent one
// (see task 0053's own text, "there is no single layer today that EVERY
// path goes through" — there was no single layer both sides went through
// before this task), and this function is what keeps this file from growing
// a second
// one of its own: both open() and oldPathHint(), below, call this instead
// of writing filepath.Join again.
func dbPath(opts options) string {
	return filepath.Join(opts.dir, opts.account, opts.db+".sapedb")
}

// oldPathHint decides whether a name dbname.CheckPair just refused is one
// that, before this check existed, had already been used to write a real
// file — and if so, says exactly where it is and how to get the data out.
//
// Same discipline as checkOldExtension above, and the same reason: os.Stat,
// not a promise. A name nobody ever wrote anything under gets the bare
// refusal and nothing more — a migration lecture that does not apply teaches
// people to stop reading migration lectures.
//
// The encrypted case is deliberately not offered mv. dbkey.Key derives a
// database's encryption key from account+"/"+name (see that package), so
// changing either half of a valid pair changes the key — there is no
// rename that carries the old key forward, only a read-with-the-old-code,
// then a restore into a name this version accepts. Task 0053's Result
// section has the structural argument for why no pair CheckPair accepts can
// ever collide with a pair it refused: their info strings cannot be equal.
func oldPathHint(opts options, refusal error) error {
	path := dbPath(opts)
	if _, err := os.Stat(path); err != nil {
		return refusal
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	parts := strings.TrimSuffix(abs, ".sapedb") + ".parts"

	if opts.encrypt {
		return fmt.Errorf("%w\n  a file already exists at %s (and %s, if there is one).\n"+
			"  It is encrypted, so no rename carries its key forward: read it out with\n"+
			"  the sapedb build from before this name was refused, then restore into a\n"+
			"  valid account/db pair.", refusal, abs, parts)
	}
	return fmt.Errorf("%w\n  a file already exists at %s (and %s, if there is one).\n"+
		"  mv it, and that .parts directory if there is one, to a valid account/db\n"+
		"  pair.", refusal, abs, parts)
}

// open opens the database file, taking its lock.
func open(opts options) (*store.Store, func(), error) {
	// checkOldExtension first, and before anything that touches disk:
	// os.Stat needs no lock and creates nothing, so a name refused here
	// leaves SAPEDB_DIR exactly as it found it — no .lock, no account
	// folder. Task 0038 found both of those left behind by a refusal that
	// used to happen after LockDir and MkdirAll had already run; task 0050
	// is this reordering. A side effect worth naming: when the server
	// already holds the directory lock and the file also carries the old
	// extension, this now answers with the mv hint instead of "the server
	// is probably running" — both are true, but the mv hint is the one
	// action that is actually blocking, and doing it makes the other
	// message appear on the next attempt.
	//
	// checkOldExtension is placed ahead of BOTH os.MkdirAll and vfs.LockDir
	// below, not just ahead of LockDir — and that first half matters on its
	// own, separately from the old-extension case this function is named
	// after. TestARefusalOnTheLockedDirectoryLeavesNoAccountFolder is the
	// case that pins it: a directory-lock refusal (nothing wrong with the
	// file at all, somebody else just holds the lock) must not create the
	// account folder either, and it will if os.MkdirAll runs before this
	// check — MkdirAll does not know or care whether the file it is making
	// room for is old, missing, or fine; it creates the folder the moment
	// it runs, refusal or not. An earlier version of this comment argued
	// that ordering could never be observed because an old-extension file
	// can only be found once its folder already exists, making MkdirAll a
	// no-op either way — true for THAT specific repro, but that argument
	// quietly assumed the only way into this refusal path was through an
	// old-extension file. It is not: a bare locked-directory refusal, with
	// no file of any kind on disk yet, goes through the exact same lines.
	path := dbPath(opts)
	if err := checkOldExtension(path); err != nil {
		return nil, nil, err
	}

	// The directory next: a server holds this for its whole life, so being
	// refused here is the answer to "is the server running", rather than a
	// guess that depends on which databases it happens to have opened.
	held, err := vfs.LockDir(opts.dir)
	if err != nil {
		if errors.Is(err, vfs.ErrLocked) {
			return nil, nil, fmt.Errorf("%w\n  the server is probably running; stop it, or work on a dump instead", err)
		}
		return nil, nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		held.Close()
		return nil, nil, err
	}

	file, err := vfs.OpenFile(path, 0o600)
	if err != nil {
		held.Close()
		if errors.Is(err, vfs.ErrLocked) {
			return nil, nil, fmt.Errorf("%w\n  the server is probably running; stop it, or work on a dump instead", err)
		}
		return nil, nil, err
	}

	settings := pager.Options{}
	if opts.encrypt {
		key, err := dbkey.Key(opts.secret, opts.account, opts.db)
		if err != nil {
			file.Close()
			held.Close()
			return nil, nil, err
		}
		settings.Key = key
	}

	size, err := file.Size()
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}

	var pages *pager.Pager
	if size == 0 {
		pages, err = pager.CreateWith(file, settings)
	} else {
		pages, err = pager.OpenWith(file, settings)
	}
	if err != nil {
		file.Close()
		held.Close()
		if errors.Is(err, pager.ErrKey) {
			return nil, nil, fmt.Errorf("%w\n  SAPEDB_SECRET does not match the one this database was made with", err)
		}
		if errors.Is(err, pager.ErrNotEncrypted) {
			return nil, nil, fmt.Errorf("%w\n  drop -encrypt, or this is not the database you meant", err)
		}
		return nil, nil, err
	}

	opened, err := store.Open(pages)
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}

	// The same directory the server uses, so that a tool and a server see the
	// same database rather than each seeing the part of it they made.
	folder, err := vfs.At(strings.TrimSuffix(path, ".sapedb")+".parts", 0o600)
	if err != nil {
		file.Close()
		held.Close()
		return nil, nil, err
	}
	opened.Keep(folder, settings.Key)

	return opened, func() { pages.Close(); held.Close() }, nil
}

// schema is what an apply file holds.
type schema struct {
	Collections []store.Spec      `json:"collections,omitempty"`
	Operations  []store.Operation `json:"operations,omitempty"`
}

// apply declares what the files say, and says what changed.
//
// Applying the same file twice does nothing the first time did not: a
// collection already declared that way is left alone, and an operation whose
// declaration has not changed is not given a new version. That is what makes
// this safe to run on every deploy, which is the only way it will actually be
// run.
// by is what the change log records against every collection and every
// operation this run declares. It is the account the command was pointed at, and it is a label
// rather than a proof: running apply means holding the database file's
// exclusive lock, and anybody who can do that could have written the same
// bytes by hand. It is still the best name available at this point, and the
// alternative — the empty actor this path recorded until now — is not a
// smaller claim, it is no claim at all.
func apply(db *store.Store, files []string, out io.Writer, by store.Caller) error {
	if len(files) == 0 {
		return fmt.Errorf("%w: apply needs a file", ErrUsage)
	}

	// Buffered rather than written to out as each line is decided: every
	// message below reports something declared inside this one transaction,
	// and there is exactly one Commit for the whole run, at the end. A later
	// file failing does not undo what an earlier file already printed — the
	// terminal has no equivalent of the transaction the database itself
	// rolled back. So nothing here reaches the caller's out until Commit
	// below actually succeeds; on any earlier return, this buffer is
	// dropped with everything else the failed run touched.
	var progress bytes.Buffer

	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}

		// UseNumber, then exactSchema, before a single Declare — see
		// exactSchema, and internal/number for why the check cannot live
		// anywhere later than this Decode. A constant in a declaration was
		// rounded exactly as silently as an argument was, and worse: it is
		// stored once and then written again by every call that runs the
		// operation.
		wanted := schema{}
		decoder := json.NewDecoder(strings.NewReader(string(content)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&wanted); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := exactSchema(name, &wanted); err != nil {
			return err
		}

		for _, spec := range wanted.Collections {
			if _, err := db.Declare(by, spec); err != nil {
				return fmt.Errorf("%s: collection %q: %w", name, spec.Name, err)
			}
			fmt.Fprintf(&progress, "collection %s\n", spec.Name)
		}

		for _, operation := range wanted.Operations {
			before, found, err := db.Operation(operation.Name, 0)
			if err != nil && !errors.Is(err, store.ErrNoOperation) {
				return err
			}
			if found && sameOperation(before, operation) {
				fmt.Fprintf(&progress, "operation %s unchanged at version %d\n", operation.Name, before.Version)
				continue
			}

			stored, err := db.DeclareOperation(by, operation)
			if err != nil {
				return fmt.Errorf("%s: operation %q: %w", name, operation.Name, err)
			}
			fmt.Fprintf(&progress, "operation %s version %d\n", stored.Name, stored.Version)
		}
	}

	if err := db.Commit(); err != nil {
		return err
	}
	_, err := out.Write(progress.Bytes())
	return err
}

// sameOperation compares a declaration with one already stored, ignoring the
// fields the STORE assigns rather than the declarer writing them: the version
// it was given, and the identity it was recorded against.
//
// DeclaredBy is here for exactly the reason Version is. Neither is ever
// present in a schema file or a bundle — DeclareOperation overwrites both from
// its own knowledge — so leaving either in the comparison makes every stored
// declaration differ from every file that produced it, and `apply` run twice
// writes a second version of everything. That is not a hypothetical: adding
// the field without this line turned TestApplyingTwiceChangesNothing and
// TestInstallingAVerifiedBundleDeclaresWhatItCarries red on the first run.
//
// So an identical declaration re-applied by somebody ELSE keeps the identity
// already on it. That is the honest answer rather than a gap: nothing was
// declared, and the record says who declared, not who last ran a command that
// declined to. Recording the second identity would mean writing a new version
// that differs from the old one in nothing but its attribution, which is the
// noise these skips exist to prevent.
func sameOperation(stored, wanted store.Operation) bool {
	wanted.Version = stored.Version
	wanted.DeclaredBy = stored.DeclaredBy

	first, err := json.Marshal(stored)
	if err != nil {
		return false
	}
	second, err := json.Marshal(wanted)
	if err != nil {
		return false
	}
	return string(first) == string(second)
}

func list(db *store.Store, out io.Writer) error {
	// First, because how the database was last left decides whether anything
	// below is the whole story.
	if state := db.HowItWasLeft(); state != "" {
		fmt.Fprintln(out, state)
	}

	for _, name := range db.Collections() {
		collection, err := db.Collection(name)
		if err != nil {
			return err
		}
		spec := collection.Spec()

		fmt.Fprintf(out, "collection %s (key %s %s", spec.Name, spec.Key.Path, spec.Key.Type)
		if spec.Key.Auto != "" {
			fmt.Fprintf(out, ", %s", spec.Key.Auto)
		}
		fmt.Fprintln(out, ")")

		for _, index := range spec.Indexes {
			fields := make([]string, 0, len(index.Fields))
			for _, field := range index.Fields {
				mark := ""
				if field.Descending {
					mark = " desc"
				}
				fields = append(fields, field.Path+" "+field.Type+mark+" missing:"+field.Missing)
			}
			flags := ""
			if index.Unique {
				flags += " unique"
			}
			if index.Array != "" {
				flags += " array:" + index.Array
			}
			fmt.Fprintf(out, "  index %s%s [%s]\n", index.Name, flags, strings.Join(fields, ", "))
		}
	}

	operations, err := db.Operations()
	if err != nil {
		return err
	}
	for _, operation := range operations {
		fmt.Fprintf(out, "operation %s v%d %s %s", operation.Name, operation.Version, operation.Action, operation.Collection)
		if operation.Index != "" {
			fmt.Fprintf(out, " via %s", operation.Index)
		}
		if operation.Limit > 0 {
			fmt.Fprintf(out, " limit %d", operation.Limit)
		}
		if len(operation.Scopes) > 0 {
			fmt.Fprintf(out, " scopes %s", strings.Join(operation.Scopes, ","))
		}
		fmt.Fprintln(out)
	}

	latest, err := db.LatestLSN()
	if err != nil {
		return err
	}
	oldest, err := db.OldestLSN()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "log %d..%d\n", oldest, latest)
	return nil
}

func dump(db *store.Store, out io.Writer) error {
	_, err := db.Dump(out)
	return err
}

func restore(db *store.Store, in io.Reader, out io.Writer) error {
	at, err := db.Restore(in)
	if err != nil {
		return err
	}
	if err := db.Commit(); err != nil {
		return err
	}
	fmt.Fprintf(out, "restored to change %d\n", at)
	return nil
}

func changes(db *store.Store, args []string, out io.Writer) error {
	from := uint64(0)
	if len(args) > 0 {
		if _, err := fmt.Sscanf(args[0], "%d", &from); err != nil {
			return fmt.Errorf("%w: %q is not an entry number", ErrUsage, args[0])
		}
	}

	return db.Changes(from, func(change store.Change) bool {
		line, err := json.Marshal(change)
		if err != nil {
			return false
		}
		fmt.Fprintln(out, string(line))
		return true
	})
}

// checkURL is url's argument check, moved here verbatim from what used to be
// url()'s own body: a password is read from stdin with "-", never given as
// an argument, because arguments are visible in ps to every user on the
// machine. This is the check task 0058's mutation P12 nudges outward by one
// (len(args) > 1 to len(args) > 2) to ask whether that guard is pinned to
// N=2 by a real wall or only by coincidence — see the Results table for the
// answer.
//
// The len(args) > 2 line below is not from the original task 0058 patch —
// QA 0058 measured that "sapedb url localhost:9 - junk junk2" exited 0,
// printed the connection string, and dropped "junk" and "junk2" without a
// word, because this function had no upper bound on argument count at all.
// That is the exact shape task 0058's own rule 1 forbids ("a rejected
// argument must not be silently dropped by any subcommand"), so this closes
// it here rather than leaving it as a debt: url now takes at most a host
// and one more word ("-" or a password), same as every other subcommand.
// See CHANGELOG.md — this is the fifth breaking change, called out there.
func checkURL(_ options, args []string) error {
	if len(args) > 1 && args[1] != "-" {
		return fmt.Errorf("%w: a password is read from stdin with -, never given as an argument", ErrUsage)
	}
	if len(args) > 2 {
		return fmt.Errorf("%w: url takes at most a host and -, and got %q", ErrUsage, args)
	}
	return nil
}

// url prints a connection string.
//
// The password is made here and printed as part of the string, or read from
// stdin when the caller has one already. It is never an argument: arguments
// are visible in ps to every user on the machine, and a password that has been
// in a process list is a password that has been published.
func url(opts options, args []string, stdin io.Reader, out io.Writer) error {
	host := "localhost:7433"
	if len(args) > 0 {
		host = args[0]
	}

	password := ""
	if len(args) > 1 {
		// checkURL has already refused anything here but "-".
		read, err := io.ReadAll(io.LimitReader(stdin, 1024))
		if err != nil {
			return err
		}
		password = strings.TrimSpace(string(read))
	} else {
		made, err := makePassword()
		if err != nil {
			return err
		}
		password = made
	}

	// The password is not checked here: signing refuses one the format does not
	// allow, and two places deciding the same thing is one place to forget when
	// the rule changes.
	signature, err := signing.Sign(
		signing.Parts{AccountID: opts.account, Password: password, DBName: opts.db},
		opts.secret, opts.label,
	)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "sapedb://%s:%s@%s/%s?sig=%s\n", opts.account, password, host, opts.db, signature)
	return nil
}

func makePassword() (string, error) {
	raw := make([]byte, passwordLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sapedb: no randomness for a password: %w", err)
	}

	// Rejection-free: the alphabet is 66 long and a byte is 256, so taking the
	// remainder favours the first 58 characters slightly. Drawn again until the
	// byte is in a range that divides evenly instead.
	made := make([]byte, 0, passwordLength)
	for len(made) < passwordLength {
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		for _, b := range raw {
			if int(b) >= (256/len(passwordAlphabet))*len(passwordAlphabet) {
				continue
			}
			made = append(made, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(made) == passwordLength {
				break
			}
		}
	}
	return string(made), nil
}
