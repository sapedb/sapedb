package cli

import (
	"crypto/ed25519"
	"os"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/bundle"
)

// workedModule is the module the published page works through, read out of
// the repository rather than restated here. These two tests are about a
// command that reads a file and prints what is in it, so the file it reads
// has to be the one an operator will actually be handed.
const workedModule = "../../examples/library/module.json"

func sealedWorkedModule(t *testing.T, private ed25519.PrivateKey) string {
	t.Helper()
	raw, err := os.ReadFile(workedModule)
	if err != nil {
		t.Fatalf("reading %s: %v", workedModule, err)
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		t.Fatalf("%s is not a bundle this version can read: %v", workedModule, err)
	}
	if err := bundle.Seal(&b, private); err != nil {
		t.Fatalf("sealing %s: %v", workedModule, err)
	}
	return writtenAt(t, b)
}

// TestVerifyPrintsTheCostEnvelopeOfEveryOperationWithoutInstallingAnything is
// SAPE-12's fourth criterion at the command line — the half that said
// "readable BEFORE installation" and was not true until this flag existed.
//
// Four things are asserted, and each one is a different way it could be
// hollow.
//
//   - The numbers are the right numbers. Written out here by hand from the
//     declarations in examples/library/module.json: `borrow` reaches three
//     collections through its three steps and nobody wrote that list down,
//     and its ceiling of 3 is the sum of those steps rather than a number
//     that appears anywhere in the file.
//   - Every operation gets one. An envelope block that quietly skipped an
//     operation would make a module read as smaller than it is.
//   - The flag is what produces it. The same bundle without it must print
//     none of this, or "readable before installation" would be a description
//     of output that was already there.
//   - Nothing was installed. verify opens no database, and a command that had
//     quietly started opening one to answer this would be "install it into a
//     scratch database first" wearing a different name.
func TestVerifyPrintsTheCostEnvelopeOfEveryOperationWithoutInstallingAnything(t *testing.T) {
	setup := start(t)
	public, private := author(t, 31)
	path := sealedWorkedModule(t, private)

	before := walkTree(t, setup.dir)
	out, errs, status := setup.runWith(trusting("worked-example", public), "", "verify", "-envelopes", path)
	if status != 0 {
		t.Fatalf("verifying the worked module with -envelopes was refused: %s\n%s", errs, out)
	}

	for _, want := range []string{
		"cost envelopes (read from these declarations, not from any database)",

		// A write: one document touched, and no fields come back out.
		"  library:books.shelve\n",
		"    collections  library_books\n",
		"    rows at most 1\n",
		"    escapes      nothing\n",

		// A scan with a projection: the fourth fact, spelled out.
		"  library:books.by_author\n",
		"    indexes      by_author\n",
		"    rows at most 50\n",
		"    escapes      id, on_loan, shelf, title\n",

		// A count: it may walk 1000 and it hands back a number, not rows.
		"  library:books.on_shelf\n",
		"    indexes      by_shelf\n",
		"    rows at most 1000\n",

		// The batch that is the whole argument for deriving rather than
		// writing: three collections, none of them listed by its author.
		"  library:loans.borrow\n",
		"    collections  library_books, library_loans, library_members\n",
		"    rows at most 3\n",

		// And the one beside it that reaches two of the same three.
		"  library:loans.give_back\n",
		"    collections  library_books, library_loans\n",
		"    rows at most 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verify -envelopes did not print %q:\n%s", want, out)
		}
	}

	for _, name := range []string{
		"library:books.shelve", "library:members.join", "library:books.get",
		"library:books.by_author", "library:books.on_shelf", "library:loans.borrow",
		"library:loans.give_back", "library:loans.of_member", "library:loans.per_member",
	} {
		if !strings.Contains(out, "\n  "+name+"\n") {
			t.Errorf("verify -envelopes printed no envelope for %s:\n%s", name, out)
		}
	}

	plain, _, status := setup.runWith(trusting("worked-example", public), "", "verify", path)
	if status != 0 {
		t.Fatalf("verifying the worked module without the flag was refused:\n%s", plain)
	}
	if strings.Contains(plain, "cost envelopes") || strings.Contains(plain, "rows at most") {
		t.Errorf("verify printed the envelopes without being asked, so -envelopes is not what makes them readable:\n%s", plain)
	}
	// And it still prints what it always printed, so the check just above is
	// not passing because the command stopped working altogether.
	if !strings.Contains(plain, "operation library:loans.borrow batch library_loans") {
		t.Errorf("verify without the flag no longer prints what the bundle declares:\n%s", plain)
	}

	assertTreeUnchanged(t, setup.dir, before)
}

// TestVerifyRefusesAnOptionItDoesNotHave is the door on the small parser
// verifyArgs adds. An unknown option silently ignored is how `-envelope`
// (singular, mistyped) becomes a verify that prints no envelopes and exits 0,
// which on the terminal reads exactly like a bundle that has none.
func TestVerifyRefusesAnOptionItDoesNotHave(t *testing.T) {
	setup := start(t)
	public, private := author(t, 32)
	path := sealedWorkedModule(t, private)

	out, errs, status := setup.runWith(trusting("worked-example", public), "", "verify", "-envelope", path)
	if status == 0 {
		t.Fatalf("verify accepted an option it does not have:\n%s", out)
	}
	assertRefusedBecause(t, errs, `verify has no option called "-envelope"`)

	// Two files is a refusal too, rather than the second one being dropped: a
	// reader who typed two paths is owed an error, not a report about
	// whichever one happened to come first.
	out, errs, status = setup.runWith(trusting("worked-example", public), "", "verify", path, path)
	if status == 0 {
		t.Fatalf("verify accepted two bundle files:\n%s", out)
	}
	assertRefusedBecause(t, errs, "verify is about one bundle file")
}
