package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// ISS-35's second half, measured at the commands rather than at the rule.
//
// The first half closed the invoke frame. `sapedb apply` never passes it: it
// opens the database file directly and declares out of a schema file, and a
// constant written into one of those declarations — `"n": {"value":
// 9007199254740993}` — was stored as 9007199254740992, exit 0, nothing in the
// log. Worse than a rounded argument, because a declaration is stored once and
// then written again by every call that runs it.
//
// Two things are measured here and neither is the error message on its own:
// that the run is refused, and that the refusal leaves NOTHING behind. A
// refusal that has already created the database file is a refusal somebody has
// to clean up.

// schemaWith is one insert whose document carries the given constant, written
// as the literal text so the digits reach the decoder as typed. Building this
// by marshalling a Go value would round it in the test before the command ever
// saw it, which is the bug, and would leave the test green against a command
// that does nothing.
func schemaWith(constant string) string {
	return fmt.Sprintf(`{
  "collections": [
    {"name": "ids", "key": {"path": "id", "type": "string", "auto": "ulid"}, "indexes": []}
  ],
  "operations": [
    {"name": "ids.fixed", "collection": "ids", "action": "insert",
     "document": {"n": {"value": %s, "constant": true}}}
  ]
}`, constant)
}

// leftBehind is every file in the directory that is not one this test wrote.
func leftBehind(t *testing.T, dir string) []string {
	t.Helper()
	found := []string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, ".json") {
			return nil
		}
		found = append(found, strings.TrimPrefix(path, dir))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestApplyRefusesAConstantItCannotStoreAndWritesNothing is the measurement in
// ISS-35's second half, run as the command.
func TestApplyRefusesAConstantItCannotStoreAndWritesNothing(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", schemaWith("9007199254740993"))

	out, errs, status := setup.run("apply", file)

	if status == 0 {
		t.Fatalf("apply stored a number it cannot hold, and said so: %q", out)
	}
	// The value by name, and what would have been stored: the two halves that
	// let an author see the neighbouring number they would have found in a
	// dump.
	if !strings.Contains(errs, "9007199254740993") {
		t.Errorf("the refusal does not name the value sent: %q", errs)
	}
	if !strings.Contains(errs, "9007199254740992") {
		t.Errorf("the refusal does not say what would have been stored: %q", errs)
	}
	// Which constant of which declaration, in a file that may hold hundreds.
	if !strings.Contains(errs, `operation "ids.fixed".document.n.value`) {
		t.Errorf("the refusal does not say which constant it was: %q", errs)
	}
	if out != "" {
		t.Errorf("a refused apply printed progress: %q", out)
	}

	// And nothing on disk. apply's check runs before open(), so a refused run
	// does not create the database it was pointed at — there is no half-built
	// file for somebody to find, and no directory to clean up.
	if left := leftBehind(t, setup.dir); len(left) != 0 {
		t.Errorf("a refused apply left %v behind", left)
	}
}

// TestTheApplyBoundaryIsRepresentabilityNotMagnitude pins both sides at the
// command, because a rule can be right in internal/number and wired up wrong
// here — an `apply` that refused every large integer would pass a test holding
// only the refusals, and would break authors whose numbers were always fine.
//
// The two decimals are in the table for the same reason: 1.5e300 is
// integer-valued and 0.1 is not exactly a float64, and both have always gone
// in and come back as written.
func TestTheApplyBoundaryIsRepresentabilityNotMagnitude(t *testing.T) {
	for _, one := range []struct {
		sent    string
		refused bool
		why     string
	}{
		{"9007199254740992", false, "2^53, exactly a float64"},
		{"9007199254740993", true, "2^53+1, the nearest float64 is 2^53"},
		{"9007199254740994", false, "2^53+2 — larger than the refused one, and exact"},
		{"9223372036854775807", true, "2^63-1, not a float64"},
		{"9223372036854775808", false, "2^63 — vastly larger than the refused one, and exact"},
		{"-9007199254740993", true, "the same hole below zero"},
		{"1.5e300", false, "integer-valued, written as a float, printed back as written"},
		{"0.1", false, "a fraction float64 does not hold — and never has, for anybody"},
		{"42", false, "an ordinary integer"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			setup := start(t)
			file := setup.write("schema.json", schemaWith(one.sent))

			out, errs, status := setup.run("apply", file)

			if one.refused {
				if status == 0 {
					t.Fatalf("%s (%s) was declared rather than refused: %q", one.sent, one.why, out)
				}
				if !strings.Contains(errs, one.sent) {
					t.Errorf("the refusal does not name %s: %q", one.sent, errs)
				}
				if left := leftBehind(t, setup.dir); len(left) != 0 {
					t.Errorf("a refused apply left %v behind", left)
				}
				return
			}

			if status != 0 {
				t.Fatalf("%s (%s) was refused: %q", one.sent, one.why, errs)
			}
			if !strings.Contains(out, "operation ids.fixed version 1") {
				t.Fatalf("%s was accepted without being declared: %q", one.sent, out)
			}
		})
	}
}

// TestAnAcceptedConstantIsStoredAsTheNumberThatWasWritten reads it back out of
// the database rather than trusting exit 0.
//
// The point of the accepted half is that the declaration in the catalogue says
// what the file said. A check that let a value through and still rounded it
// would pass every test above.
func TestAnAcceptedConstantIsStoredAsTheNumberThatWasWritten(t *testing.T) {
	for _, one := range []struct {
		sent   string
		stored string
	}{
		{"9007199254740992", "9007199254740992"},
		{"9007199254740994", "9007199254740994"},
		{"42", "42"},
		{"0.1", "0.1"},

		// The one accepted value whose printed spelling differs. float64
		// holds 2^63 exactly — nothing is lost — but the shortest decimal
		// that identifies that float64 is 9223372036854776000, so that is
		// what a dump prints. The value survives; its spelling does not.
		// Stated here rather than papered over, because somebody comparing a
		// dump against their schema file will meet it.
		{"9223372036854775808", "9223372036854776000"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			setup := start(t)
			file := setup.write("schema.json", schemaWith(one.sent))

			if _, errs, status := setup.run("apply", file); status != 0 {
				t.Fatalf("apply: %s", errs)
			}
			out, errs, status := setup.run("dump")
			if status != 0 {
				t.Fatalf("dump: %s", errs)
			}
			wanted := `"value":` + one.stored
			if !strings.Contains(out, wanted) {
				t.Fatalf("declared %s, and the database holds something else — looked for %s in:\n%s",
					one.sent, wanted, out)
			}
		})
	}
}

// TestApplyRefusesBeforeItDeclaresAnythingAtAll is the partial-write half, on
// the shape that can actually half-apply: several files, the bad number in the
// last one.
//
// apply buffers its progress and commits once at the end, so a refusal part
// way through rolls the transaction back — but the check has to run before any
// of it, not per file as the loop reaches it, or the refusal arrives after the
// first file's collection has been built in an open transaction. Measured on
// the database, not on the message: nothing declared, and no file on disk.
func TestApplyRefusesBeforeItDeclaresAnythingAtAll(t *testing.T) {
	setup := start(t)
	good := setup.write("good.json", articles)
	bad := setup.write("bad.json", schemaWith("9007199254740993"))

	out, errs, status := setup.run("apply", good, bad)
	if status == 0 {
		t.Fatalf("a run holding a refused constant applied anyway: %q", out)
	}
	if !strings.Contains(errs, "9007199254740993") {
		t.Errorf("the refusal does not name the value: %q", errs)
	}
	if out != "" {
		t.Errorf("a refused run printed progress for the file it got through: %q", out)
	}
	if left := leftBehind(t, setup.dir); len(left) != 0 {
		t.Errorf("a refused run left %v behind", left)
	}
}

// TestTheShellSendsTheDigitsThatWereTypedForATypedAccess is the courier half
// for the operator shell, and the reason the server's refusal is reachable
// through the tool this project ships.
//
// `get ids 9007199254740993` used to unmarshal into a float64 before the
// access went out, which means the shell destroyed the digits itself: the
// daemon was handed 9007199254740992 and fetched the document under THAT key,
// and printed it. A row came back and it was somebody else's. A server-side
// refusal alone would have been unreachable through the shell.
//
// So this asserts what goes on the wire, by marshalling the access, rather
// than asserting a Go type. The wire is what the server reads.
func TestTheShellSendsTheDigitsThatWereTypedForATypedAccess(t *testing.T) {
	for _, one := range []struct {
		line string
		wire string
	}{
		{`get ids 9007199254740993`, `"key":9007199254740993`},
		{`get ids 9223372036854775807`, `"key":9223372036854775807`},
		{`get ids 9007199254740992`, `"key":9007199254740992`},
		{`get ids 1.5e300`, `"key":1.5e300`},
		{`get ids 0.1`, `"key":0.1`},
		{`scan ids from 9007199254740993 limit 5`, `"values":[9007199254740993]`},
		{`scan ids after 1234567890123456789 limit 5`, `"values":[1234567890123456789]`},
	} {
		t.Run(one.line, func(t *testing.T) {
			asked, err := access(strings.Fields(one.line))
			if err != nil {
				t.Fatalf("the shell refused the line: %v", err)
			}
			sent, err := json.Marshal(asked)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(sent), one.wire) {
				t.Fatalf("typed %q, sent %s, wanted it to carry %s", one.line, sent, one.wire)
			}
		})
	}
}

// TestTheShellStillReadsALimitAsAWholeNumberOfRows is the control for the line
// above.
//
// A limit is a row count this shell turns into an int before anything sees it,
// not a `number` the database stores, so ISS-35 has nothing to say about it —
// but literal() now hands back a json.Number rather than a float64, and the
// limit check used to be a float64 type assertion. Left alone, `limit 100`
// would have started meaning "not a whole number of rows".
func TestTheShellStillReadsALimitAsAWholeNumberOfRows(t *testing.T) {
	for _, one := range []struct {
		line    string
		limit   int
		refused bool
	}{
		{line: `scan ids limit 5`, limit: 5},
		{line: `scan ids limit 100`, limit: 100},
		{line: `scan ids limit 1e2`, limit: 100},
		{line: `scan ids limit 5.0`, limit: 5},
		{line: `scan ids limit 5.5`, refused: true},
		{line: `scan ids limit 0`, refused: true},
		{line: `scan ids limit -1`, refused: true},
		{line: `scan ids limit "5"`, refused: true},
		{line: `scan ids limit x`, refused: true},
	} {
		t.Run(one.line, func(t *testing.T) {
			asked, err := access(strings.Fields(one.line))
			if one.refused {
				if err == nil {
					t.Fatalf("%q was read as limit %d", one.line, asked.Limit)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was refused: %v", one.line, err)
			}
			if asked.Limit != one.limit {
				t.Fatalf("%q was read as limit %d, want %d", one.line, asked.Limit, one.limit)
			}
		})
	}
}

// TestTheShellStillRefusesTwoValuesWhereItWantsOne is the other control.
//
// literal() moved from json.Unmarshal to a json.Decoder to stop it rounding,
// and json.Decoder does not refuse trailing bytes on its own. Without the
// explicit check, `get ids 1 2` would quietly read as `get ids 1` — a
// different question answered without anybody noticing, which is the failure
// this shell's own doc says it exists to prevent.
func TestTheShellStillRefusesTwoValuesWhereItWantsOne(t *testing.T) {
	for _, word := range []string{`1 2`, `{"a":1} 2`, `"a" "b"`} {
		if _, err := literal(word); err == nil {
			t.Errorf("%q was read as a single value", word)
		}
	}
}

// TestAnIndexedNumberIsStillAnIndexedNumber is the control that the accepted
// half of all of this still reaches the parts of the store that care.
//
// A constant that survives is handed on as float64, and internal/keys encodes
// a number field from a float64. A json.Number left in place would be a value
// the key encoder has never seen; it would not round anything, it would refuse
// or mis-sort, and only on the declarations that were fine all along.
func TestAnIndexedNumberIsStillAnIndexedNumber(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", `{
  "collections": [
    {"name": "ids", "key": {"path": "id", "type": "string", "auto": "ulid"},
     "indexes": [{"name": "by_n", "fields": [{"path": "n", "type": "number", "missing": "skip"}]}]}
  ],
  "operations": [
    {"name": "ids.fixed", "collection": "ids", "action": "insert",
     "document": {"n": {"value": 9007199254740994, "constant": true}}},
    {"name": "ids.all", "collection": "ids", "action": "scan", "index": "by_n", "limit": 10}
  ]
}`)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatalf("apply: %s", errs)
	}

	db, closeDB, err := open(options{
		dir: setup.dir, account: "acme", db: "main", secret: secret, lookup: setup.env(nil),
	})
	if err != nil {
		t.Fatalf("opening the database the command just wrote: %v", err)
	}
	defer closeDB()

	if _, err = db.Invoke(store.Caller{}, "ids.fixed", 0, map[string]any{}); err != nil {
		t.Fatalf("running the declaration that carries the constant: %v", err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}

	result, err := db.Invoke(store.Caller{}, "ids.all", 0, map[string]any{})
	if err != nil {
		t.Fatalf("reading it back through the index: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("the index holds %d rows, want 1", len(result.Rows))
	}
	if got := result.Rows[0]["n"]; got != float64(9007199254740994) {
		t.Fatalf("the indexed value came back as %v (%T), want 9007199254740994", got, got)
	}
}

// TestSealingRefusesADraftItCannotSignFaithfully is the bundle door at the
// author's end, and one of the two honest limits this change has.
//
// A bundle is not the author's file; it is a re-emission of it. `seal` writes
// the declarations as marshalled Go values, so every constant in one comes out
// spelled the way a float64 prints — and for integers above roughly 1e17 that
// is not the spelling the author wrote. 9223372036854775808 is ACCEPTED by the
// rule (float64 holds 2^63 exactly, and nothing about the value is lost) and
// is written back out as 9223372036854776000, which is a different integer and
// not one a float64 holds. So the bundle would be refused by the install that
// read it.
//
// The refusal is right; where it landed was not. Without this check the author
// gets exit 0 and a signed artifact, and an operator somewhere else gets the
// refusal, days later, about a number nobody typed. Here it is refused at the
// moment the author is still looking at the file — and nothing is written.
//
// This is a real narrowing, and it is stated rather than hidden: a value in
// that class can be declared through `apply` and cannot be published in a
// bundle. See the CHANGELOG.
func TestSealingRefusesADraftItCannotSignFaithfully(t *testing.T) {
	setup := start(t)

	public, _, status := setup.run("keygen", filepath.Join(setup.dir, "acme.key"))
	if status != 0 {
		t.Fatalf("keygen: %d", status)
	}
	if strings.TrimSpace(public) == "" {
		t.Fatal("keygen printed no public key")
	}
	key, err := os.ReadFile(filepath.Join(setup.dir, "acme.key"))
	if err != nil {
		t.Fatal(err)
	}

	draftOf := func(constant string) string {
		return setup.write("draft-"+constant+".json", fmt.Sprintf(`{
  "format": "sapedb/bundle:v1",
  "name": "ids", "version": "1.0.0", "signer": "acme-eng",
  "collections": [
    {"name": "ids", "key": {"path": "id", "type": "string", "auto": "ulid"}, "indexes": []}
  ],
  "operations": [
    {"name": "ids.fixed", "collection": "ids", "action": "insert",
     "document": {"n": {"value": %s, "constant": true}}}
  ]
}`, constant))
	}

	// The one that cannot survive the round trip through a bundle.
	out := filepath.Join(setup.dir, "cannot.bundle.json")
	_, errs, status := setup.runWith(nil, string(key), "seal", draftOf("9223372036854775808"), out)
	if status == 0 {
		t.Fatal("a draft was sealed into a bundle that install would refuse")
	}
	if !strings.Contains(errs, "read back as itself") {
		t.Errorf("the refusal does not say why a bundle is different from a schema file: %q", errs)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused seal left a bundle file behind: %v", err)
	}

	// The control, and the reason this is a narrowing rather than a ban: a
	// constant that is spelled the same on the way out still seals, and the
	// bundle it writes still verifies.
	fine := filepath.Join(setup.dir, "fine.bundle.json")
	if _, errs, status := setup.runWith(nil, string(key), "seal", draftOf("9007199254740994"), fine); status != 0 {
		t.Fatalf("a constant that round-trips was refused: %q", errs)
	}
	trusted := map[string]string{"SAPEDB_TRUST": "acme-eng=" + strings.TrimSpace(public)}
	if _, errs, status := setup.runWith(trusted, "", "verify", fine); status != 0 {
		t.Fatalf("the bundle seal wrote does not verify: %q", errs)
	}
}

// TestADumpStillRestores is the other honest limit, pinned so that somebody
// does not close it and break the thing it protects.
//
// `restore` deliberately does NOT get this check, and the reason is that a
// dump is not somebody's declaration — it is this database's own printout,
// already spelled the way a float64 prints. An accepted constant of
// 9223372036854775808 is dumped as 9223372036854776000, and that text is a
// plain integer float64 does not hold exactly, so a checked restore would
// refuse this database's own dump. Losing dump-and-restore is a worse outcome
// than a hand-edited dump carrying a number nobody can store.
//
// So restore is the one path here that takes a number on trust, it says so in
// store.Restore's own comment, and this test is what stops the exemption being
// removed by somebody who only reads the rule.
func TestADumpStillRestores(t *testing.T) {
	from := start(t)
	file := from.write("schema.json", schemaWith("9223372036854775808"))
	if _, errs, status := from.run("apply", file); status != 0 {
		t.Fatalf("apply: %s", errs)
	}
	dumped, errs, status := from.run("dump")
	if status != 0 {
		t.Fatalf("dump: %s", errs)
	}
	// The spelling the database prints, which is not the spelling that was
	// declared — that is the whole reason this test exists.
	if !strings.Contains(dumped, `"value":9223372036854776000`) {
		t.Fatalf("the dump does not hold the re-spelled constant:\n%s", dumped)
	}

	into := start(t)
	out, errs, status := into.runWith(nil, dumped, "restore")
	if status != 0 {
		t.Fatalf("this database cannot restore its own dump: %s", errs)
	}
	if !strings.Contains(out, "restored to change") {
		t.Fatalf("restore printed %q", out)
	}
}

// TestApplyItselfRefusesWithoutRelyingOnItsArgumentCheck is the guard behind
// the guard, and it exists because a mutation found it missing.
//
// checkApply and apply() read and decode the same file on purpose — checkApply's
// own comment says why — and checkApply runs first, so every test that goes
// through Run() is satisfied by checkApply alone. Break apply()'s decoder or
// delete apply()'s own check and the whole suite stays green, which means the
// duplication was decorative: two copies where only one was measured.
//
// So this calls apply() directly, on an open store, the way
// TestARefusedInstallLeavesNothingInTheOPENStoreEither calls install(). What
// it measures is that the second copy is a real second copy — and that a
// caller in the same process that keeps hold of the store after a refused run
// is not handed a collection that will never be committed.
func TestApplyItselfRefusesWithoutRelyingOnItsArgumentCheck(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", schemaWith("9007199254740993"))

	db, closeDB, err := open(options{
		dir: setup.dir, account: "acme", db: "main", secret: secret, lookup: setup.env(nil),
	})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer closeDB()

	out := &strings.Builder{}
	err = apply(db, []string{file}, out, store.Caller{Actor: "acme (apply)"})
	if err == nil {
		t.Fatal("apply() stored a number it cannot hold as sent")
	}
	if !strings.Contains(err.Error(), "9007199254740993") || !strings.Contains(err.Error(), "9007199254740992") {
		t.Errorf("the refusal does not name the value and what would have been stored: %v", err)
	}
	if out.String() != "" {
		t.Errorf("a refused apply printed progress: %q", out.String())
	}
	if held := db.Collections(); len(held) != 0 {
		t.Errorf("a refused apply left %v in the open store", held)
	}

	// The control on the same store: a constant that round-trips still goes
	// in, so the refusal above is the check and not a broken apply().
	good := setup.write("good.json", schemaWith("9007199254740994"))
	if err := apply(db, []string{good}, &strings.Builder{}, store.Caller{Actor: "acme (apply)"}); err != nil {
		t.Fatalf("a constant that round-trips was refused: %v", err)
	}
}
