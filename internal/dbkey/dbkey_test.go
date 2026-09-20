package dbkey

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/sapedb/sapedb/internal/dbname"
)

// goldenSecret is the secret every vector below is computed against. Its
// value does not matter — it exists only so every row shares the same
// secret except the one row whose whole point is a different secret.
const goldenSecret = "correct horse battery staple"

// Every hex string in this file was NOT produced by calling dbkey.Key and
// copying its answer — that would only prove dbkey.Key agrees with itself.
// It was produced by a throwaway program (kept outside this repo, in the
// task's scratchpad) that inlined the derivation by hand:
// hkdf.Key(sha256.New, []byte(secret), []byte(account+"/"+db), label, 32),
// with `label` typed into that program as a literal copy of this package's
// Label constant (see dbkey.go),
//
// These vectors moved once, and this is the only thing that moved them: the
// rename changed Label's text from the product's old name to "sapedb", so
// every key derived under it changed too. That is the whole content of the
// change — the hash, the info string, the argument order and the requested
// length are all exactly what they were, which is why the five rows below
// still differ from each other along exactly the same five axes. The vectors
// were regenerated the same independent way they were first produced, not by
// pasting what the suite reported as "got"; the two agreed, which is the
// cross-check worth having and the reason to bother.
//
// What this test can no longer claim, and used to: that dbkey.Key agrees
// with a database file written before the refactor. It does not, on purpose
// — see Label's own comment. What it still claims is the thing that
// matters from here on: that the derivation is this arithmetic and not
// whatever this package's code happens to compute today,
//
// run under the same docker image house-rules.md names for Go:
//
//	docker run --rm -v "$PWD":/src -v "$HOME/go/pkg/mod":/go/pkg/mod -w /src \
//	  golang:1.24-alpine sh -c "go run main.go"
//
// A refactor that changed the derivation — the
// hash, the byte order of the two strings hkdf.Key takes, which one is the
// info and which is the label, the requested length — would still compile
// and would still make internal/cli and internal/server agree with each
// other, because both call this one function. It would not agree with a
// database file that already exists, and this is the only thing in the
// suite that would notice.
func TestKeyMatchesTheGoldenVector(t *testing.T) {
	key, err := Key(goldenSecret, "acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Compare the whole key, never a prefix of it. HKDF's output is a
	// deterministic prefix: the first 16 bytes of a 32-byte derivation are
	// also the first 16 bytes of a 31-byte one, because the expansion blocks
	// do not depend on the requested length. So a comparison narrowed to 16
	// or 32 hex characters still matches after someone changes pager.KeyBytes
	// out from under Key — and a 31-byte key writes, reads and serves end to
	// end without complaint, because internal/cli and internal/server both
	// ask this one function for it and so both get the same wrong key. That
	// mutation is observable nowhere else in the tree; measured, a prefix-only
	// comparison here together with a KeyBytes-1 derivation leaves all 14
	// packages green. This assertion is what stands between that and nothing.
	const want = "2bd666e5f47857529f54ebe0ba5ef22b11c02b34b0fb2c4dd7528edb5bb15a4b"
	if got := hex.EncodeToString(key); got != want {
		t.Errorf("Key(%q, %q, %q) = %s, want %s (the value the derivation independently produces)",
			goldenSecret, "acme", "main", got, want)
	}
}

// TestKeyTable is the universal claim "Key depends on all three arguments,
// and on how they are combined" turned into a table — one row per axis, each
// row changing exactly one thing from the base row, and each row pinned to
// its own golden hex rather than merely asserted "different from the base".
// A table that only checked "not equal to base" would pass for a mutant that
// collapsed every non-base row onto one shared wrong answer, so long as that
// wrong answer differed from base; pinning each row's own hex closes that.
//
// All five hexes came from the same scratch program as the test above, in
// the same run.
func TestKeyTable(t *testing.T) {
	cases := []struct {
		name                string
		secret, account, db string
		want                string
	}{
		{
			name: "base", secret: goldenSecret, account: "acme", db: "main",
			want: "2bd666e5f47857529f54ebe0ba5ef22b11c02b34b0fb2c4dd7528edb5bb15a4b",
		},
		{
			// Axis: secret. Account and db held at the base row's values.
			name: "secret differs", secret: "a different secret entirely", account: "acme", db: "main",
			want: "c0a1273ab29066cb17286cf2755322e8848c6ac983d221d2b67df9774aa4f8a9",
		},
		{
			// Axis: account. Secret and db held at the base row's values.
			name: "account differs", secret: goldenSecret, account: "bravo", db: "main",
			want: "ea0011d52d4b19644ea5428b0866ae5014a1010a26ee2149891448f376503e67",
		},
		{
			// Axis: db. Secret and account held at the base row's values.
			name: "db differs", secret: goldenSecret, account: "acme", db: "other",
			want: "ca09d282401ccdaf55614e03ccb660f3928465f0193aa9285c2d9d422fb103ed",
		},
		{
			// Axis: which of account/db lands in which position. Same two
			// strings as base, swapped — this is the row that would catch a
			// refactor that flipped the two arguments Key hands to the info
			// string, which M2 in the task's mutation catalogue plants.
			name: "account and db swapped", secret: goldenSecret, account: "main", db: "acme",
			want: "97ae203324816b3b37816b174d0c71e6330926428ef465ceb8b0772dcf1713d6",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, err := Key(c.secret, c.account, c.db)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(key); got != c.want {
				t.Errorf("Key(%q, %q, %q) = %s, want %s", c.secret, c.account, c.db, got, c.want)
			}
		})
	}
}

// TestKeyDoesNotDistinguishWhereTheSlashFallsInAccountOrDB used to be the
// finding this file was asked to make and report rather than fix: Key builds
// its info string as account+"/"+name, and that concatenation cannot tell
// ("a/b", "c") apart from ("a", "b/c") — both produce the literal string
// "a/b/c", and (before this test was rewritten) both derived the same key,
// 5c3d27f090a379ed14913c228b177608f14b03b4c5c943ac5ed0354a9eac21c2. The
// comment on that old version said "whether that is reachable is a separate
// question", left for the Result section of whatever task answered it.
//
// Task 0053 answered it: reachable, through the CLI, which read
// SAPEDB_ACCOUNT/SAPEDB_DB with no shape check of its own at all. The fix
// is not in this file — Key now calls dbname.CheckPair before it derives
// anything (see dbkey.go) — so this test now asserts the opposite of what
// it used to: both colliding pairs are refused before the arithmetic that
// used to produce their shared key ever runs. That arithmetic has not
// changed; nothing reaches it with these two pairs any more. The old hex
// above is kept in this comment, not in an assertion, so a future reader
// knows the collision was real and measured, not merely theorized — and so
// nobody "closes the gap" a second time by re-deriving a key that is no
// longer derivable.
func TestKeyDoesNotDistinguishWhereTheSlashFallsInAccountOrDB(t *testing.T) {
	if _, err := Key(goldenSecret, "a/b", "c"); !errors.Is(err, dbname.Err) {
		t.Errorf("Key(%q, %q, %q) = _, %v, want a dbname.Err refusal", goldenSecret, "a/b", "c", err)
	}
	if _, err := Key(goldenSecret, "a", "b/c"); !errors.Is(err, dbname.Err) {
		t.Errorf("Key(%q, %q, %q) = _, %v, want a dbname.Err refusal", goldenSecret, "a", "b/c", err)
	}

	// Control: a valid pair is entirely unaffected — same golden vector as
	// TestKeyMatchesTheGoldenVector pins on its own.
	key, err := Key(goldenSecret, "acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	const want = "2bd666e5f47857529f54ebe0ba5ef22b11c02b34b0fb2c4dd7528edb5bb15a4b"
	if got := hex.EncodeToString(key); got != want {
		t.Errorf("Key(%q, %q, %q) = %s, want %s", goldenSecret, "acme", "main", got, want)
	}
}

// TestLabelIsExactlyThis pins Label's text as a literal, deliberately not by
// comparing the constant to itself.
//
// The golden vectors above already fail if Label changes, so this is not the
// only thing watching it — but they fail by reporting two hex strings, which
// says the derivation moved without saying which part of it did. This says
// it in one line. The two together are the difference between "a key changed"
// and "the label changed", and only one of those is diagnosable at a glance.
//
// The :v1 suffix is pinned along with the rest: it marks the derivation
// scheme, which this rename did not touch.
func TestLabelIsExactlyThis(t *testing.T) {
	const want = "sapedb/server:database:v1"
	if Label != want {
		t.Errorf("Label is %q, want %q", Label, want)
	}
}
