package sapedb

// The page and the job, held apart (SAPE-12, criterion 3).
//
// .github/scripts/worked-example.sh follows the sequence on
// docs/external-operations-page.md from cold and asserts the answers. It does
// not read that page. The obvious build — extract the commands out of the
// markdown and run them — cannot fail when the page is wrong: it would run
// whatever nonsense the page contained and report it green. So the job's
// commands are written out by hand, and these tests are what stops the two
// from quietly parting company afterwards.
//
// THIS FILE IS THE THIRD COPY. The command shapes below are written here as
// literals, independently of both the page and the job, and each one has to be
// found on both sides. That is what makes this a check rather than a
// tautology: nothing here is derived from either file, so neither file can
// move the expectation by moving itself.
//
// What happens when they drift, in both directions:
//
//   - The page is edited and the job is not. If the edit changed or removed a
//     command this file names, the literal is no longer on the page and
//     TestTheWorkedExamplePageAndTheCIJobAgree says so, naming the entry and
//     saying the page is the side that lost it. If the edit added a new
//     operation to the documented module,
//     TestEveryOperationOnThePageIsInvokedByTheJob says the job never calls it.
//     If the edit only added prose, nothing fires — correctly: the job still
//     covers everything the page claims.
//
//   - The job is edited and the page is not. The same literal is now missing
//     from the job, and the same test says so, naming the job as the side that
//     lost it. A command dropped from the job cannot quietly reduce what CI
//     proves while the page goes on promising it.
//
//   - Both are edited to agree with each other, and this file is not. Both
//     sides are red, which is the intended outcome: changing what the page
//     tells a stranger to type is a deliberate act, and it costs a deliberate
//     edit here.
//
// The honest limit, stated rather than left to be discovered: this is a check
// at the granularity of the literals below. A change to the page that touches
// nothing in this table and adds no operation is invisible to it. That is why
// the job asserts behaviour — real answers out of a real daemon — rather than
// string-matching the page, and why this file is the smaller of the two
// guards.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	workedExamplePage     = "docs/external-operations-page.md"
	workedExampleJob      = ".github/scripts/worked-example.sh"
	workedExampleWorkflow = ".github/workflows/worked-example.yml"
)

// agreed is the hand-written third copy: what the page tells a stranger to
// type, and what the job therefore has to be doing.
//
// Every literal here has to be found on BOTH sides. It is not a containment
// check in either direction — a containment check that only ever narrows is
// satisfied by an empty set, and neither of these files being empty is
// something this test would then notice. Each entry is a positive assertion
// about both files at once.
var agreed = []struct {
	what    string
	literal string
}{
	// The author's half. Two commands, neither of which touches a database.
	{"keygen names the file the private half goes into", "keygen author.key"},
	{"seal takes the draft and the bundle, and the key on stdin", "seal module.json library.bundle.json"},
	{"seal says what it sealed and who signed it", "sealed library.bundle.json, signed by"},

	// The operator's half.
	{"verify is run on the bundle by name", "verify library.bundle.json"},
	{"trust is an environment variable", "SAPEDB_TRUST"},
	{"the four checks: read", "the signature is over exactly these declarations"},
	{"the four checks: trusted, when it passes", "an operator of this server put that key on the list"},

	// The three refusals the page prints in full.
	{"an empty trust list refuses everything", "this server's trust list is empty; set SAPEDB_TRUST"},
	{"and says so in those words", "no signer is trusted on this server, so no bundle can be"},
	{"a tampered bundle is not intact", "these are not the declarations that key signed"},
	{"and trust is not even reached", "not reached; the signature did not check out"},
	{"a key nobody here trusts", "no operator of this server put that key on the list"},

	// The tamper the page chose: a row ceiling, 50 to 5000. The signature is
	// over the declarations rather than the bytes, which is the whole reason
	// this particular edit is the one worth showing.
	{"the tamper is the row ceiling", `sed 's/"limit": 50/"limit": 5000/' library.bundle.json`},

	// Installing, into a database that does not exist yet.
	{"install names the directory, account and database", "-dir lib -account acme -db main install library.bundle.json"},
	{"the install line names the bundle and the key", `installing bundle "library" version "1.0.0", signed by`},

	// Talking to a running daemon.
	{"the host the page tells a reader to connect to", "127.0.0.1:7455"},
	{"which account and database", "-account acme -db main"},
	{"and without TLS", "-insecure"},
	{"the non-interactive form is what a script can branch on", "sapedb invoke HOST NAME ARG=VALUE"},

	// The worked module itself.
	{"the module the page works through", "examples/library/module.json"},

	// The refusals an invocation can get, quoted verbatim on the page.
	{"a missing required argument is named", `"library:books.get" needs "id", which is a string`},
	{"an undeclared argument is named", `"library:books.get" does not take "shelf"`},
	{"a wrongly typed argument is named", `"joined" is a number, and "yesterday" is not one`},
	{"an operation that is not there is named", `there is no operation called "library:books.burn" here`},
}

// floorOnAgreed is a hand-written minimum on the table above.
//
// The table is the positive assertion this whole file rests on, and a table
// that had been emptied would make every check below pass by having nothing to
// check. Deliberately a little under the real length, so that deleting one
// entry on purpose does not force an edit here, and far enough above zero that
// a gutted table is red.
const floorOnAgreed = 20

// flatten is the whole of law 10 in one function: prose in these two files
// wraps, and a sentence broken over two lines is invisible to a line-based
// search. Every comparison below runs on the flattened text.
//
// TestFlatteningIsWhatMakesTheseSearchesWork is its positive control: it names
// a sentence that is genuinely split across a newline on the page and shows
// that it is found only after this runs.
func flatten(text string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
}

func readFlat(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return flatten(string(raw))
}

// TestTheWorkedExamplePageAndTheCIJobAgree is the drift check.
func TestTheWorkedExamplePageAndTheCIJobAgree(t *testing.T) {
	if len(agreed) < floorOnAgreed {
		t.Fatalf("the agreed table holds %d entries and this test needs at least %d to be worth running",
			len(agreed), floorOnAgreed)
	}

	page := readFlat(t, workedExamplePage)
	job := readFlat(t, workedExampleJob)

	for _, entry := range agreed {
		want := flatten(entry.literal)
		onPage := strings.Contains(page, want)
		inJob := strings.Contains(job, want)

		switch {
		case onPage && inJob:
			// Both sides still say it.
		case !onPage && !inJob:
			t.Errorf("%s: neither %s nor %s holds %q any more — if this was deliberate, this table is the place to say so",
				entry.what, workedExamplePage, workedExampleJob, entry.literal)
		case !onPage:
			t.Errorf("%s: %s no longer holds %q, and %s still does — the page was edited and the job was not",
				entry.what, workedExamplePage, entry.literal, workedExampleJob)
		default:
			t.Errorf("%s: %s no longer holds %q, and %s still does — the job was edited and the page was not",
				entry.what, workedExampleJob, entry.literal, workedExamplePage)
		}
	}
}

// TestTheDriftCheckCanSayNo is the control on the matcher itself.
//
// Every assertion above is "this literal is present". A matcher that had
// become unconditionally true — a flatten that collapsed everything, a
// Contains that was reading the wrong variable — would make all of them pass
// and say nothing. So here is a command nobody has ever written, asserted
// absent from both files: if this goes green while the test above also goes
// green, the search is doing real work in both directions.
func TestTheDriftCheckCanSayNo(t *testing.T) {
	const neverWritten = "sapedb detonate --every-database --yes-really"

	page := readFlat(t, workedExamplePage)
	job := readFlat(t, workedExampleJob)

	if strings.Contains(page, neverWritten) {
		t.Errorf("%s holds %q, which is not a command this tool has", workedExamplePage, neverWritten)
	}
	if strings.Contains(job, neverWritten) {
		t.Errorf("%s holds %q, which is not a command this tool has", workedExampleJob, neverWritten)
	}
}

// TestFlatteningIsWhatMakesTheseSearchesWork is law 10's positive control.
//
// The sentence below is one the whole CI job depends on — it is the page's own
// statement that `sapedb invoke` exits non-zero on a refusal, which is what
// makes the sequence assertable from a shell at all. On the page it is broken
// across a newline in the middle. A line-based grep does not find it. This
// test shows both halves of that: absent from the raw bytes, present once
// whitespace is flattened.
//
// If the page is ever rewrapped so that this sentence lands on one line, the
// first half of this test fails — and that failure is correct, because it
// means the control has stopped controlling anything and needs a sentence that
// still spans a break.
func TestFlatteningIsWhatMakesTheseSearchesWork(t *testing.T) {
	const spansALineBreak = "which exits non-zero when the operation is refused, so a deployment step can branch on it"

	raw, err := os.ReadFile(workedExamplePage)
	if err != nil {
		t.Fatalf("reading %s: %v", workedExamplePage, err)
	}

	if strings.Contains(string(raw), spansALineBreak) {
		t.Errorf("%q is now on one line of %s, so it no longer controls anything — pick a sentence that still spans a break",
			spansALineBreak, workedExamplePage)
	}
	if !strings.Contains(flatten(string(raw)), spansALineBreak) {
		t.Errorf("%q is not on %s even after flattening, so the page no longer says the thing the CI job depends on",
			spansALineBreak, workedExamplePage)
	}
}

// operationNames finds every namespaced operation this module declares,
// wherever it is written.
//
// Used by the sweep below in both directions. It enumerates; it does not
// decide anything. What each name has to be true of is asserted against the
// other file, and the count is floored, so an empty result is red rather than
// vacuously green.
func operationNames(text string) []string {
	found := regexp.MustCompile(`library:[a-z_]+\.[a-z_]+`).FindAllString(text, -1)
	seen := make(map[string]bool, len(found))
	names := make([]string, 0, len(found))
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestEveryOperationOnThePageIsInvokedByTheJob is the sweep the hand-written
// table cannot do: it catches a name nobody thought to add to that table.
//
// Both directions, because each one is a different mistake. An operation the
// page shows and the job never calls is a documented claim nothing measures. An
// operation the job calls and the page never mentions is a job testing
// something the page does not teach — harmless on its own, but it means one of
// the two is out of date and nobody has said which.
//
// The floor is what keeps this from being the empty-set check law 9 warns
// about. The module declares nine operations, and the page also shows one that
// does not exist (library:books.burn, the refusal), so ten names is the number
// both files should be carrying. Written here, not counted from either file.
func TestEveryOperationOnThePageIsInvokedByTheJob(t *testing.T) {
	const namesBothFilesShouldCarry = 10

	onPage := operationNames(readFlat(t, workedExamplePage))
	inJob := operationNames(readFlat(t, workedExampleJob))

	if len(onPage) < namesBothFilesShouldCarry {
		t.Fatalf("%s names %d operations (%v), and the worked module has %d — this sweep has nothing to sweep",
			workedExamplePage, len(onPage), onPage, namesBothFilesShouldCarry)
	}
	if len(inJob) < namesBothFilesShouldCarry {
		t.Fatalf("%s names %d operations (%v), and the worked module has %d — the job stopped exercising the module",
			workedExampleJob, len(inJob), inJob, namesBothFilesShouldCarry)
	}

	job := strings.Join(inJob, " ")
	for _, name := range onPage {
		if !strings.Contains(job, name) {
			t.Errorf("%s shows %s and %s never invokes it: the page makes a claim CI does not measure",
				workedExamplePage, name, workedExampleJob)
		}
	}
	page := strings.Join(onPage, " ")
	for _, name := range inJob {
		if !strings.Contains(page, name) {
			t.Errorf("%s invokes %s and %s never mentions it: one of the two is out of date",
				workedExampleJob, name, workedExamplePage)
		}
	}
}

// TestTheWorkedExampleJobIsActuallyWiredIntoCI closes the hole the three tests
// above leave open: every one of them would pass for a script that no runner
// ever executes.
//
// A job that exists and is never triggered proves exactly as much as no job at
// all, and it is a comfortable thing to end up with — the file is there, the
// tests about it are green, and nothing runs it. So the workflow is checked for
// the three facts that make it a job: it runs the script, on a push, and on a
// pull request.
func TestTheWorkedExampleJobIsActuallyWiredIntoCI(t *testing.T) {
	workflow := readFlat(t, workedExampleWorkflow)

	for _, want := range []struct{ what, literal string }{
		{"it runs the script", workedExampleJob},
		{"on a push", "push:"},
		{"and on a pull request", "pull_request:"},
	} {
		if !strings.Contains(workflow, want.literal) {
			t.Errorf("%s: %s does not hold %q", want.what, workedExampleWorkflow, want.literal)
		}
	}

	if _, err := os.Stat(workedExampleJob); err != nil {
		t.Errorf("%s names %s and there is no such file: %v", workedExampleWorkflow, workedExampleJob, err)
	}
}
