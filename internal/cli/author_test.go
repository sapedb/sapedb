package cli

// The loop an author outside this repository actually walks: make a key,
// seal a bundle, hand the public key to an operator, and have the operator's
// server install it.
//
// Every test here drives the real commands through Run — the same entry point
// a shell reaches — rather than calling keygen(), seal() or bundle.Seal
// directly. A test that called the functions would say nothing about whether
// the commands are wired into the table, whether they survive the
// secret/account/name gates, or whether stdin reaches seal at all, and those
// three are most of what was missing before this task: internal/bundle could
// already sign, and nothing an author could type could reach it.
//
// # Every key here is made at run time
//
// There is not one key literal in this file and none anywhere in this repository.
// Each test that needs an identity runs `sapedb keygen` into a t.TempDir() that
// the test framework deletes, and reads the public half off the command's own
// stdout. That is not only hygiene: a committed key would make these tests
// unable to fail for the thing they are for, because a fixture key proves that
// bundle.Seal works and proves nothing at all about whether the binary can
// produce one.
//
// The files that are NOT keys — the draft, and the deliberate junk in
// TestKeygenWillNotWriteOverAFileThatIsAlreadyThere — are written by the test
// at run time too, into the same temporary directory, and none of them is a
// secret.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hexOnly is the alphabet a key is written in. Written out here as a literal
// rather than taken from internal/bundle's lowerHex or from ed25519's size
// constants on purpose: this is the shape SAPEDB_TRUST's documentation
// promises an operator, and an expectation read out of the code it is
// checking cannot go red when that code changes the promise.
var hexOnly = regexp.MustCompile(`^[0-9a-f]+$`)

// publicKeyHex and privateKeyHex are the two lengths, hand written. 64 and 128
// characters, because an ed25519 public key is 32 bytes and a private key is
// 64, and hex is two characters a byte.
const (
	publicKeyHex  = 64
	privateKeyHex = 128
)

// ordersDraft is what an author writes: the two lists `sapedb apply` already
// reads, with a name, a version and an author's own name in front of them.
//
// A literal, not a marshalled bundle.Bundle. Building this from the struct
// would make the test agree with whatever the struct's json tags happen to
// say, including after somebody renames one — which is exactly the failure an
// author would hit, and exactly the failure this file has to be able to show.
//
// Nothing in it is named "acme" or "main" (the account and database the setup
// harness uses) or "orders-authors" (the operator's label for the key below),
// so an assertion that the install named the bundle cannot be satisfied by
// something else in the output happening to carry the same word.
const ordersDraft = `{
  "name": "orders-pack",
  "version": "3.4.0",
  "signer": "Orders Authors",
  "collections": [
    {
      "name": "orders",
      "key": {"path": "id", "type": "string", "auto": "ulid"},
      "indexes": [
        {"name": "by_customer", "fields": [
          {"path": "customer", "type": "string", "missing": "skip"},
          {"path": "placed", "type": "number", "descending": true, "missing": "last"}
        ]}
      ]
    }
  ],
  "operations": [
    {
      "name": "orders.add", "collection": "orders", "action": "insert",
      "input": [
        {"name": "customer", "type": "string", "required": true},
        {"name": "placed", "type": "number", "required": true}
      ],
      "document": {"customer": {"arg": "customer"}, "placed": {"arg": "placed"}}
    },
    {
      "name": "orders.by_customer", "collection": "orders", "action": "scan",
      "index": "by_customer", "limit": 25, "scopes": ["orders:read"],
      "input": [{"name": "customer", "type": "string", "required": true}],
      "from": {"terms": [{"arg": "customer"}]},
      "to": {"terms": [{"arg": "customer"}]}
    }
  ]
}`

// desk is the author's own directory: their key, their draft, their release.
// Deliberately not SAPEDB_DIR — a bundle written inside the data directory
// would turn up in walkTree and make the "a refused command left the tree
// alone" claims elsewhere in this package unmeasurable.
func desk(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// put writes a file at an absolute path and returns it.
func put(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// makeKey runs the real keygen command and returns the file it wrote and the
// public key it printed, checking the printed value is the 64 lower-case hex
// characters SAPEDB_TRUST documents.
func makeKey(t *testing.T, s *setup, path string) (keyFile, public string) {
	t.Helper()
	out, errs, status := s.run("keygen", path)
	if status != 0 {
		t.Fatalf("keygen exited %d: %s", status, errs)
	}
	public = strings.TrimSuffix(out, "\n")
	if len(public) != publicKeyHex || !hexOnly.MatchString(public) {
		t.Fatalf("keygen printed %q; SAPEDB_TRUST takes %d lower-case hex characters", public, publicKeyHex)
	}
	return path, public
}

// keyOf reads a private key file back, the way a shell redirect would.
func keyOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// wants asserts every phrase is in what a command printed, and says which one
// was missing rather than dumping a diff.
func wants(t *testing.T, what, got string, phrases ...string) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(got, phrase) {
			t.Errorf("%s does not say %q:\n%s", what, phrase, got)
		}
	}
}

// TestAnAuthorCanMakeAKeySealABundleAndHaveItInstalled is the whole loop, in
// the order an author and an operator walk it, through the commands they
// actually type.
//
// The point being measured is that the four commands compose: the key keygen
// wrote is the key seal signs with, the public half keygen printed is the
// value SAPEDB_TRUST takes verbatim, the bundle seal wrote is a file verify
// accepts, and what install put in the database is what ls reads back. Each
// of those is a hand-off between two commands, and a hand-off is the one
// thing unit tests on either side of it never cover.
func TestAnAuthorCanMakeAKeySealABundleAndHaveItInstalled(t *testing.T) {
	setup := start(t)
	home := desk(t)

	// 1. The author makes an identity.
	keyFile, public := makeKey(t, setup, filepath.Join(home, "orders.key"))

	// 2. The author seals their declarations, feeding the key in on stdin
	//    exactly as `sapedb seal draft bundle < orders.key` would.
	draftPath := put(t, filepath.Join(home, "orders.draft.json"), ordersDraft)
	bundlePath := filepath.Join(home, "orders.bundle")

	out, errs, status := setup.runWith(nil, keyOf(t, keyFile), "seal", draftPath, bundlePath)
	if status != 0 {
		t.Fatalf("seal exited %d: %s", status, errs)
	}
	wants(t, "seal", out, "sealed "+bundlePath, "signed by "+public)
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("seal exited 0 and wrote no bundle: %v", err)
	}

	// 3. The operator puts the public key on the trust list, written the way
	//    the usage block says to write it.
	operator := map[string]string{"SAPEDB_TRUST": "orders-authors=" + public}

	// 4. The operator checks the file before committing to anything.
	out, errs, status = setup.runWith(operator, "", "verify", bundlePath)
	if status != 0 {
		t.Fatalf("verify exited %d: %s", status, errs)
	}
	wants(t, "the verify report", out,
		`bundle "orders-pack" version "3.4.0"`,
		"read      yes",
		"signed    yes",
		"intact    yes",
		"trusted   yes",
		`says it is "Orders Authors"`,
		`known as  "orders-authors"`,
		"collection orders (key id string, ulid)",
		"index by_customer [customer string missing:skip, placed number desc missing:last]",
		"operation orders.add insert orders",
		"operation orders.by_customer scan orders via by_customer limit 25 scopes orders:read",
	)

	// 5. The operator installs it. What it declares is said out loud.
	out, errs, status = setup.runWith(operator, "", "install", bundlePath)
	if status != 0 {
		t.Fatalf("install exited %d: %s", status, errs)
	}
	wants(t, "install", out,
		`installing bundle "orders-pack" version "3.4.0", signed by `+public+`, trusted here as "orders-authors"`,
		"\ncollection orders\n",
		"\noperation orders.add version 1\n",
		"\noperation orders.by_customer version 1\n",
	)

	// 6. Reading it back shows the operations, with the versions this store
	//    handed out — which the bundle could not have carried, since a bundle
	//    claiming one is refused.
	out, errs, status = setup.run("ls")
	if status != 0 {
		t.Fatalf("ls exited %d: %s", status, errs)
	}
	wants(t, "ls", out,
		"collection orders (key id string, ulid)",
		"index by_customer [customer string missing:skip, placed number desc missing:last]",
		"operation orders.add v1 insert orders",
		"operation orders.by_customer v1 scan orders via by_customer limit 25 scopes orders:read",
	)

	// 7. And the change log records the bundle and the key, not the account
	//    that happened to run the command.
	out, errs, status = setup.run("log")
	if status != 0 {
		t.Fatalf("log exited %d: %s", status, errs)
	}
	// The log is JSON, so the quotes the actor string carries arrive escaped;
	// the phrases below are written the way they appear on the line, not the
	// way installedBy composes them.
	wants(t, "the change log", out,
		`bundle \"orders-pack\" \"3.4.0\" signed by `+public+`, trusted here as \"orders-authors\"`,
	)
	if strings.Contains(out, `"actor":"acme`) {
		t.Errorf("the change log recorded the account that ran the command instead of the bundle:\n%s", out)
	}
}

// TestKeygenPutsThePrivateKeyInAFileAndNeverOnStdout is the claim that makes
// keygen safe to run in a terminal and in CI.
//
// Both halves are asserted, not just the absence: a command that printed
// nothing at all would satisfy "the private key is not on stdout" and be
// useless, so the positive assertions — the file really holds a private key,
// stdout really holds the public one, and one is the public half of the other
// — carry the claim, and the two Contains checks are the ceiling on top.
func TestKeygenPutsThePrivateKeyInAFileAndNeverOnStdout(t *testing.T) {
	setup := start(t)
	path := filepath.Join(desk(t), "acme.key")

	out, errs, status := setup.run("keygen", path)
	if status != 0 {
		t.Fatalf("keygen exited %d: %s", status, errs)
	}

	private := strings.TrimSpace(keyOf(t, path))
	if len(private) != privateKeyHex || !hexOnly.MatchString(private) {
		t.Fatalf("the key file holds %d characters, want %d lower-case hex", len(private), privateKeyHex)
	}

	public := strings.TrimSuffix(out, "\n")
	if len(public) != publicKeyHex || !hexOnly.MatchString(public) {
		t.Fatalf("keygen printed %q, want %d lower-case hex characters", public, publicKeyHex)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("keygen printed more than the one line SAPEDB_TRUST takes:\n%q", out)
	}

	// The pairing, by crypto/ed25519's documented layout: a private key is the
	// 32-byte seed with the public key appended, so the printed public half
	// must be the tail of the stored private one. The end-to-end test above
	// proves the same pairing behaviourally, by signing with one and
	// verifying against the other; this is the cheap local version of it.
	if !strings.HasSuffix(private, public) {
		t.Errorf("the key printed is not the public half of the key stored")
	}

	// The ceiling. The seed is the half that is genuinely secret — the tail is
	// the public key and appears on stdout legitimately — so it is checked
	// separately, and on both streams.
	seed := private[:privateKeyHex/2]
	if strings.Contains(out, seed) {
		t.Errorf("the private key reached stdout, where it can be scrolled back and logged")
	}
	if strings.Contains(errs, seed) {
		t.Errorf("the private key reached stderr, where it can be scrolled back and logged")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file is mode %04o; anything but 0600 is an identity another account on this host holds", info.Mode().Perm())
	}
}

// TestKeygenWillNotWriteOverAFileThatIsAlreadyThere.
//
// The file put in the way is deliberately not a key — an obvious throwaway
// sentence, written by this test at run time — so that what is measured is
// "keygen refuses a path that is taken", not "keygen recognises a key". A
// version that only refused real keys would still destroy the draft, the
// bundle, or anything else an author typed the wrong path for.
func TestKeygenWillNotWriteOverAFileThatIsAlreadyThere(t *testing.T) {
	setup := start(t)
	path := filepath.Join(desk(t), "orders.key")
	const already = "this file is not a key, and keygen must leave it exactly as it is\n"
	put(t, path, already)

	out, errs, status := setup.run("keygen", path)
	if status == 0 {
		t.Fatalf("keygen wrote over a file that was already there")
	}
	wants(t, "the refusal", errs, "there is already a file at "+path)

	// The positive half: refused is not enough, the file has to still be
	// there and still be what it was.
	if back := keyOf(t, path); back != already {
		t.Errorf("the file that was in the way is now %q", back)
	}
	if out != "" {
		t.Errorf("a refused keygen printed something: %q", out)
	}
}

// TestTwoKeygenRunsMakeTwoDifferentIdentities. A constant key — a fixed seed,
// a stubbed reader, a forgotten test double left in — is the one keygen bug
// that leaves every other test in this file green: every bundle would still
// seal, verify and install, and every author in the world would share one
// identity.
func TestTwoKeygenRunsMakeTwoDifferentIdentities(t *testing.T) {
	setup := start(t)
	home := desk(t)

	_, first := makeKey(t, setup, filepath.Join(home, "first.key"))
	_, second := makeKey(t, setup, filepath.Join(home, "second.key"))

	if first == second {
		t.Fatalf("two keygen runs produced the same identity: %s", first)
	}
	if keyOf(t, filepath.Join(home, "first.key")) == keyOf(t, filepath.Join(home, "second.key")) {
		t.Fatalf("two keygen runs produced the same private key")
	}
}

// TestABundleSealedWithAKeyThisServerDoesNotTrustIsRefused is the first
// negative half: the signature is perfect and the bundle is still refused,
// because the key behind it is not one this operator put on the list.
//
// Both keys are real and both are made at run time, so the difference between
// them is the only thing that can be producing the refusal.
func TestABundleSealedWithAKeyThisServerDoesNotTrustIsRefused(t *testing.T) {
	setup := start(t)
	home := desk(t)

	ours, _ := makeKey(t, setup, filepath.Join(home, "ours.key"))
	_, theirs := makeKey(t, setup, filepath.Join(home, "theirs.key"))

	draftPath := put(t, filepath.Join(home, "orders.draft.json"), ordersDraft)
	bundlePath := filepath.Join(home, "orders.bundle")
	if _, errs, status := setup.runWith(nil, keyOf(t, ours), "seal", draftPath, bundlePath); status != 0 {
		t.Fatalf("seal exited %d: %s", status, errs)
	}

	// The list names somebody else. It is not empty — an empty list refuses
	// everything with a different error, and this test is about the key, not
	// about an unconfigured server.
	operator := map[string]string{"SAPEDB_TRUST": "somebody-else=" + theirs}

	out, errs, status := setup.runWith(operator, "", "verify", bundlePath)
	if status == 0 {
		t.Fatalf("verify accepted a bundle signed by a key that is not on the list")
	}
	wants(t, "the refusal", errs, "signed by a key no operator of this server trusts")
	wants(t, "the verify report", out,
		"trusted   no",
		// The positive half, and the reason this refusal is about trust and
		// nothing else: the signature itself checked out.
		"signed    yes",
		"intact    yes",
		"nothing below is vouched for",
	)

	if _, errs, status := setup.runWith(operator, "", "install", bundlePath); status == 0 {
		t.Fatalf("install took a bundle verify refused")
	} else {
		wants(t, "the refusal", errs, "signed by a key no operator of this server trusts")
	}

	// And nothing landed. ls is asserted to have actually produced a listing,
	// so "no orders collection" cannot be satisfied by ls printing nothing.
	out, errs, status = setup.run("ls")
	if status != 0 {
		t.Fatalf("ls exited %d: %s", status, errs)
	}
	if !strings.Contains(out, "log ") {
		t.Fatalf("ls printed no listing at all, so it proves nothing about what is missing from it:\n%q", out)
	}
	if strings.Contains(out, "collection orders") || strings.Contains(out, "orders.by_customer") {
		t.Errorf("a refused bundle declared something anyway:\n%s", out)
	}
}

// TestABundleChangedAfterItWasSealedIsRefused is the second negative half.
//
// Two edits, because they sit on opposite sides of the signed message: the
// version is the bundle's own label for itself and the limit is part of a
// declaration. A signature covering only one of the two would pass the other
// case, and both are things somebody rewriting a release in transit would
// reach for — the first to pass a file off as a different release, the second
// to widen what an operation may read.
func TestABundleChangedAfterItWasSealedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, shows string
	}{
		{
			name:  "the version it claims",
			from:  `"version": "3.4.0"`,
			to:    `"version": "3.4.9"`,
			shows: `bundle "orders-pack" version "3.4.9"`,
		},
		{
			name:  "how far an operation may read",
			from:  `"limit": 25`,
			to:    `"limit": 900`,
			shows: "operation orders.by_customer scan orders via by_customer limit 900",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := start(t)
			home := desk(t)

			keyFile, public := makeKey(t, setup, filepath.Join(home, "orders.key"))
			draftPath := put(t, filepath.Join(home, "orders.draft.json"), ordersDraft)
			bundlePath := filepath.Join(home, "orders.bundle")
			if _, errs, status := setup.runWith(nil, keyOf(t, keyFile), "seal", draftPath, bundlePath); status != 0 {
				t.Fatalf("seal exited %d: %s", status, errs)
			}

			sealed := keyOf(t, bundlePath)
			if strings.Count(sealed, tc.from) != 1 {
				t.Fatalf("the sealed bundle holds %d copies of %q, so editing it proves nothing",
					strings.Count(sealed, tc.from), tc.from)
			}
			put(t, bundlePath, strings.Replace(sealed, tc.from, tc.to, 1))

			// The right key, on the list, under a label. Nothing about trust
			// is wrong here: the only thing that changed is the bytes.
			operator := map[string]string{"SAPEDB_TRUST": "orders-authors=" + public}

			out, errs, status := setup.runWith(operator, "", "verify", bundlePath)
			if status == 0 {
				t.Fatalf("verify accepted a bundle that was edited after it was sealed")
			}
			wants(t, "the refusal", errs, "the signature does not match these declarations")
			wants(t, "the verify report", out,
				"intact    no",
				"these are not the declarations that key signed",
				// The positive half: the signature block is still readable
				// and still names the author's key, so the refusal is about
				// the declarations having moved and not about a mangled file.
				"signed    yes",
				"key "+public,
				// And the edit really did land, so the refusal is about this
				// change and not about some accident of the harness.
				tc.shows,
			)

			if _, errs, status := setup.runWith(operator, "", "install", bundlePath); status == 0 {
				t.Fatalf("install took a bundle that was edited after it was sealed")
			} else {
				wants(t, "the refusal", errs, "the signature does not match these declarations")
			}
		})
	}
}

// TestSealRefusesADraftItWouldHaveToLieAbout. Three drafts seal must not
// sign, each for a different reason, and in every case the refusal has to
// come before anything is written — an author whose pipeline stops must not
// be left with a bundle file that is the previous release, or half of one.
func TestSealRefusesADraftItWouldHaveToLieAbout(t *testing.T) {
	for _, tc := range []struct {
		name, draft, because string
	}{
		{
			name:    "a field this version does not read",
			draft:   `{"name": "x", "version": "1", "signer": "y", "shapes": []}`,
			because: `unknown field "shapes"`,
		},
		{
			name:    "some other format",
			draft:   `{"format": "sapedb/bundle:v9", "name": "x", "version": "1", "signer": "y"}`,
			because: `says its format is "sapedb/bundle:v9"`,
		},
		{
			name: "a signature that is already there",
			// Not hand written: this case needs a signature that is real, so
			// that what is refused is "there is a signature here" and not "the
			// signature here is nonsense". It is filled in by the body below.
			because: "already carries a signature",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := start(t)
			home := desk(t)
			keyFile, _ := makeKey(t, setup, filepath.Join(home, "orders.key"))
			key := keyOf(t, keyFile)

			source := tc.draft
			if source == "" {
				first := put(t, filepath.Join(home, "orders.draft.json"), ordersDraft)
				already := filepath.Join(home, "orders.bundle")
				if _, errs, status := setup.runWith(nil, key, "seal", first, already); status != 0 {
					t.Fatalf("seal exited %d: %s", status, errs)
				}
				source = keyOf(t, already)
			}

			draftPath := put(t, filepath.Join(home, "second.draft.json"), source)
			outPath := filepath.Join(home, "second.bundle")

			_, errs, status := setup.runWith(nil, key, "seal", draftPath, outPath)
			if status == 0 {
				t.Fatalf("seal signed a draft it should have refused")
			}
			wants(t, "the refusal", errs, tc.because)

			// The positive half of "refused": nothing was written where the
			// bundle would have gone.
			if _, err := os.Stat(outPath); err == nil {
				t.Errorf("a refused seal left a bundle at %s", outPath)
			}
		})
	}
}
