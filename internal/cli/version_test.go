package cli

import (
	"regexp"
	"strings"
	"testing"
)

// oneVersionLine is the shape `sapedb version` prints: the product name, a
// space, and one word that is the build.
//
// A shape and not a value, deliberately. What this command must print is what
// the linker stamped into this binary, and the test binary running here is
// not stamped — asserting the value would mean comparing the output against
// build.Version, which is the constant that produced it, and such a test
// agrees with itself no matter what either side becomes. The value is pinned
// where it can be pinned honestly: in TestAStampedBuildSaysWhatItWasStampedWith
// at the module root, which builds a real binary with a version written out
// by hand and reads what it says.
var oneVersionLine = regexp.MustCompile(`^sapedb [^\s]+\n$`)

// TestVersionAnswersWithNoSecretAndNoDatabaseNamed is the point of the
// command. Every other command in this tool is about one database and is
// rightly refused without a secret, an account and a name; this one is about
// the binary, and the moment somebody needs it — an unfamiliar container, a
// bug report from a host they do not administer — is exactly the moment they
// have none of the three.
func TestVersionAnswersWithNoSecretAndNoDatabaseNamed(t *testing.T) {
	setup := start(t)
	empty := setup.envOnly(nil)

	// The positive control, and it runs first on purpose: this test's claim
	// is that ONE command gets through an empty environment, which is worth
	// nothing unless the environment really is empty. If `ls` also ran here,
	// the assertion below would be measuring a gate that is not there at all
	// rather than a command that steps around it.
	if _, errs, status := runIn(setup, empty, "ls"); status == 0 {
		t.Fatalf("ls ran with no secret set, so this environment is not the empty one this test needs: %q", errs)
	} else if !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Fatalf("ls was refused, but not for the missing secret: %q", errs)
	}

	before := walkTree(t, setup.dir)

	out, errs, status := runIn(setup, empty, "version")
	if status != 0 {
		t.Fatalf("version exited %d with no secret set: %q", status, errs)
	}
	if errs != "" {
		t.Errorf("version complained about something: %q", errs)
	}
	if !oneVersionLine.MatchString(out) {
		t.Errorf("version printed %q, want one line of the form \"sapedb <build>\"", out)
	}

	// Asking a binary what it is must not create an account directory or take
	// the directory lock. Nothing in version's path calls open(), and this is
	// what says so from the outside.
	assertTreeUnchanged(t, setup.dir, before)
}

// TestVersionStillRefusesAnArgument is the other half of running above the
// gates: stepping around the secret must not have stepped around the
// argument check as well.
//
// rejectionCases carries the same case, which is what ties it to the table;
// this one exists separately because the table's loop runs with a full
// environment, and the interesting version of the question is whether the
// check still happens on the path that skips everything else.
func TestVersionStillRefusesAnArgument(t *testing.T) {
	setup := start(t)

	out, errs, status := runIn(setup, setup.envOnly(nil), "version", "junk")
	if status == 0 {
		t.Fatalf("version ran with an argument it does not take, printing %q", out)
	}
	if !strings.Contains(errs, "version takes no arguments") {
		t.Errorf("the refusal does not say why: %q", errs)
	}
}

// TestUnknownCommandsAreStillRefusedForTheMissingSecret pins the ordering
// that the standalone shortcut had to leave alone. A name that is not a
// command falls through to the gates, so a typo gets the same answer it
// always did rather than a different one depending on whether the caller
// happened to have a secret set.
func TestUnknownCommandsAreStillRefusedForTheMissingSecret(t *testing.T) {
	setup := start(t)

	_, errs, status := runIn(setup, setup.envOnly(nil), "verison")
	if status == 0 {
		t.Fatal("a name that is not a command ran")
	}
	if !strings.Contains(errs, "SAPEDB_SECRET") {
		t.Errorf("a typo with no secret set was refused for %q, not the missing secret", errs)
	}
}

// runIn runs the command with a lookup the caller chose, rather than the
// setup's usual filled-in environment.
func runIn(s *setup, lookup func(string) (string, bool), args ...string) (string, string, int) {
	s.t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	status := Run(args, lookup, strings.NewReader(""), out, errs)
	return out.String(), errs.String(), status
}
