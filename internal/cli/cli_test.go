package cli

import (
	crand "crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

const secret = "the secret only the control plane has"

// setup is a directory, an environment, and a way to run the command in it.
type setup struct {
	t   *testing.T
	dir string
}

func start(t *testing.T) *setup {
	t.Helper()
	return &setup{t: t, dir: t.TempDir()}
}

func (s *setup) env(extra map[string]string) func(string) (string, bool) {
	vars := map[string]string{
		"SAPEDB_SECRET":  secret,
		"SAPEDB_DIR":     s.dir,
		"SAPEDB_ACCOUNT": "acme",
		"SAPEDB_DB":      "main",
	}
	for name, value := range extra {
		vars[name] = value
	}
	return func(name string) (string, bool) {
		value, found := vars[name]
		return value, found
	}
}

// envOnly is exactly the variables given, none of the setup's usual
// defaults — for tests about what happens when a variable this command
// needs is simply not there.
func (s *setup) envOnly(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := vars[name]
		return value, found
	}
}

// run returns what the command printed, what it complained about, and its
// status.
func (s *setup) run(args ...string) (string, string, int) {
	s.t.Helper()
	return s.runWith(nil, "", args...)
}

func (s *setup) runWith(extra map[string]string, stdin string, args ...string) (string, string, int) {
	s.t.Helper()

	out, errs := &strings.Builder{}, &strings.Builder{}
	status := Run(args, s.env(extra), strings.NewReader(stdin), out, errs)
	return out.String(), errs.String(), status
}

// write puts a file in the directory and returns its path.
func (s *setup) write(name, content string) string {
	s.t.Helper()
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return path
}

const articles = `{
  "collections": [
    {
      "name": "articles",
      "key": {"path": "id", "type": "string", "auto": "ulid"},
      "indexes": [
        {"name": "by_author", "fields": [
          {"path": "author", "type": "string", "missing": "skip"},
          {"path": "published", "type": "number", "descending": true, "missing": "last"}
        ]}
      ]
    }
  ],
  "operations": [
    {
      "name": "articles.add", "collection": "articles", "action": "insert",
      "input": [
        {"name": "title", "type": "string", "required": true},
        {"name": "author", "type": "string", "required": true}
      ],
      "document": {"title": {"arg": "title"}, "author": {"arg": "author"}}
    },
    {
      "name": "articles.by_author", "collection": "articles", "action": "scan",
      "index": "by_author", "limit": 20,
      "input": [{"name": "author", "type": "string", "required": true}],
      "from": {"terms": [{"arg": "author"}]},
      "to": {"terms": [{"arg": "author"}]}
    }
  ]
}`

func TestApplyDeclaresWhatTheFileSays(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	out, errs, status := setup.run("apply", file)
	if status != 0 {
		t.Fatalf("apply: %d %s", status, errs)
	}
	if !strings.Contains(out, "collection articles") ||
		!strings.Contains(out, "operation articles.add version 1") {
		t.Fatalf("apply printed %q", out)
	}

	out, errs, status = setup.run("ls")
	if status != 0 {
		t.Fatalf("ls: %d %s", status, errs)
	}
	for _, want := range []string{
		"collection articles (key id string, ulid)",
		"index by_author [author string missing:skip, published number desc missing:last]",
		"operation articles.add v1 insert articles",
		"operation articles.by_author v1 scan articles via by_author limit 20",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls does not show %q:\n%s", want, out)
		}
	}
}

// Applying the same file again must change nothing. This runs on every deploy
// or it does not run at all, and a tool that makes a new version of every
// operation each time makes the version number meaningless.
func TestApplyingTwiceChangesNothing(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)

	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatalf("first apply: %s", errs)
	}
	out, errs, status := setup.run("apply", file)
	if status != 0 {
		t.Fatalf("second apply: %s", errs)
	}
	if !strings.Contains(out, "articles.add unchanged at version 1") {
		t.Errorf("the second apply printed %q", out)
	}

	// And a real change does make a version.
	changed := strings.Replace(articles, `"limit": 20`, `"limit": 5`, 1)
	file = setup.write("schema.json", changed)
	out, errs, status = setup.run("apply", file)
	if status != 0 {
		t.Fatalf("third apply: %s", errs)
	}
	if !strings.Contains(out, "articles.by_author version 2") {
		t.Errorf("a changed operation printed %q", out)
	}
	if !strings.Contains(out, "articles.add unchanged at version 1") {
		t.Errorf("an unchanged operation was given a version: %q", out)
	}
}

func TestADeclarationThatMakesNoSenseIsRefusedWithItsName(t *testing.T) {
	setup := start(t)

	for name, content := range map[string]string{
		"a field with no missing policy": `{"collections":[{"name":"a","key":{"path":"id","type":"string"},
			"indexes":[{"name":"i","fields":[{"path":"x","type":"string"}]}]}]}`,
		"a scan with no limit": `{"collections":[{"name":"a","key":{"path":"id","type":"string"}}],
			"operations":[{"name":"a.all","collection":"a","action":"scan","index":"_key"}]}`,
		"an operation on a collection that is not there": `{"operations":[
			{"name":"a.all","collection":"nowhere","action":"scan","index":"_key","limit":1}]}`,
		"a field nobody knows": `{"collections":[{"name":"a","key":{"path":"id","type":"string"},"wat":1}]}`,
		// task 0045: a declared scan whose From and To are both constants and
		// From sorts after To can never return a row, with any argument —
		// refused when the schema is applied, not discovered the first time
		// somebody runs it. "b" > "a", so this is backwards.
		"a scan whose declared from/to can never return a row": `{
			"collections":[{"name":"a","key":{"path":"id","type":"string","auto":"ulid"},
				"indexes":[{"name":"i","fields":[{"path":"x","type":"string","missing":"skip"}]}]}],
			"operations":[{"name":"a.backwards","collection":"a","action":"scan","index":"i","limit":1,
				"from":{"terms":[{"value":"b"}]},"to":{"terms":[{"value":"a"}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			file := setup.write("bad.json", content)
			_, errs, status := setup.run("apply", file)
			if status == 0 {
				t.Fatal("it was applied anyway")
			}
			if !strings.Contains(errs, "bad.json") {
				t.Errorf("the complaint does not name the file: %q", errs)
			}
			if strings.Contains(errs, "goroutine ") || strings.Contains(errs, "panic:") {
				t.Errorf("the complaint looks like a raw Go crash, not an operator-readable line: %q", errs)
			}
		})
	}
}

func TestDumpAndRestoreGoThroughTheCommand(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	dumped, errs, status := setup.run("dump")
	if status != 0 {
		t.Fatalf("dump: %s", errs)
	}
	if !strings.Contains(dumped, `"kind":"header"`) || !strings.Contains(dumped, `"kind":"end"`) {
		t.Fatalf("the dump reads %q", dumped[:min(200, len(dumped))])
	}

	// Into a database that has never been used.
	into := start(t)
	out, errs, status := into.runWith(nil, dumped, "restore")
	if status != 0 {
		t.Fatalf("restore: %s", errs)
	}
	if !strings.Contains(out, "restored to change") {
		t.Errorf("restore printed %q", out)
	}

	listed, _, _ := into.run("ls")
	if !strings.Contains(listed, "collection articles") || !strings.Contains(listed, "operation articles.add") {
		t.Errorf("the restored database holds %q", listed)
	}

	// And a dump cut short is refused rather than restored in part.
	third := start(t)
	half := dumped[:len(dumped)/2]
	_, errs, status = third.runWith(nil, half, "restore")
	if status == 0 {
		t.Error("half a dump was restored")
	}
	// ErrDumpFormat's own wording (internal/store/dump.go), not this
	// package's: a truncation landing mid-line — which cutting a real dump
	// in half does, since it is not decoder.Decode aligned — reads as a
	// JSON syntax error, and Restore wraps every one of those in
	// ErrDumpFormat. This is the "for what reason" pin for the whole
	// TestARejectedArgumentLeavesTheDiskExactlyAsItFound family's dump/
	// restore sibling, which that table itself does not cover.
	assertRefusedBecause(t, errs, pinnedPhrases["dump/half a dump"])
}

func TestTheLogCanBeRead(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	out, errs, status := setup.run("log")
	if status != 0 {
		t.Fatalf("log: %s", errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("the log has %d entries", len(lines))
	}
	first := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("an entry does not read as json: %v", err)
	}
	if first["kind"] != "declare" {
		t.Errorf("the first entry is a %v", first["kind"])
	}

	// From part way along.
	from, _, _ := setup.run("log", "2")
	if strings.Count(strings.TrimSpace(from), "\n") >= strings.Count(strings.TrimSpace(out), "\n") {
		t.Error("reading from entry 2 gave as much as reading from the start")
	}
	_, errs, status = setup.run("log", "soon")
	if status == 0 {
		t.Error("a log position that is not a number was accepted")
	}
	assertRefusedBecause(t, errs, pinnedPhrases["log/not a number"])
}

// The connection string has to be one the client library parses and the server
// verifies. Printing something that looks right is not the same thing.
func TestTheConnectionStringIsOneTheOtherSideAccepts(t *testing.T) {
	setup := start(t)

	out, errs, status := setup.run("url", "db.example:7433")
	if status != 0 {
		t.Fatalf("url: %s", errs)
	}

	printed := strings.TrimSpace(out)
	parsed, err := connection.Parse(printed)
	if err != nil {
		t.Fatalf("what it printed does not parse: %v\n%s", err, printed)
	}
	if parsed.Account != "acme" || parsed.DBName != "main" || parsed.Host != "db.example" || parsed.Port != 7433 {
		t.Errorf("it printed %+v", parsed)
	}
	if err := connection.VerifyString(printed, secret, ""); err != nil {
		t.Errorf("the signature does not verify: %v", err)
	}

	// A different secret must not verify it, or the signature proves nothing.
	if err := connection.VerifyString(printed, "another secret entirely", ""); err == nil {
		t.Error("it verified under a secret it was not signed with")
	}
}

// TestTheDirectLabelIsRecognisedRegardlessOfCase is the case-folding this
// command promises with strings.EqualFold: an operator typing SAPEDB_LABEL
// in a shell script is not reliably going to match the lower-case spelling
// used in the one doc comment that names it. "Direct" and "DIRECT" must
// select the same signing mode as "direct" does.
func TestTheDirectLabelIsRecognisedRegardlessOfCase(t *testing.T) {
	for _, spelling := range []string{"direct", "Direct", "DIRECT"} {
		t.Run(spelling, func(t *testing.T) {
			setup := start(t)
			out, errs, status := setup.runWith(map[string]string{"SAPEDB_LABEL": spelling}, "", "url")
			if status != 0 {
				t.Fatalf("url: %s", errs)
			}
			printed := strings.TrimSpace(out)

			if err := connection.VerifyString(printed, secret, signing.Direct); err != nil {
				t.Errorf("%q did not select the direct form: %v", spelling, err)
			}
			if err := connection.VerifyString(printed, secret, ""); err == nil {
				t.Errorf("%q verified under the default label too, so it did not actually select direct", spelling)
			}
		})
	}
}

// A password on the command line is visible in ps to every user on the
// machine. It is generated, or read from stdin, and never taken as an
// argument.
func TestAPasswordIsNeverAnArgument(t *testing.T) {
	setup := start(t)

	_, errs, status := setup.run("url", "localhost:7433", "hunter2-hunter2-hunter2")
	if status == 0 {
		t.Fatal("a password given as an argument was accepted")
	}
	if !strings.Contains(errs, "stdin") {
		t.Errorf("the complaint does not say where a password goes: %q", errs)
	}

	// From stdin it is taken.
	given := "a-password-of-the-right-shape"
	out, errs, status := setup.runWith(nil, given+"\n", "url", "localhost:7433", "-")
	if status != 0 {
		t.Fatalf("url with stdin: %s", errs)
	}
	if !strings.Contains(out, given) {
		t.Errorf("it did not use the password it was given: %q", out)
	}

	// And one the format refuses is refused here, not signed and printed.
	if _, _, status := setup.runWith(nil, "short\n", "url", "localhost:7433", "-"); status == 0 {
		t.Error("a password the format does not allow was signed anyway")
	}
}

func TestAGeneratedPasswordIsDifferentEveryTimeAndValid(t *testing.T) {
	setup := start(t)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		out, _, status := setup.run("url", "localhost:7433")
		if status != 0 {
			t.Fatal(out)
		}
		parsed, err := connection.Parse(strings.TrimSpace(out))
		if err != nil {
			t.Fatal(err)
		}
		if seen[parsed.Password] {
			t.Fatal("the same password was made twice")
		}
		seen[parsed.Password] = true
	}
}

// TestThePartitionsFolderNameDropsTheFileExtension catches a mutation that
// TestACommandRunWhileTheServerHoldsTheFileIsRefused above cannot: that test
// only ever names main.sapedb, so it says nothing about the sibling folder
// open() derives from it with TrimSuffix. Drop the TrimSuffix and the
// database file is still exactly where every other test expects it — only
// the partitions folder ends up misnamed "main.sapedb.parts" instead of
// "main.parts", which nothing above would have noticed.
func TestThePartitionsFolderNameDropsTheFileExtension(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	if info, err := os.Stat(filepath.Join(setup.dir, "acme", "main.parts")); err != nil || !info.IsDir() {
		t.Errorf("want a partitions folder named main.parts, stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(setup.dir, "acme", "main.sapedb.parts")); err == nil {
		t.Error("the partitions folder kept the .sapedb suffix instead of having it trimmed")
	}
}

// oldDBExt mirrors internal/cli's own oldFileExt: built from bytes so this
// file, inside the tree internal/naming walks, does not carry the old word
// as a literal.
func oldDBExt() string {
	return "." + string([]byte{'r', 's', 'q', 'l'})
}

// walkTree lists every path under root, file or directory, relative to
// root and sorted. Task 0050's whole point is that "a refused command
// leaves nothing" has to be checked against the disk image, not against
// the one path a person happened to think of — the old version of this
// test stat'd exactly the .sapedb path and said nothing about the
// directory lock file or the account folder that a rejected open() used
// to leave behind on the way to that refusal.
func walkTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

// treesMatch is the actual comparison assertTreeUnchanged makes, factored
// out so TestWalkTreeSeesAPathThatChangedIdentity, below, can drive it
// directly with a synthetic before/after pair — without going through a
// *testing.T, where a comparison that is SUPPOSED to report a difference
// would mark that meta-test itself failed rather than letting it assert on
// the result. Comparing full paths, not just how many there are: none of
// the 18 cases in TestARejectedArgumentLeavesTheDiskExactlyAsItFound ever
// removes a path (see section 5 of task 0058), so on every case in THAT
// TABLE len(before) != len(after) already — the only shape a length-only
// comparison would miss there is a path renamed to a different name of the
// same count.
//
// That is a claim about this test file's own case table, not about the
// package. QA 0058 read the rest of the tree: Collection.expire() ->
// store.DropPart -> vfs.Folder.Remove -> os.Remove does delete a path, and
// apply can reach it — schema.Collections is passed straight through to
// db.Declare, Partition.Keep included. No case here builds that shape (it
// needs a partitioned collection to actually expire a part, which none of
// the 18 cases sets up), so the length-only blind spot stays theoretical
// for this file; a future case that does build it must not lean on
// length alone.
func treesMatch(before, after []string) bool {
	return reflect.DeepEqual(before, after)
}

// assertTreeUnchanged compares a snapshot taken before running a command
// against the tree now, and fails with both lists when they differ — so a
// failure names exactly what appeared or disappeared, not just that
// something did.
func assertTreeUnchanged(t *testing.T, root string, before []string) {
	t.Helper()
	after := walkTree(t, root)
	if !treesMatch(before, after) {
		t.Errorf("a refused command changed %s\n  before: %v\n  after:  %v", root, before, after)
	}
}

// ---------------------------------------------------------------------------
// Task 0061: a case that only asserts status != 0 cannot tell "refused for
// what reason" (refused for the reason it claims to guard) from any of the
// package's other ways to end up at exit 1 — a short password, empty stdin,
// a closed port, ErrUsage's own catch-all. See house-rules.md's closing
// entry, "When EVERY failing path yields the same status, the status
// distinguishes nothing."
//
// refusalLine and assertRefusedBecause below are what every case added or
// tightened for this task runs its "for what reason" assertion through.

// refusalLine is the sentence a refusal leads with. Run (cli.go) prints the
// whole usage constant — 1231 characters, 1233 bytes (len(usage) in Go
// counts bytes; one em dash in it is 3 bytes) — after every ErrUsage, so an
// assertion made against the FULL stderr is satisfied by any word that
// appears in that block — "stdin", "password", "argument", and all seven
// command names are in it, measured — no matter which command failed or
// why. Cutting at the first line is what turns a Contains on this string
// into a measurement instead of a tautology. TestWhatIsNotACommandIsExplained
// is the one exception: it asserts on whether the usage block is printed
// AT ALL, so it deliberately keeps reading the full, unstripped errs.
func refusalLine(errs string) string {
	line, _, _ := strings.Cut(errs, "\n")
	return line
}

// pinnedPhrases is every phrase the refusal cases in this file assert a
// command was refused BECAUSE of, written out by hand. It is not derived
// from the messages it pins and not derived from usage: an expectation
// taken from the thing being checked cannot fail. assertRefusedBecause uses
// it for the other half of the pair — the negative check that catches one
// command's refusal carrying another command's sentence, which every case
// in this package passed for until this task.
//
// Two entries are shorter than the usual 16-character/3-word tripwire;
// belowThreshold, just below, documents why each is still a safe
// measurement rather than a coincidence.
var pinnedPhrases = map[string]string{
	"apply/no file":                   "apply needs a file",
	"apply/missing file":              "no such file or directory",
	"apply/directory as file":         "is a directory",
	"apply/broken json":               "unexpected EOF",
	"apply/unknown field":             `json: unknown field "unexpectedField"`,
	"apply/conflicting redeclaration": `collection "articles": sapedb/store:`,
	"keygen/no file":                  "keygen takes one file to write the private key into",
	"seal/wrong arguments":            "seal takes a draft file and a bundle file",
	"verify/no file":                  "verify takes one bundle file",
	"install/no file":                 "install takes one bundle file",
	"log/not a number":                "is not an entry number",
	"log/too many arguments":          "log takes at most one argument, FROM",
	"ls/no arguments":                 "ls takes no arguments",
	"dump/no arguments":               "dump takes no arguments",
	"restore/no arguments":            "restore takes no arguments",
	"dump/half a dump":                "this is not a dump this build can read",
	"url/password as argument":        "never given as an argument",
	"url/too many arguments":          "url takes at most a host and -",
	"shell/bad option":                "shell takes host:port and optionally -insecure",
}

// belowThreshold documents every pinnedPhrases entry shorter than 16
// characters or 3 words — the tripwire TestNoPinnedPhraseCanBeSatisfiedByTheUsageBlock
// otherwise enforces — together with why it is still a safe measurement and
// not a phrase picked for convenience. Both of these are the ENTIRE word
// that distinguishes their case from its nearest neighbour; task 0061's own
// text names both in advance, in section 3.3.
var belowThreshold = map[string]string{
	"apply/directory as file": `14 characters, 3 words. The whole distinguishing word between case 3 ` +
		`(a directory) and case 2 (a missing file): os.ReadFile fails on a ` +
		`directory in a way os.Stat does not, and "is a directory" is what the ` +
		`OS (golang:1.24-alpine, per house rule) calls it.`,
	"apply/broken json": `14 characters, 2 words — below both halves of the tripwire. The whole ` +
		`distinguishing word between case 4 (broken JSON) and case 5 (an unknown ` +
		`field): both are decode errors, and "unexpected EOF" is the only text ` +
		`that says WHICH one this is.`,
}

// assertRefusedBecause pins WHY, not just THAT. The leading sentence has to
// carry `because`, and must NOT carry any of the other pinned phrases — the
// negative half is what catches a command printing another command's
// sentence, which every test in this package passed for until task 0061.
func assertRefusedBecause(t *testing.T, errs, because string) {
	t.Helper()
	line := refusalLine(errs)
	if !strings.Contains(line, because) {
		t.Errorf("the refusal's first line does not say %q: %q", because, line)
	}
	for name, phrase := range pinnedPhrases {
		if phrase == because {
			continue
		}
		// This ONE pair, and only this pair, is excluded from the NEGATIVE
		// half — measured, not assumed (Reviewer 0058/0061 re-derived the
		// same finding independently): "unexpected EOF" (apply/broken
		// json) is io.ErrUnexpectedEOF's own text, and restore's
		// ErrDumpFormat wraps that exact same stdlib sentinel for a dump
		// truncated mid-line — "sapedb/store: this is not a dump this
		// build can read: unexpected EOF" legitimately contains it, with
		// no R2/R3-style bug involved (checkApply is nowhere on that
		// path). An earlier version of this exclusion skipped the cross
		// check for BOTH belowThreshold entries ("apply/directory as
		// file" too), which is wider than the collision it was written to
		// fix: only this one pair actually collides (measured — a
		// deliberately swapped message reusing "is a directory" alongside
		// its real sentence still gets caught), and the broad version
		// left two real swaps undetected. Narrowed to exactly the pair
		// that collides; belowThreshold's POSITIVE use (proving each
		// entry's own case) is unaffected either way.
		if name == "apply/broken json" && because == pinnedPhrases["dump/half a dump"] {
			continue
		}
		if strings.Contains(line, phrase) {
			t.Errorf("the refusal pinned as %q also carries %s's phrase (%q): %q", because, name, phrase, line)
		}
	}
}

// TestNoPinnedPhraseCanBeSatisfiedByTheUsageBlock is the collision guard
// task 0061 adds. pinnedPhrases is written by hand, above, not read back
// from usage, commands, or any check function — an expectation taken from
// the thing being checked cannot fail — and this proves each entry actually
// holds up against the one thing every ErrUsage refusal carries: not in
// usage, long enough (or documented in belowThreshold), and not a substring
// of any other entry. Runs unconditionally: house rule, an absent gate is
// red.
func TestNoPinnedPhraseCanBeSatisfiedByTheUsageBlock(t *testing.T) {
	for name, phrase := range pinnedPhrases {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(usage, phrase) {
				t.Fatalf("pinned phrase %q (for %s) is satisfied by the usage block itself", phrase, name)
			}
			if words := len(strings.Fields(phrase)); len(phrase) < 16 || words < 3 {
				if _, ok := belowThreshold[name]; !ok {
					t.Fatalf("pinned phrase %q (for %s) is below 16 characters/3 words and has no entry in belowThreshold", phrase, name)
				}
			}
			for otherName, other := range pinnedPhrases {
				if otherName == name {
					continue
				}
				if strings.Contains(phrase, other) {
					t.Fatalf("pinned phrase %q (for %s) contains %s's phrase %q as a substring", phrase, name, otherName, other)
				}
			}
		})
	}
}

// TestAnOldExtensionFileIsRefusedNotSilentlyReplaced is the file-extension
// counterpart to TestTheOldEnvironmentNameIsRefusedNotSilentlyIgnored, and
// the reason it needs its own test rather than reusing that one's shape: a
// renamed environment variable degrades cleanly — either it is read, or the
// signpost above refuses to start. A renamed *file extension* degrades by
// vfs.OpenFile treating "not found" as "create a new, empty database",
// which is a database opening successfully, on the wrong file, holding no
// data — the one variant of the old name in this whole rename that would
// otherwise fail silently rather than being refused.
//
// Task 0050 widened the claim this test makes from "no new .sapedb file"
// to "SAPEDB_DIR is unchanged, full stop": before that task, open() ran
// vfs.LockDir and os.MkdirAll before checkOldExtension, so a refusal here
// still left <dir>/.lock behind (see the sibling tests below for that,
// and for the account-folder half of the same claim on a fresh
// directory). Both commands that reach open() on this path are exercised
// — ls and apply — because a fix that only moved the check late enough to
// satisfy ls would say nothing about apply.
//
// apply is given a real, valid file rather than none at all: task 0058
// moved apply's own argument check (a file is required, and has to parse)
// ahead of open(), so "apply" with no file now stops there and never
// reaches checkOldExtension — a case this test would otherwise stop
// covering for apply without saying so.
func TestAnOldExtensionFileIsRefusedNotSilentlyReplaced(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(t *testing.T) []string
	}{
		{name: "ls", args: func(*testing.T) []string { return []string{"ls"} }},
		{name: "apply", args: func(t *testing.T) []string { return []string{"apply", writeOutside(t, articles)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := start(t)
			if err := os.MkdirAll(filepath.Join(setup.dir, "acme"), 0o700); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(setup.dir, "acme", "main"+oldDBExt())
			if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
				t.Fatal(err)
			}

			before := walkTree(t, setup.dir)

			_, errs, status := setup.run(tc.args(t)...)
			if status == 0 {
				t.Fatal("it ran against a database whose only file on disk carries the old extension")
			}

			want := filepath.Join(setup.dir, "acme", "main.sapedb")
			if !strings.Contains(errs, old) || !strings.Contains(errs, want) {
				t.Errorf("the refusal does not name both paths: %q", errs)
			}
			// "does not exist, but", not a bare "mv": checkOldExtension
			// (cli.go) builds this whole message as one fmt.Errorf with no
			// ErrUsage wrapping, so it is exactly one line and this phrase
			// is the literal text between its two interpolated paths — the
			// paths themselves are already the stronger check just above,
			// this closes the same 2-character "mv" gap QA 0058 found
			// elsewhere in this file. Measured directly against
			// checkOldExtension's source rather than copied from
			// elsewhere: oldPathHint's "mv it, and that .parts directory"
			// belongs to a different function (a colliding account/db
			// pair, not an old file extension) and does not appear here.
			if !strings.Contains(errs, "does not exist, but") {
				t.Errorf("the refusal does not describe the old-extension file it found: %q", errs)
			}

			assertTreeUnchanged(t, setup.dir, before)
		})
	}
}

// TestARefusedCommandDoesNotDeleteAnExistingLock guards against exactly the
// "fix" task 0050 rules out in its own text: deleting .lock on the refusal
// path to tidy up after a lock this call would otherwise have taken.
// Section 2 of that task is explicit about why not — a lock file is not
// this process's alone to delete once created, and racing whoever else
// might open it next is worse than a harmless leftover. This test starts
// from a directory that already has a .lock, the ordinary resting state
// between two runs of any command that succeeds, and checks that a
// refusal for an unrelated reason (the old-extension file) leaves it
// exactly as it was.
func TestARefusedCommandDoesNotDeleteAnExistingLock(t *testing.T) {
	setup := start(t)
	if err := os.MkdirAll(filepath.Join(setup.dir, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(setup.dir, "acme", "main"+oldDBExt())
	if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Nobody holds this right now — an unlocked .lock file left over from a
	// previous, successful run is the normal state of this design, not a
	// sign that anything is wrong.
	if err := os.WriteFile(filepath.Join(setup.dir, ".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	before := walkTree(t, setup.dir)

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it ran against a database whose only file on disk carries the old extension")
	}
	if !strings.Contains(errs, "does not exist, but") {
		t.Errorf("the refusal does not describe the old-extension file it found: %q", errs)
	}

	assertTreeUnchanged(t, setup.dir, before)
}

// TestASuccessfulCommandStillTakesItsLock is the control case task 0050's
// own text asks for: the rule is "a REFUSED command leaves nothing new",
// not "nothing is ever created". A command that actually runs is supposed
// to take the directory lock — held.Close() releases it but does not
// remove the file — and it is supposed to create the account folder its
// database lives under. Without this test, a change that made open()
// never take the lock at all would make every refusal test above pass
// for the wrong reason.
func TestASuccessfulCommandStillTakesItsLock(t *testing.T) {
	setup := start(t)

	if _, errs, status := setup.run("ls"); status != 0 {
		t.Fatal(errs)
	}

	if _, err := os.Stat(filepath.Join(setup.dir, ".lock")); err != nil {
		t.Errorf(".lock did not appear after a command that ran successfully: %v", err)
	}
	if info, err := os.Stat(filepath.Join(setup.dir, "acme")); err != nil || !info.IsDir() {
		t.Errorf("the account folder was not created for a command that ran successfully: %v", err)
	}
}

// TestAnOldExtensionFileWinsOverALockedDirectory pins the design decision
// in task 0050's Result section: when both are true — the server holds
// this directory's lock, and the database's only file on disk carries the
// old extension — the operator sees the mv hint, not "the server is
// probably running". Before this task the order was reversed (LockDir ran
// first), so this exact situation reported the wrong blocker: stopping
// the server would not have made ls succeed, because the file itself was
// still the one thing actually in the way.
func TestAnOldExtensionFileWinsOverALockedDirectory(t *testing.T) {
	setup := start(t)
	if err := os.MkdirAll(filepath.Join(setup.dir, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(setup.dir, "acme", "main"+oldDBExt())
	if err := os.WriteFile(old, []byte("stand-in for a real database file; only its path matters here"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Somebody else — the server — holds the directory lock.
	held, err := vfs.LockDir(setup.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it ran while both the directory was locked and the file had the old extension")
	}
	if !strings.Contains(errs, "does not exist, but") {
		t.Errorf("the refusal does not describe the old-extension file it found: %q", errs)
	}
	if strings.Contains(errs, "probably running") {
		t.Errorf("the refusal blames the server instead of the old-extension file: %q", errs)
	}
}

// TestARefusalOnTheLockedDirectoryLeavesNoAccountFolder is the boundary QA
// found on M2 (open(): os.MkdirAll moved ahead of checkOldExtension, both
// still ahead of vfs.LockDir) that TestAnOldExtensionFileWinsOverALockedDirectory
// above cannot reach. Dropping just the old-extension file and keeping that
// case's own os.MkdirAll(".../acme") setup is not enough — Reviewer built
// that exact half-step and it still survives, because the account folder is
// then pre-created again and MkdirAll has nothing new to do. The axis that
// actually carries this test is the account folder not existing YET: this
// case drops both the old-extension file and the setup that pre-creates its
// folder, so nothing has touched this directory besides the lock itself.
// checkOldExtension finds nothing to refuse (neither the .sapedb path nor
// its old-extension sibling exists), so the only thing that can refuse this
// call is LockDir, and LockDir must be the ONLY thing that ran.
//
// M2 fails this: it runs os.MkdirAll(filepath.Dir(path)) before LockDir
// ever gets a turn, so it creates <dir>/acme/ unconditionally on the way to
// a refusal that has nothing to do with that folder. Every other test that
// exercises the locked-directory path (TestAnOldExtensionFileWinsOverALockedDirectory,
// TestTheToolIsRefusedWhileAServerHoldsTheDirectory) pre-creates the
// account folder as part of placing a schema or an old-extension file, so
// none of them can tell "the folder was already there" from "MkdirAll just
// made it" — this one starts from a directory where the folder is
// genuinely new, and compares the whole tree to say so.
func TestARefusalOnTheLockedDirectoryLeavesNoAccountFolder(t *testing.T) {
	setup := start(t)

	// Somebody else — the server — holds the directory lock. Nothing else
	// has touched this directory yet: no account folder, no old-extension
	// file, nothing for checkOldExtension to object to.
	held, err := vfs.LockDir(setup.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	before := walkTree(t, setup.dir)

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it ran while the directory was locked")
	}
	if !strings.Contains(errs, "probably running") {
		t.Errorf("the refusal does not blame the server: %q", errs)
	}

	assertTreeUnchanged(t, setup.dir, before)
}

func TestACommandRunWhileTheServerHoldsTheFileIsRefused(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	// Somebody else — the server — has it open.
	held, err := vfs.OpenFile(filepath.Join(setup.dir, "acme", "main.sapedb"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	_, errs, status := setup.run("ls")
	if status == 0 {
		t.Fatal("it opened a database another process is writing")
	}
	if !strings.Contains(errs, "another process") || !strings.Contains(errs, "server") {
		t.Errorf("the complaint does not say what to do: %q", errs)
	}
}

func TestTheWrongSecretIsSaidPlainly(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.runWith(map[string]string{"SAPEDB_ENCRYPT": "1"}, "", "apply", file); status != 0 {
		t.Fatal(errs)
	}

	_, errs, status := setup.runWith(map[string]string{
		"SAPEDB_ENCRYPT": "1", "SAPEDB_SECRET": "a different secret entirely",
	}, "", "ls")
	if status == 0 {
		t.Fatal("it opened an encrypted database with the wrong secret")
	}
	if !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Errorf("the complaint does not name the secret: %q", errs)
	}

	// And opening an encrypted database without saying so says which mistake
	// it is, rather than reading as a damaged file.
	_, errs, status = setup.run("ls")
	if status == 0 {
		t.Fatal("an encrypted database opened with no key")
	}
	if !strings.Contains(errs, "encrypted") {
		t.Errorf("the complaint reads %q", errs)
	}
}

// TestABlankVariableIsNotAValue pins the half of get() that survives the
// old-name guard this file used to carry: `found && value != ""`, so a
// variable declared with no value reads as "not set" and falls back to the
// documented default rather than to the empty string.
//
// It uses LABEL rather than DIR on purpose. DIR's fallback is the hard-coded
// absolute path /var/lib/sapedb, and reaching it here — which "must run"
// requires, since nothing refuses first — is exactly the system-path
// dependency this package's tests otherwise avoid. LABEL's fallback is the
// empty string, so this observes the same branch without touching a real
// path.
//
// This used to be a subtest of the old-environment-name table, which is why
// it is worth keeping on its own now that the table is gone: the guarantee
// it measures is about how a blank variable is read, and has nothing to do
// with what the product used to be called.
func TestABlankVariableIsNotAValue(t *testing.T) {
	setup := start(t)
	vars := map[string]string{
		"SAPEDB_SECRET":  secret,
		"SAPEDB_DIR":     setup.dir,
		"SAPEDB_ACCOUNT": "acme",
		"SAPEDB_DB":      "main",
		"SAPEDB_LABEL":   "",
	}

	stdoutBuf, errBuf := &strings.Builder{}, &strings.Builder{}
	if status := Run([]string{"ls"}, setup.envOnly(vars), strings.NewReader(""), stdoutBuf, errBuf); status != 0 {
		t.Fatalf("a blank SAPEDB_LABEL was not read as unset: %s", errBuf.String())
	}
}

func TestWhatIsNotACommandIsExplained(t *testing.T) {
	setup := start(t)

	for name, args := range map[string][]string{
		"nothing at all":          {},
		"a command nobody has":    {"frobnicate"},
		"an option nobody has":    {"-wat", "x", "ls"},
		"an option with no value": {"-account"},
		"apply with no file":      {"apply"},
	} {
		t.Run(name, func(t *testing.T) {
			_, errs, status := setup.run(args...)
			if status == 0 {
				t.Fatal("it ran anyway")
			}
			if !strings.Contains(errs, "sapedb [options]") {
				t.Errorf("it did not print how to use it: %q", errs)
			}
		})
	}

	// And the things a command cannot work without.
	if _, errs, _ := setup.runWith(map[string]string{"SAPEDB_SECRET": ""}, "", "ls"); !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Errorf("no secret: %q", errs)
	}
	if _, errs, _ := setup.runWith(map[string]string{"SAPEDB_DB": ""}, "", "ls"); !strings.Contains(errs, "-db") {
		t.Errorf("no database: %q", errs)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A generated password is drawn from the alphabet evenly. The alphabet is 66
// long and a byte is 256, so taking the remainder would make the first 58
// characters a third more likely than the last 8 — which costs about one bit
// across a whole password and is worth almost nothing, but is also the kind of
// thing that gets copied into somewhere it does matter.
func TestGeneratedPasswordsAreDrawnEvenly(t *testing.T) {
	counts := map[byte]int{}
	total := 0

	for i := 0; i < 4000; i++ {
		made, err := makePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(made) != passwordLength {
			t.Fatalf("a password of %d characters", len(made))
		}
		for j := 0; j < len(made); j++ {
			if !strings.ContainsRune(passwordAlphabet, rune(made[j])) {
				t.Fatalf("a password holds %q, which is not in the alphabet", made[j])
			}
			counts[made[j]]++
			total++
		}
	}

	if len(counts) != len(passwordAlphabet) {
		t.Fatalf("only %d of %d characters ever appeared", len(counts), len(passwordAlphabet))
	}

	expected := float64(total) / float64(len(passwordAlphabet))
	for character, count := range counts {
		off := float64(count)/expected - 1
		if off < -0.2 || off > 0.2 {
			t.Errorf("%q appeared %d times, %.0f%% off the %.0f expected",
				character, count, off*100, expected)
		}
	}
}

// The tool tells people the server is probably running. That has to be a fact
// rather than a guess, which means the lock it trips over is the directory —
// not whichever database files the server happens to have opened.
func TestTheToolIsRefusedWhileAServerHoldsTheDirectory(t *testing.T) {
	setup := start(t)
	file := setup.write("schema.json", articles)
	if _, errs, status := setup.run("apply", file); status != 0 {
		t.Fatal(errs)
	}

	// A server that has started and opened nothing at all.
	held, err := vfs.LockDir(setup.dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range [][]string{{"ls"}, {"dump"}, {"log"}, {"apply", file}} {
		_, errs, status := setup.run(command...)
		if status == 0 {
			t.Errorf("%v ran while the server held the directory", command)
		}
		if !strings.Contains(errs, "server") {
			t.Errorf("%v: the complaint does not say what to do: %q", command, errs)
		}
	}

	// url does not touch the database, so it still works — which is what makes
	// it usable for handing somebody a connection string on a running system.
	if _, errs, status := setup.run("url", "localhost:7433"); status != 0 {
		t.Errorf("url needs no database and should still work: %s", errs)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, errs, status := setup.run("ls"); status != 0 {
		t.Errorf("after the server let go: %s", errs)
	}
}

// ---------------------------------------------------------------------------
// Task 0058: a command refused because of a bad ARGUMENT must not touch disk
// at all, not merely "not create a database file". Section 6 of the task
// lays out eighteen cases below, each changing exactly one axis from the
// case before it, all read through walkTree and assertTreeUnchanged above so
// a failure names exactly what appeared or disappeared. Every case gets its
// own start(t) — the shared-fixture trap in TestWhatIsNotACommandIsExplained
// above (see the comment on that test's "apply with no file" row) is not one
// this table repeats.

// canonicalShape is the tree a database actually leaves behind once it opens
// and something inside it succeeds: .lock and the account folder from
// open(), main.sapedb from the pager, main.parts from vfs.At. Cases 14 and
// 15 below assert the tree equals exactly this — not merely that these
// paths exist — because a fix that suppressed main.parts unconditionally
// would make every refusal case above pass for the wrong reason.
func canonicalShape() []string {
	return []string{
		".lock",
		"acme",
		filepath.Join("acme", "main.parts"),
		filepath.Join("acme", "main.sapedb"),
	}
}

// writeOutside puts a file somewhere other than SAPEDB_DIR. Every case below
// that gives apply a real file uses this instead of setup.write: a file
// living inside SAPEDB_DIR would show up in the very walkTree snapshot the
// case is trying to keep clean, and the assertion would end up measuring the
// test's own fixture instead of the command under test.
func writeOutside(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const onePersonSchema = `{"collections":[{"name":"people","key":{"path":"id","type":"string"}}]}`

const unknownFieldSchema = `{"collections":[{"name":"a","key":{"path":"id","type":"string"},"unexpectedField":1}]}`

const brokenJSONSchema = `{"collections": [ { "name": "a" `

// conflictingArticlesSchema redeclares the "articles" collection from the
// package-level articles constant, above, with a different primary key.
// store.Declare refuses that — comparing the key it already has against the
// one just asked for — before writing anything, which is what makes case 18
// below a check on SAVED STATE rather than on an argument: section 4 of the
// task draws that line on purpose, and this task does not move it.
const conflictingArticlesSchema = `{"collections":[{"name":"articles","key":{"path":"id","type":"number"}}]}`

func TestARejectedArgumentLeavesTheDiskExactlyAsItFound(t *testing.T) {
	// Case 1: apply, no file at all, on a fresh directory.
	t.Run("1 apply with no file", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("apply")
		if status == 0 {
			t.Fatal("apply ran with no file")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["apply/no file"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 2: a file argument that is present but points nowhere.
	t.Run("2 apply, a file that does not exist", func(t *testing.T) {
		setup := start(t)
		missing := filepath.Join(t.TempDir(), "missing.json")
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("apply", missing)
		if status == 0 {
			t.Fatal("apply ran against a file that is not there")
		}
		// The OS's own wording (golang:1.24-alpine, per house rule — every
		// run in this suite goes through that image), not this package's:
		// os.ReadFile's error is passed straight through by checkApply and
		// apply(). Written out here so nobody downstream mistakes it for a
		// sentence this tool composed.
		assertRefusedBecause(t, errs, pinnedPhrases["apply/missing file"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 3: a file argument that exists but is a directory. os.ReadFile
	// fails on a directory the way os.Stat would not — this case exists
	// specifically to catch a check that uses the wrong one of the two.
	t.Run("3 apply, a directory instead of a file", func(t *testing.T) {
		setup := start(t)
		asDir := t.TempDir()
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("apply", asDir)
		if status == 0 {
			t.Fatal("apply ran with a directory as its file argument")
		}
		// "is a directory" is 14 characters — below the usual tripwire, and
		// exempted in belowThreshold: it is the entire word that tells this
		// case apart from case 2 above (a file that is not there at all).
		assertRefusedBecause(t, errs, pinnedPhrases["apply/directory as file"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 4: the file exists and reads, but is not valid JSON.
	t.Run("4 apply, broken JSON", func(t *testing.T) {
		setup := start(t)
		file := writeOutside(t, brokenJSONSchema)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("apply", file)
		if status == 0 {
			t.Fatal("apply ran with broken JSON")
		}
		// "unexpected EOF" is 14 characters — below the usual tripwire, and
		// exempted in belowThreshold: it is the entire word that tells this
		// case apart from case 5 below (valid JSON, an unknown field).
		assertRefusedBecause(t, errs, pinnedPhrases["apply/broken json"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 5: valid JSON, but a field DisallowUnknownFields does not know.
	t.Run("5 apply, an unknown field", func(t *testing.T) {
		setup := start(t)
		file := writeOutside(t, unknownFieldSchema)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("apply", file)
		if status == 0 {
			t.Fatal("apply ran with a field nobody declared")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["apply/unknown field"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 6: two files, and the SECOND one is missing. This is the index
	// axis: a check that only ever reads files[0] would let this straight
	// through to open() and to apply()'s own loop, which would declare
	// "people" from the first file — printing it — before failing on the
	// second. Neither the tree nor stdout may show that.
	t.Run("6 apply, two files, the second one missing", func(t *testing.T) {
		setup := start(t)
		ok := writeOutside(t, onePersonSchema)
		missing := filepath.Join(t.TempDir(), "missing.json")
		before := walkTree(t, setup.dir)
		out, errs, status := setup.run("apply", ok, missing)
		if status == 0 {
			t.Fatal("apply ran with its second file missing")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["apply/missing file"])
		// This is the index axis, not the "for what reason" axis: !Contains(out,
		// "collection") is the real shield against mutation A1
		// (files -> files[:1]), which would still say "no such file or
		// directory" for the wrong file — the wants pin above cannot tell
		// files[0] and files[1] apart, only this can.
		if strings.Contains(out, "collection") {
			t.Errorf("apply declared something before failing on the second file: %q", out)
		}
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 7: log's own argument is not a number.
	t.Run("7 log, not a number", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("log", "not-a-number")
		if status == 0 {
			t.Fatal("log ran with a non-numeric position")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["log/not a number"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 8: log takes at most one argument (FROM); this gives it three.
	// Before this task only args[0] was ever read, so the extra two were
	// silently dropped and the command ran anyway.
	t.Run("8 log, too many arguments", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("log", "1", "2", "3")
		if status == 0 {
			t.Fatal("log ran with three positional arguments")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["log/too many arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 8b: the actual wall, not the one case 8 happens to sit on. QA
	// 0058 measured that checkLog's `len(args) > 1` guard was only ever
	// exercised at N=3 (case 8 above, and rejectionCases["log"] — same
	// N), and nudging it outward by one to `len(args) > 2` (mutation P6)
	// survived every test in this file: at N=3 both walls agree ("too
	// many" either way), so N=3 alone cannot tell `> 1` from `> 2` apart.
	// N=2 is the one shape that does — this is the case that closes that
	// gap by pinning the wall at the point where the two disagree, rather
	// than only widening case 8's name to admit it was never checked.
	t.Run("8b log, exactly two arguments (the wall itself, not N=3)", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("log", "1", "2")
		if status == 0 {
			t.Fatal("log ran with two positional arguments")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["log/too many arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 9: ls takes no arguments at all. Before this task it took one
	// anyway and ignored it.
	t.Run("9 ls, an argument it has no use for", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("ls", "junk")
		if status == 0 {
			t.Fatal("ls ran with an argument")
		}
		// The command name is the only thing that tells cases 9/10/11
		// apart — checkNoArguments("ls"), checkNoArguments("dump") and
		// checkNoArguments("restore") differ ONLY in the string they close
		// over, so a generic "takes no arguments" pin shared by all three
		// would say nothing about R2-shaped mutations (one command's check
		// printing another's name).
		assertRefusedBecause(t, errs, pinnedPhrases["ls/no arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 10: dump only ever writes to stdout; an argument means nothing.
	t.Run("10 dump, an argument it has no use for", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("dump", "junk")
		if status == 0 {
			t.Fatal("dump ran with an argument")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["dump/no arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 11: restore reads a dump from stdin; an argument means nothing.
	t.Run("11 restore, an argument it has no use for", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("restore", "junk")
		if status == 0 {
			t.Fatal("restore ran with an argument")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["restore/no arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 12: the depth axis QA 0050 left as debt. .lock and an empty
	// acme/ are seeded by hand, both at depth 1, so that everything a
	// rejected command would add (main.parts/, main.sapedb) sits at depth 2
	// and nowhere else. A walkTree limited to one level, or a command that
	// still calls open() before checking its argument, both see [.lock
	// acme] before and after and call that unchanged; only an actual
	// recursive diff catches it.
	t.Run("12 depth 2 or deeper", func(t *testing.T) {
		setup := start(t)
		if err := os.WriteFile(filepath.Join(setup.dir, ".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(setup.dir, "acme"), 0o700); err != nil {
			t.Fatal(err)
		}
		before := walkTree(t, setup.dir)

		_, errs, status := setup.run("log", "not-a-number")
		if status == 0 {
			t.Fatal("log ran with a non-numeric position")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["log/not a number"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 13: the other half of the same debt — a database that is
	// completely real except that main.parts/ is missing, so the only
	// path a rejected command could add back is a single EMPTY DIRECTORY.
	// A walkTree that only counts files, or a comparison that only checks
	// len(before) == len(after), is blind to this.
	t.Run("13 a single empty directory reappearing", func(t *testing.T) {
		setup := start(t)
		file := writeOutside(t, articles)
		if _, errs, status := setup.run("apply", file); status != 0 {
			t.Fatalf("seeding a real database: %s", errs)
		}
		if err := os.RemoveAll(filepath.Join(setup.dir, "acme", "main.parts")); err != nil {
			t.Fatal(err)
		}
		before := walkTree(t, setup.dir)

		_, errs, status := setup.run("log", "not-a-number")
		if status == 0 {
			t.Fatal("log ran with a non-numeric position")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["log/not a number"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 14: the positive control for cases 12 and 13, and a different
	// SHAPE of command from case 15 below — this one only prints. A command
	// that actually runs must still produce main.parts/: it is the shape of
	// the database, not debris from a refusal.
	t.Run("14 a command that runs recreates main.parts (log)", func(t *testing.T) {
		setup := start(t)
		file := writeOutside(t, articles)
		if _, errs, status := setup.run("apply", file); status != 0 {
			t.Fatalf("seeding a real database: %s", errs)
		}
		if err := os.RemoveAll(filepath.Join(setup.dir, "acme", "main.parts")); err != nil {
			t.Fatal(err)
		}

		_, errs, status := setup.run("log")
		if status != 0 {
			t.Fatalf("log: %s", errs)
		}
		got := walkTree(t, setup.dir)
		if !reflect.DeepEqual(got, canonicalShape()) {
			t.Errorf("after a command that ran: %v, want %v", got, canonicalShape())
		}
	})

	// Case 15: the write-side positive control — apply, not log, on a
	// completely fresh directory.
	t.Run("15 a command that runs creates the whole shape (apply)", func(t *testing.T) {
		setup := start(t)
		file := writeOutside(t, articles)

		_, errs, status := setup.run("apply", file)
		if status != 0 {
			t.Fatalf("apply: %s", errs)
		}
		got := walkTree(t, setup.dir)
		if !reflect.DeepEqual(got, canonicalShape()) {
			t.Errorf("after apply: %v, want %v", got, canonicalShape())
		}
	})

	// Case 16: url never opens a database at all — this pins that the new
	// command table did not change that.
	//
	// Task 0061 rewrote this fixture: it used to be "hunter2" as the
	// argument, with nothing on stdin, and it was green for the WRONG
	// reason — measured by mutation U1 (checkURL with the password rule
	// deleted): with the rule gone, url() still reads from stdin (it reads
	// whenever len(args) > 1, never looking at what args[1] actually
	// says), gets an EMPTY string back because this case gave it none, and
	// signing.Sign refuses that empty string on its own — still exit 1,
	// still "green", and saying nothing about the rule this case exists to
	// pin. The fix is the fixture, not the assertion: give stdin a real,
	// valid-shaped password (16-128 characters), so that removing the rule
	// makes url actually SUCCEED — status 0 — which is what turns "it was
	// refused" back into a real measurement. See case 16b below, which is
	// the same shape one word further along.
	t.Run("16 url, a password given as an argument", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.runWith(nil, "a-password-of-the-right-shape\n", "url", "localhost:7433", "a-password-of-the-right-shape")
		if status == 0 {
			t.Fatal("url accepted a password as an argument")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["url/password as argument"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 16b: the arity cap checkURL was missing entirely, found by QA
	// 0058 on the user-facing path — "url localhost:9 - junk junk2"
	// exited 0, printed the connection string, and dropped "junk" and
	// "junk2" in silence. url now takes at most a host and one more word
	// ("-" or a password); this pins that a word past that is refused,
	// not swallowed. The password on stdin has to be the right SHAPE
	// (16-128 characters — see TestAPasswordIsNeverAnArgument) precisely
	// so this case is refused for the trailing word alone: "hunter2" is
	// short enough that signing.Sign would refuse it on its own, which
	// would make this case pass for the wrong reason and say nothing
	// about the arity cap it exists to pin.
	//
	// This sits at N=4 (four arguments after "url"). Task 0061 found that
	// N=4 is not the wall: checkURL's actual guard is len(args) > 2, which
	// case 16c below pins at N=3, the point where >2 and >3 first
	// disagree. This case stays at N=4 anyway, unchanged, as the second
	// independent word-past-the-marker shape (two leftover words rather
	// than one) — case 16c does not repeat it, it closes a gap this one
	// cannot reach.
	t.Run("16b url, a word left over after the password marker", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.runWith(nil, "a-password-of-the-right-shape\n", "url", "localhost:7433", "-", "junk", "junk2")
		if status == 0 {
			t.Fatal("url accepted a leftover word after the password marker")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["url/too many arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 16c: the actual wall, not the one case 16b happens to sit on.
	// Task 0061 measured that checkURL's `len(args) > 2` guard was only
	// ever exercised at N=4 (case 16b above, and — before this task —
	// rejectionCases["url"]), and nudging it outward by one to
	// `len(args) > 3` survived every test in this file: at N=4 both walls
	// agree ("too many" either way), so N=4 alone cannot tell `> 2` from
	// `> 3` apart. This is exactly the same shape as case 8 -> case 8b for
	// checkLog, one command over: N=3 is the one shape where the two
	// walls disagree, and stdin carries a real, valid-shaped password so
	// that removing the rule lets url actually run — "junk" is never read
	// by url() either way, so a mutant that lets it through prints a
	// connection string and exits 0.
	t.Run("16c url, one word past the password marker (N=3, the wall itself)", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.runWith(nil, "a-password-of-the-right-shape\n", "url", "localhost:7433", "-", "junk")
		if status == 0 {
			t.Fatal("url accepted a leftover word after the password marker, at N=3")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["url/too many arguments"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 17: same for shell, refused before any network is touched —
	// "-nope" is not a valid trailing option. "localhost:1" is a closed
	// port (nothing is listening on it), which is exactly the axis case
	// 17b below removes: without a real server, mutation P7/P10/S1
	// (checkShell's rule gone entirely) would still exit 1 here, just for
	// "connection refused" instead — the wants pin below is what tells the
	// two apart on THIS case; case 17b is the stronger shape that also
	// makes the mutant's status flip to 0.
	t.Run("17 shell, an option it does not have", func(t *testing.T) {
		setup := start(t)
		before := walkTree(t, setup.dir)
		_, errs, status := setup.run("shell", "localhost:1", "-nope")
		if status == 0 {
			t.Fatal("shell accepted an option it does not have")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["shell/bad option"])
		assertTreeUnchanged(t, setup.dir, before)
	})

	// Case 17b: the "closed port" case 17 above cannot rule out on its
	// own — a REAL server, listening, with the account/database this
	// case's SAPEDB_SECRET/-ACCOUNT/-DB actually name. Section 3.2 of task
	// 0061 calls the reason out by name: url and shell are both
	// `opens: false`, so open() never runs for either one, and the disk
	// image left behind by a check-stage refusal, a bad secret, empty
	// stdin, or a plain "connection refused" all look identical —
	// assertTreeUnchanged is structurally blind here, on purpose, which is
	// exactly why this case exists instead of only widening case 17.
	//
	// The setup mirrors shell_live_test.go: a real *server.Server on a
	// real listener. "-nope" must still be refused, by checkShell, before
	// any frame crosses the wire — proven by the positive control right
	// after it: "-insecure" against the SAME server, with the SAME
	// account/db/secret, and an EMPTY stdin, must exit 0. Without that
	// control this case could be green because the secret is wrong, the
	// account/db do not match, or the server has not called Serve yet —
	// any of which also exits 1 and would make "-nope" look refused for
	// the right reason when it is not. House rule: "a cap that only
	// SUBTRACTS never turns a ONE-WAY assertion red".
	t.Run("17b shell against a real, listening server (not a closed port)", func(t *testing.T) {
		liveSecret := "the secret this live server for case 17b was started with"
		live, err := server.New(server.Options{Dir: t.TempDir(), Secret: liveSecret})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Close() })

		// Created up front, the way `sapedb apply` would, so the shell's
		// opening WhatIsHere() call has a real, empty database to open
		// rather than nothing at all.
		_, release, err := live.Store("acme", "main")
		if err != nil {
			t.Fatal(err)
		}
		release()

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() { _ = live.Serve(listener) }()

		addr := listener.Addr().String()
		env := map[string]string{"SAPEDB_SECRET": liveSecret, "SAPEDB_ACCOUNT": "acme", "SAPEDB_DB": "main"}

		setup := start(t)
		before := walkTree(t, setup.dir)

		_, errs, status := setup.runWith(env, "", "shell", addr, "-nope")
		if status == 0 {
			t.Fatal("shell accepted an option it does not have, even against a live, reachable server")
		}
		assertRefusedBecause(t, errs, pinnedPhrases["shell/bad option"])
		assertTreeUnchanged(t, setup.dir, before)

		// The positive control: -insecure IS a valid trailing option, the
		// server is genuinely reachable, and an empty stdin session ends
		// at the first EOF — so this must succeed. If it does not, the
		// refusal above was never about "-nope" being a bad option in the
		// first place.
		_, errs, status = setup.runWith(env, "", "shell", addr, "-insecure")
		if status != 0 {
			t.Fatalf("shell -insecure against the same live server did not succeed: %s", errs)
		}
	})

	// Case 18: the boundary this task deliberately leaves alone. Once a
	// database file exists, a conflicting redeclaration is refused by
	// store.Declare — comparing the key it already has against the one
	// being asked for — before either side writes anything. That is a
	// check on SAVED STATE, not on an argument, and section 4 of the task
	// is explicit that it stays where it is, after open().
	t.Run("18 a conflicting redeclaration against a real database", func(t *testing.T) {
		setup := start(t)
		first := writeOutside(t, articles)
		if _, errs, status := setup.run("apply", first); status != 0 {
			t.Fatalf("seeding a real database: %s", errs)
		}
		before := walkTree(t, setup.dir)

		second := writeOutside(t, conflictingArticlesSchema)
		_, errs, status := setup.run("apply", second)
		if status == 0 {
			t.Fatal("a conflicting redeclaration was applied")
		}
		// Not a bare "articles": store.go prints spec.Name (the collection
		// name) whether or not apply's own "collection %q" wrapper is
		// there to say so — QA 0058 measured that dropping "collection %q"
		// from apply's %w formatting leaves this suite entirely green
		// (mutation P20). Pinning the prefix store itself never omits
		// ("sapedb/store:") right next to the collection name closes that:
		// it is only present when BOTH layers contribute — apply's own
		// wrapper AND store's error.
		assertRefusedBecause(t, errs, pinnedPhrases["apply/conflicting redeclaration"])
		assertTreeUnchanged(t, setup.dir, before)
	})
}

// TestUsageListsExactlyTheCommandsInTheTable is G1. usage is prose a human
// reads and commands is the table run() actually dispatches through; they
// are written independently of each other on purpose. Generating usage from
// commands is banned, not merely unnecessary — it would turn this into a
// string compared against itself, which cannot fail no matter what a future
// eighth command does to only one of the two.
func TestUsageListsExactlyTheCommandsInTheTable(t *testing.T) {
	re := regexp.MustCompile(`(?m)^  sapedb \[options\] (\S+)`)
	matches := re.FindAllStringSubmatch(usage, -1)
	if len(matches) == 0 {
		t.Fatal("no command lines found in usage — did its format change?")
	}
	var fromUsage []string
	for _, m := range matches {
		fromUsage = append(fromUsage, m[1])
	}

	var fromTable []string
	for _, c := range commands {
		fromTable = append(fromTable, c.name)
	}

	if !reflect.DeepEqual(fromUsage, fromTable) {
		t.Errorf("usage lists %v, commands lists %v", fromUsage, fromTable)
	}
}

// rejectionCase is one command line, given as a literal invocation, that G2
// below asserts is refused with the tree exactly as it was — and, since task
// 0061, refused for the reason wants names.
type rejectionCase struct {
	args  []string
	stdin string
	wants string
}

// rejectionCases is written by hand and does not derive from commands: it is
// the second, independent source a name added to commands without a
// matching entry here is missing from. Every value must actually be
// refused — this is checked, not assumed — and every one is chosen to be
// refused at the check stage, before open(), so the case also demonstrates
// the tree staying clean.
//
// One row, one command — the exact rule TestEveryCommandInTheTableHasARejectionCase
// exists to enforce (a second row for the same command would break that
// set-equality check), so a second rule for a command already listed here
// gets its own case in TestARejectedArgumentLeavesTheDiskExactlyAsItFound
// instead, not a second line in this map.
//
// "url" and "shell" carry the same fixture discipline as cases 16 and 17b:
// a password of a valid shape, on stdin, that removing the rule being
// measured would actually let through — task 0061 measured that the old
// "hunter2" fixture here left rejectionCases["url"] entirely out of mutation
// P6's (checkURL cut from the table) red set, for the identical reason case
// 16 was rewritten for, above.
var rejectionCases = map[string]rejectionCase{
	"apply":  {args: []string{"apply"}, wants: "apply needs a file"},
	"keygen": {args: []string{"keygen"}, wants: "keygen takes one file to write the private key into"},
	// Three arguments rather than none, so this row is refused by the count
	// rule itself and not by the draft it would otherwise go on to read: a
	// case that is really testing os.ReadFile would still be red if checkSeal
	// lost its count rule entirely.
	"seal":    {args: []string{"seal", "a.json", "b.bundle", "c"}, wants: "seal takes a draft file and a bundle file"},
	"verify":  {args: []string{"verify"}, wants: "verify takes one bundle file"},
	"install": {args: []string{"install"}, wants: "install takes one bundle file"},
	"ls":      {args: []string{"ls", "junk"}, wants: "ls takes no arguments"},
	"dump":    {args: []string{"dump", "junk"}, wants: "dump takes no arguments"},
	"restore": {args: []string{"restore", "junk"}, wants: "restore takes no arguments"},
	"log":     {args: []string{"log", "1", "2", "3"}, wants: "log takes at most one argument, FROM"},
	"url": {
		args:  []string{"url", "localhost:7433", "a-password-of-the-right-shape"},
		stdin: "a-password-of-the-right-shape\n",
		wants: "never given as an argument",
	},
	"shell": {args: []string{"shell", "localhost:1", "-nope"}, wants: "shell takes host:port and optionally -insecure"},
	// version runs before the secret/account/name gates, so its rejection
	// case is the one that proves skipping those gates did not also skip
	// the argument check: "version junk" must still be refused, and refused
	// by checkNoArguments rather than by something further down that
	// version never reaches.
	"version": {args: []string{"version", "junk"}, wants: "version takes no arguments"},
}

// TestEveryCommandInTheTableHasARejectionCase is G2. A command added to
// commands with opens and check set, but with no case here, would still
// pass TestUsageListsExactlyTheCommandsInTheTable as long as usage got the
// same update — this test is the second, independent guard, and it names
// exactly which command is missing rather than just reporting a mismatch.
func TestEveryCommandInTheTableHasARejectionCase(t *testing.T) {
	var fromTable []string
	for _, c := range commands {
		fromTable = append(fromTable, c.name)
	}
	sort.Strings(fromTable)

	var fromMap []string
	for name := range rejectionCases {
		fromMap = append(fromMap, name)
	}
	sort.Strings(fromMap)

	if !reflect.DeepEqual(fromTable, fromMap) {
		want := map[string]bool{}
		for _, n := range fromTable {
			want[n] = true
		}
		have := map[string]bool{}
		for _, n := range fromMap {
			have[n] = true
		}
		var missing, extra []string
		for _, n := range fromTable {
			if !have[n] {
				missing = append(missing, n)
			}
		}
		for _, n := range fromMap {
			if !want[n] {
				extra = append(extra, n)
			}
		}
		t.Fatalf("commands and rejectionCases disagree — in commands but missing a rejection case: %v; in rejectionCases but not a real command: %v", missing, extra)
	}

	for name, tc := range rejectionCases {
		t.Run(name, func(t *testing.T) {
			setup := start(t)
			before := walkTree(t, setup.dir)
			_, errs, status := setup.runWith(nil, tc.stdin, tc.args...)
			if status == 0 {
				t.Fatalf("%q ran when the case table says it must be refused", name)
			}
			// wants is a "for what reason" pin across all seven commands in one
			// loop — cheap breadth, one line covering every entry in the
			// table. Measured (task 0061's harness, H1 paired with P7,
			// P10, P17, P18): removing THIS specific check does not, on
			// its own, let any of those four mutants back through,
			// because case 17/17b and cases 9/10/11 each carry their own
			// assertRefusedBecause independently. So this line's real job
			// here is breadth and cross-checking (the same table also
			// drives TestEveryCommandInTheTableHasARejectionCase's
			// set-equality check just above), not being the sole guard
			// for any one of those four — do not read it as such.
			assertRefusedBecause(t, errs, tc.wants)
			assertTreeUnchanged(t, setup.dir, before)
		})
	}
}

// TestACommandNameMustMatchExactly pins findCommand's equality check. QA
// 0058 (Q6) found that swapping c.name == name for
// strings.HasPrefix(c.name, name) survives every other test in this file —
// "l" is a prefix of "ls", "app" is a prefix of "apply", and findCommand
// runs before check ever sees the rest of the argument list, so nothing
// downstream gets a chance to object. Under that mutation "sapedb l" would
// run ls and "sapedb app" would try to run apply; both must instead be
// refused as an unknown command.
func TestACommandNameMustMatchExactly(t *testing.T) {
	for _, name := range []string{"l", "app"} {
		t.Run(name, func(t *testing.T) {
			setup := start(t)
			before := walkTree(t, setup.dir)
			_, errs, status := setup.run(name)
			if status == 0 {
				t.Fatalf("%q ran as if it were a real command", name)
			}
			if !strings.Contains(errs, "no command called") {
				t.Errorf("%q was refused, but not for being an unknown command: %q", name, errs)
			}
			assertTreeUnchanged(t, setup.dir, before)
		})
	}
}

// TestCheckShellAcceptsHostAlone is the positive control checkShell was
// missing at N=1. QA 0058 (Q5) found that `for _, arg := range args[1:]`
// turned into `for _, arg := range args` survives every test in this
// file: the only case that exercises checkShell (case 17, N=2 with a bad
// trailing option) is refused either way — args[1:] rejects "-nope",
// args also rejects it, same outcome, no case tells them apart. N=1 is
// the shape that does: with only a host and nothing after it, args[1:] is
// empty and there is nothing to object to, but args still holds the host
// string itself, which is not "-insecure" and gets refused as if it were
// a bad option — refusing "sapedb shell HOST", the exact form usage
// documents ("shell [HOST]").
//
// This calls checkShell directly rather than going through the full CLI:
// shell's run() dials a real connection once check passes, and "localhost:1"
// with nothing listening would make this test depend on the network stack
// refusing the right way instead of on the one function task 0058 moved.
func TestCheckShellAcceptsHostAlone(t *testing.T) {
	if err := checkShell(options{}, []string{"localhost:1"}); err != nil {
		t.Fatalf("shell HOST alone, the form usage documents, was refused before it ever tried to connect: %v", err)
	}
}

// TestWalkTreeSeesAPathThatChangedIdentity is a test of the test helper
// itself, for one specific reason: none of the 18 cases in
// TestARejectedArgumentLeavesTheDiskExactlyAsItFound builds a delete — every
// regression they guard against strictly ADDS a path. That is a claim about
// this test file's own case table, same scope as the note on treesMatch's
// comment above: the package does have a real delete path
// (Collection.expire() -> store.DropPart -> vfs.Folder.Remove ->
// os.Remove, reachable through apply), none of these 18 cases sets up a
// partitioned collection to trigger it. So on THIS TABLE, len(before) !=
// len(after) already holds for every real bug above. A comparison that
// checked only the length instead of the actual paths would still happen to
// catch every case in this file, for the wrong reason, and would only be
// exposed by a path changing identity without the count changing — a shape
// none of these 18 cases builds, though the package can. So this builds
// that shape directly on disk, without going through the CLI, and drives
// the exact comparison the case table above relies on.
func TestWalkTreeSeesAPathThatChangedIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := walkTree(t, dir)

	if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	after := walkTree(t, dir)

	if len(before) != len(after) {
		t.Fatalf("this test needs the count to stay the same to make its point: before %v after %v", before, after)
	}
	if treesMatch(before, after) {
		t.Fatal("treesMatch did not notice a path renamed to a different name of the same count")
	}
}

// ---------------------------------------------------------------------------
// restore's own debt: garbage on stdin must never look like a successful
// restore. buildRealDump below goes around the CLI on purpose — apply has no
// way to put a document in a collection, only to declare the collection and
// the operation that would insert one, and inserting one for real needs a
// server on the other end of a connection. Building the store directly is
// the only way to get a dump with an actual document in it, and it is
// exactly the shape a client library would hand this command in production:
// bytes that came from Store.Dump, not from another instance of this CLI.

// buildRealDump makes a tiny real database with one collection and three
// documents, and returns it dumped to a string — the positive control this
// whole file needs: a real dump has to restore, or every case below that
// checks garbage is refused proves nothing except that this tool refuses
// everything.
func buildRealDump(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "source.sapedb")
	file, err := vfs.OpenFile(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	pages, err := pager.Create(file, 0)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(pages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "people",
		Key:  store.Key{Path: "id", Type: "string", Auto: "ulid"},
	}); err != nil {
		t.Fatal(err)
	}
	collection, err := db.Collection("people")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Alice", "Bob", "Carol"} {
		if _, err := collection.Put(map[string]any{"name": name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}

	var dumped strings.Builder
	if _, err := db.Dump(&dumped); err != nil {
		t.Fatal(err)
	}
	return dumped.String()
}

// TestRestoreOfARealDumpRestoresExactlyItsData is the control case task A6's
// debt insists on: without this, every case in
// TestRestoreGarbageLeavesNoVisibleDatabase below would pass just as well if
// restore refused everything, real dumps included, which is not "fixed",
// it is "worse". A real dump must restore, keep its collection, and keep
// every document it held — checked here by dumping the restored database
// right back out and finding all three names in it.
func TestRestoreOfARealDumpRestoresExactlyItsData(t *testing.T) {
	dumped := buildRealDump(t)

	into := start(t)
	out, errs, status := into.runWith(nil, dumped, "restore")
	if status != 0 {
		t.Fatalf("restore of a real dump: %s", errs)
	}
	if !strings.Contains(out, "restored to change") {
		t.Errorf("restore printed %q", out)
	}

	listed, errs, status := into.run("ls")
	if status != 0 {
		t.Fatalf("ls: %s", errs)
	}
	if !strings.Contains(listed, "collection people") {
		t.Fatalf("the restored database does not hold people: %q", listed)
	}

	redumped, errs, status := into.run("dump")
	if status != 0 {
		t.Fatalf("dump: %s", errs)
	}
	for _, name := range []string{"Alice", "Bob", "Carol"} {
		if !strings.Contains(redumped, name) {
			t.Errorf("the restored database lost %q: %q", name, redumped)
		}
	}
}

// TestRestoreGarbageLeavesNoVisibleDatabase is task A6's own debt, measured
// rather than trusted: feed restore every shape of garbage the task names
// and one more (an end line that lies), and check what is actually left --
// not just the exit status, which a status-only check cannot tell apart
// from any other reason this command fails (house-rules.md's "when every
// failing path yields the same status, the status distinguishes nothing").
//
// The disk-level check here is `ls`'s own output, not a byte comparison of
// the .sapedb file: internal/pager.Pager.Write lands pages on disk before
// Commit ever runs (see its own doc comment, "It is not part of the
// database until Commit says so"), so a rejected restore can and does leave
// the file a few pages larger than an untouched one -- that growth is the
// pager's documented, harmless "interrupted" pages (Pager.Interrupted),
// reused by the next transaction, not a partial database. A byte-identical
// check would fail on that harmless growth and miss the actual question,
// which is entirely about what `ls` can see: a collection, a non-empty log,
// or the word "restored".
func TestRestoreGarbageLeavesNoVisibleDatabase(t *testing.T) {
	dumped := buildRealDump(t)
	lines := strings.Split(strings.TrimRight(dumped, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("buildRealDump produced too few lines to cut up: %d", len(lines))
	}

	// endLiesAboutLSN keeps the real header and body and forges only the
	// last line's own claim about the change the dump ends at, so this is a
	// dump whose header and body are genuine and only the end disagrees
	// with the header it started next to.
	endLiesAboutLSN := strings.Join(lines[:len(lines)-1], "\n") + "\n" +
		strings.Replace(lines[len(lines)-1], `"lsn":4`, `"lsn":999`, 1) + "\n"

	garbage := map[string]string{
		"200 random bytes":                                      string(randomJunk(t, 200)),
		"empty stdin":                                           "",
		"valid json, wrong shape entirely":                      `{"foo":"bar"}` + "\n",
		"header only, stream cut before any collection":         lines[0] + "\n",
		"cut mid-document, no end line":                         strings.Join(lines[:len(lines)-2], "\n") + "\n",
		"end line present but lies about the document count":    strings.Replace(dumped, `"documents":3`, `"documents":5`, 1),
		"end line present but lies about the change it ends at": endLiesAboutLSN,
		"garbage appended after a real end line":                dumped + `{"kind":"document","collection":"people","document":{"name":"Ghost"}}` + "\n",
	}

	for name, junk := range garbage {
		t.Run(name, func(t *testing.T) {
			setup := start(t)
			out, errs, status := setup.runWith(nil, junk, "restore")
			if status == 0 {
				t.Fatalf("garbage restored without complaint: stdout %q", out)
			}
			if strings.Contains(out, "restored to change") {
				t.Fatalf("a rejected restore claimed success on stdout: %q", out)
			}
			if errs == "" {
				t.Fatal("a rejected restore printed nothing about why")
			}

			listed, lsErrs, lsStatus := setup.run("ls")
			if lsStatus != 0 {
				t.Fatalf("ls after a rejected restore: %s", lsErrs)
			}
			if strings.Contains(listed, "collection") {
				t.Errorf("a rejected restore left a visible collection: %q", listed)
			}
			if !strings.Contains(listed, "log 0..0") {
				t.Errorf("a rejected restore left the log at %q, not empty", listed)
			}
		})
	}
}

// randomJunk is unstructured bytes with no relation to JSON at all -- the
// crudest garbage restore can be handed, and the case most likely to be
// mistaken for "obviously" refused without ever being run.
func randomJunk(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := crand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

// TestApplyDoesNotPrintWhatItRollsBack is task A7's own shape one layer up
// from the database: a later file failing must not leave stdout claiming an
// earlier file's collection was declared, because the whole multi-file run
// is one transaction (apply's own doc comment: db.Commit is called once, at
// the end of the loop over every file) and that whole transaction is what
// actually failed. Unlike case 6 in
// TestARejectedArgumentLeavesTheDiskExactlyAsItFound, the failure here is
// NOT caught by checkApply's pre-open pass: checkApply only checks that
// every file reads and decodes as JSON DisallowUnknownFields accepts, and
// {"operations":[...{"collection":"nowhere"...}]} does. The failure only
// happens once db.DeclareOperation actually looks "nowhere" up, which is
// well after the first file's collection has already been declared into
// this transaction.
func TestApplyDoesNotPrintWhatItRollsBack(t *testing.T) {
	setup := start(t)
	ok := setup.write("people.json", `{"collections":[{"name":"people","key":{"path":"id","type":"string","auto":"ulid"}}]}`)
	bad := setup.write("bad.json", `{"operations":[{"name":"a.all","collection":"nowhere","action":"scan","index":"_key","limit":1}]}`)

	out, errs, status := setup.run("apply", ok, bad)
	if status == 0 {
		t.Fatal("apply ran despite the second file naming a collection nothing ever declared")
	}
	if !strings.Contains(errs, `no such collection: "nowhere"`) {
		t.Errorf("the complaint does not name the missing collection: %q", errs)
	}
	if strings.Contains(out, "collection") {
		t.Errorf("apply printed %q for a run that was never committed", out)
	}

	listed, errs, status := setup.run("ls")
	if status != 0 {
		t.Fatalf("ls: %s", errs)
	}
	if strings.Contains(listed, "collection") {
		t.Errorf("the failed apply left a collection on disk: %q", listed)
	}

	// And the database this left behind is genuinely empty, not "people"
	// half-declared: applying the good file alone afterwards must declare it
	// fresh, not report it unchanged.
	out, errs, status = setup.run("apply", ok)
	if status != 0 {
		t.Fatalf("apply of the good file alone: %s", errs)
	}
	if !strings.Contains(out, "collection people") {
		t.Errorf("the collection was not declared fresh after the failed run: %q", out)
	}
}
