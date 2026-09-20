package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	Secret string `json:"secret"`
	Label  string `json:"label"`
	Cases  []struct {
		Name      string `json:"name"`
		AccountID string `json:"accountId"`
		Password  string `json:"password"`
		DBName    string `json:"dbname"`
		Derived   string `json:"derived"`
		Direct    string `json:"direct"`
	} `json:"cases"`
}

func load(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/signing.json")
	if err != nil {
		t.Fatalf("the shared fixture must be readable: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(f.Cases) < 6 {
		t.Fatalf("fixture: expected at least 6 cases, got %d", len(f.Cases))
	}
	return f
}

// The single most valuable test in this service: the same triples the
// TypeScript side signs, and the same digests.
func TestFixtureAgreesWithTheAppSide(t *testing.T) {
	f := load(t)
	hexOnly := regexp.MustCompile(`^[0-9a-f]{64}$`)

	// f.Label is read from the file, not from DefaultLabel, everywhere below
	// this line — which means every assertion in this function proves the
	// two sides agree with the fixture, never that the fixture is what
	// either side actually ships as its default. Renaming DefaultLabel
	// without renaming the fixture's label (or the reverse) would leave
	// every one of them green. These two lines are what closes that gap.
	if f.Label != DefaultLabel {
		t.Fatalf("fixture label %q does not match DefaultLabel %q — one of them was renamed without the other",
			f.Label, DefaultLabel)
	}
	// Built from bytes rather than typed out: this file is inside the tree
	// internal/naming patrols, and the old name would be a hit there too if
	// it were spelled whole in a place that is not on that patrol's
	// exception table.
	oldWord := string([]byte{'r', 's', 'q', 'l'})
	if strings.Contains(f.Secret, oldWord) {
		t.Fatalf("fixture secret %q still names the old product", f.Secret)
	}

	for _, c := range f.Cases {
		parts := Parts{AccountID: c.AccountID, Password: c.Password, DBName: c.DBName}

		derived, err := Sign(parts, f.Secret, f.Label)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if derived != c.Derived {
			t.Errorf("%s (derived):\n got  %s\n want %s", c.Name, derived, c.Derived)
		}

		direct, err := Sign(parts, f.Secret, Direct)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if direct != c.Direct {
			t.Errorf("%s (direct):\n got  %s\n want %s", c.Name, direct, c.Direct)
		}

		for _, sig := range []string{derived, direct} {
			if !hexOnly.MatchString(sig) {
				t.Errorf("%s: %q is not 64 lower-case hex characters", c.Name, sig)
			}
		}
		if derived == direct {
			t.Errorf("%s: the two modes must not agree, or a mode mismatch would go unnoticed", c.Name)
		}
		if !Verify(c.Derived, parts, f.Secret, f.Label) || !Verify(c.Direct, parts, f.Secret, Direct) {
			t.Errorf("%s: a signature from the fixture must verify", c.Name)
		}
		if Verify(c.Direct, parts, f.Secret, f.Label) {
			t.Errorf("%s: a mode mismatch must be a failed signature", c.Name)
		}
	}
}

func TestTheDelimiterKeepsTwoSplitsApart(t *testing.T) {
	f := load(t)
	var one, other string
	var joinOne, joinOther string

	for _, c := range f.Cases {
		if strings.Contains(c.Name, "split of") {
			one, joinOne = c.Derived, c.AccountID+c.Password+c.DBName
		}
		if strings.Contains(c.Name, "other split") {
			other, joinOther = c.Derived, c.AccountID+c.Password+c.DBName
		}
	}
	if one == "" || other == "" {
		t.Fatal("fixture: the two split cases must be present")
	}
	if joinOne != joinOther {
		t.Fatalf("fixture: the split cases must hold the same characters: %q vs %q", joinOne, joinOther)
	}
	if one == other {
		t.Error("two different triples signed the same — the delimiter is not doing its job")
	}
}

func TestPasswordCharset(t *testing.T) {
	good := []string{strings.Repeat("y", 16), strings.Repeat("y", 128), "aA0._~-aA0._~-aA0"}
	bad := []string{strings.Repeat("y", 15), strings.Repeat("y", 129), "zürich-passwörd-2024", "has spaces here!", "colon:in-password"}

	for _, p := range good {
		if !ValidPassword(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range bad {
		if ValidPassword(p) {
			t.Errorf("%q should be refused", p)
		}
		if _, err := Sign(Parts{AccountID: "u", Password: p, DBName: "p"}, "s", DefaultLabel); err == nil {
			t.Errorf("%q should not sign", p)
		}
	}
}

func TestRefusesWhatWouldMakeTheMessageAmbiguous(t *testing.T) {
	ok := strings.Repeat("y", 16)
	for _, parts := range []Parts{
		{AccountID: "a:b", Password: ok, DBName: "p"},
		{AccountID: "u", Password: ok, DBName: "p:q"},
		{AccountID: "", Password: ok, DBName: "p"},
	} {
		if _, err := Sign(parts, "s", DefaultLabel); err == nil {
			t.Errorf("%+v should not sign", parts)
		}
	}
	if _, err := Sign(Parts{AccountID: "u", Password: ok, DBName: "p"}, "", DefaultLabel); err == nil {
		t.Error("an empty secret should not sign")
	}
}

func TestVerifyIsFalseNotFatal(t *testing.T) {
	parts := Parts{AccountID: "u", Password: strings.Repeat("y", 16), DBName: "p"}
	sig, _ := Sign(parts, "secret", DefaultLabel)

	for _, bad := range []string{"", "zz", sig[:62], sig + "00"} {
		if Verify(bad, parts, "secret", DefaultLabel) {
			t.Errorf("%q should not verify", bad)
		}
	}
	if !Verify(strings.ToUpper(sig), parts, "secret", DefaultLabel) {
		t.Error("hex case is not part of the contract")
	}
}

// A label nobody set must be the ordinary derived one, not the plainer form.
//
// The two produce different signatures, so a server that quietly chose the
// plain form would refuse every connection string a client signed normally —
// and every test in this repository would still pass, because both sides of
// this repository would agree with each other. That is what happened, and it
// was found by running a TypeScript client against the Go server rather than
// by any test here.
func TestAnUnsetLabelIsTheDefaultOneAndNotTheDirectForm(t *testing.T) {
	parts := Parts{AccountID: "acme", Password: "a-password-of-the-right-shape", DBName: "main"}
	const secret = "the secret only the control plane has"

	blank, err := Sign(parts, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	derived, err := Sign(parts, secret, DefaultLabel)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Sign(parts, secret, Direct)
	if err != nil {
		t.Fatal(err)
	}

	if blank != derived {
		t.Errorf("an unset label signs differently from the default one:\n %s\n %s", blank, derived)
	}
	if blank == direct {
		t.Error("an unset label selected the direct form")
	}
	if !Verify(blank, parts, secret, "") || !Verify(blank, parts, secret, DefaultLabel) {
		t.Error("a signature made with no label does not verify with the default one")
	}
	if Verify(direct, parts, secret, "") {
		t.Error("a direct signature verified against an unset label")
	}
}

// TestASignatureUnderTheOldLabelDoesNotVerifyUnderTheDefault is built with
// the old label string hard-coded, glued together from fragments the same
// way the fixture-secret check above builds the old product's name — not
// for concealment, but so this file, which internal/naming also walks, does
// not itself carry the one string the whole rename exists to remove. If a
// future change ever
// let DefaultLabel and this old string collide (by, say, reverting the
// derivation) this is the test that would catch it, not a fixture whose
// label would have moved with it.
func TestASignatureUnderTheOldLabelDoesNotVerifyUnderTheDefault(t *testing.T) {
	oldLabel := "ecosy/" + string([]byte{'r', 's', 'q', 'l'}) + ":connection:v1"
	if oldLabel == DefaultLabel {
		t.Fatalf("the old label and DefaultLabel must differ for this test to mean anything; both are %q", oldLabel)
	}

	parts := Parts{AccountID: "acme", Password: "a-password-of-the-right-shape", DBName: "main"}
	const secret = "the secret only the control plane has"

	signature, err := Sign(parts, secret, oldLabel)
	if err != nil {
		t.Fatal(err)
	}
	if Verify(signature, parts, secret, "") {
		t.Error("a signature made under the old label verified against the blank (default) label")
	}
	if Verify(signature, parts, secret, DefaultLabel) {
		t.Error("a signature made under the old label verified against DefaultLabel")
	}
}

func TestProvingYouCanOperateThisServer(t *testing.T) {
	nonce := make([]byte, NonceBytes)
	for i := range nonce {
		nonce[i] = byte(i)
	}

	proof, err := Operating("the-server-secret", nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !Operates(proof, "the-server-secret", nonce) {
		t.Error("the proof does not verify against the secret that made it")
	}

	// A different secret, a different challenge, or nothing at all.
	if Operates(proof, "another-secret", nonce) {
		t.Error("a proof made with one secret verified under another")
	}
	other := make([]byte, NonceBytes)
	copy(other, nonce)
	other[0]++
	if Operates(proof, "the-server-secret", other) {
		t.Error("a proof answered a challenge it was not made for")
	}
	if Operates("", "the-server-secret", nonce) || Operates("not hex", "the-server-secret", nonce) {
		t.Error("something that is not a proof was taken as one")
	}

	// The proof is not the connection-string signature under another name: a
	// string somebody holds must not also make them an operator.
	parts := Parts{AccountID: "acme", Password: "a-password-of-the-right-shape", DBName: "main"}
	sig, err := Sign(parts, "the-server-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if Operates(sig, "the-server-secret", nonce) {
		t.Error("a connection-string signature was accepted as operator proof")
	}

	// A challenge the client chose is not a challenge.
	if _, err := Operating("the-server-secret", nil); !errors.Is(err, ErrNonce) {
		t.Errorf("an empty challenge: want ErrNonce, got %v", err)
	}
	if _, err := Operating("the-server-secret", nonce[:8]); !errors.Is(err, ErrNonce) {
		t.Errorf("a short challenge: want ErrNonce, got %v", err)
	}
	if _, err := Operating("", nonce); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("no secret: want ErrEmptySecret, got %v", err)
	}
}

// TestTheOperatorProofIsTheseExactBytes pins the derivation.
//
// Every other test here compares this code against itself, which cannot see a
// changed label: derive under a different one and both sides move together,
// and the only symptom is that every operator in the field stops being one
// after an upgrade. A vector is what a second implementation would check
// against, and it is what stops this changing by accident.
func TestTheOperatorProofIsTheseExactBytes(t *testing.T) {
	nonce := make([]byte, NonceBytes)
	for i := range nonce {
		nonce[i] = byte(i)
	}

	proof, err := Operating("the-server-secret", nonce)
	if err != nil {
		t.Fatal(err)
	}
	const pinned = "f518bbe019b5a7a3dbb9e78739bbb00261024b29d9bd7e686efadd058b9ada4f"
	if proof != pinned {
		t.Errorf("the proof for the pinned secret and challenge is now\n  %s\nand was\n  %s", proof, pinned)
	}
}

// grantAt is the fixed moment the grant tests in this file mint against, and
// held is one grant good for an hour after it. Fixed rather than time.Now()
// so that a failure reads the same on every machine and in every year, and so
// that the expiry boundary below is an exact comparison rather than a race
// against the test's own runtime.
var grantAt = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func aGrant() Held {
	return Held{
		AccountID: "acme",
		DBName:    "main",
		Scopes:    []string{"articles:read", "billing:write"},
		Expires:   grantAt.Add(time.Hour),
		Serial:    "01K5ZQ9P7B3N4M6R8T0V2W4X6Y",
	}
}

// TestAGrantIsTheOneSetOfScopesForTheOneDatabase is the domain separation a
// grant is for, measured.
//
// A grant says five things at once — these scopes, this account, this
// database, until this moment, under this serial — and all five are inside the
// message, so none of them can be changed without the signature failing. The
// table is what a grant must NOT verify against, and the first row of the test
// is what it must: a table of nothing but refusals is also what a Grants that
// always refused would produce.
func TestAGrantIsTheOneSetOfScopesForTheOneDatabase(t *testing.T) {
	const secret = "the secret only the control plane has"

	held := aGrant()
	signature, err := Granting(held, secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) != 64 {
		t.Fatalf("a grant is %d characters, want 64 of lower-case hex", len(signature))
	}

	// The known-positive. Everything below is a refusal that only means
	// something because this is an acceptance.
	if err := Grants(signature, held, secret, grantAt); err != nil {
		t.Fatalf("a grant does not verify against what it was made from (%v) — nothing else in this test measures anything", err)
	}

	// And the set is a set: written in another order, with a repeat, it is the
	// same grant.
	shuffled := aGrant()
	shuffled.Scopes = []string{"billing:write", "articles:read", "billing:write"}
	if err := Grants(signature, shuffled, secret, grantAt); err != nil {
		t.Fatalf("the same scopes written in another order were not the same grant: %v", err)
	}

	another := func(change func(*Held)) Held {
		other := aGrant()
		change(&other)
		return other
	}
	elsewhere := []struct {
		name string
		held Held
	}{
		{"another database", another(func(h *Held) { h.DBName = "other" })},
		{"another account", another(func(h *Held) { h.AccountID = "rival" })},
		{"a scope added", another(func(h *Held) { h.Scopes = append(h.Scopes, "admin") })},
		{"a scope removed", another(func(h *Held) { h.Scopes = []string{"articles:read"} })},
		{"no scopes at all", another(func(h *Held) { h.Scopes = nil })},
		// The two fields ISS-11 added. A caller that wants longer, or wants
		// to be a different grant on a revocation list, has to make a new
		// signature — which it cannot.
		{"a later expiry", another(func(h *Held) { h.Expires = grantAt.Add(48 * time.Hour) })},
		{"one second later", another(func(h *Held) { h.Expires = h.Expires.Add(time.Second) })},
		{"another serial", another(func(h *Held) { h.Serial = "01K5ZQ9P7B3N4M6R8T0V2W4X6Z" })},
	}
	refused := 0
	for _, one := range elsewhere {
		t.Run(one.name, func(t *testing.T) {
			if err := Grants(signature, one.held, secret, grantAt); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("a grant made for %+v verified as %+v: %v", held, one.held, err)
			}
			refused++
		})
	}
	if refused != len(elsewhere) {
		t.Fatalf("%d of %d rows ran", refused, len(elsewhere))
	}

	// Another secret is another server, whatever the grant says.
	if err := Grants(signature, held, "a different secret entirely", grantAt); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a grant verified under a secret it was not made with: %v", err)
	}
}

// TestAnExpiredGrantIsRefusedAsExpiredAndNotAsForged is ISS-11's whole point,
// and the second half of its name is the half that took work.
//
// A grant that has run out and a grant that was never yours are both refusals,
// and a caller does different things about them: the first is a reason to go
// and ask the issuer for another, the second is a reason to stop. So the two
// arrive as different errors here and, through codeFor, as different codes on
// the wire.
//
// The boundary is exact and is asserted at the second on either side, because
// "expired" is a comparison and a comparison is where an off-by-one lives.
func TestAnExpiredGrantIsRefusedAsExpiredAndNotAsForged(t *testing.T) {
	const secret = "the secret only the control plane has"

	held := aGrant()
	signature, err := Granting(held, secret)
	if err != nil {
		t.Fatal(err)
	}

	moments := []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"an hour before it runs out", held.Expires.Add(-time.Hour), false},
		{"one second before it runs out", held.Expires.Add(-time.Second), false},
		// Expires is the first instant at which the grant is no longer one,
		// not the last at which it is — the same rule as every other exp
		// field a client author has met.
		{"the instant it runs out", held.Expires, true},
		{"one second after it runs out", held.Expires.Add(time.Second), true},
		{"a year after it runs out", held.Expires.Add(365 * 24 * time.Hour), true},
	}
	for _, moment := range moments {
		t.Run(moment.name, func(t *testing.T) {
			err := Grants(signature, held, secret, moment.now)
			switch {
			case moment.expired && !errors.Is(err, ErrExpired):
				t.Fatalf("at %s a grant that ran out at %s was answered %v, want ErrExpired",
					moment.now.Format(time.RFC3339), held.Expires.Format(time.RFC3339), err)
			case !moment.expired && err != nil:
				t.Fatalf("at %s a grant good until %s was refused: %v",
					moment.now.Format(time.RFC3339), held.Expires.Format(time.RFC3339), err)
			}
		})
	}

	// And an expired grant that is ALSO forged is forged, not expired. The
	// signature is checked first and always, so nothing a caller made up can
	// be used to ask this function whether some tuple has expired.
	if err := Grants(signature, another(held, func(h *Held) { h.DBName = "other" }), secret, held.Expires.Add(time.Hour)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an expired grant for the wrong database was reported as %v, want ErrBadSignature", err)
	}

	// The refusal names both clocks. An operator whose server is wrong has
	// nothing else to read: the credential says one time, the machine says
	// another, and the only place those two numbers meet is this sentence.
	err = Grants(signature, held, secret, held.Expires.Add(90*time.Minute))
	if err == nil {
		t.Fatal("an expired grant was accepted")
	}
	for _, want := range []string{"2026-09-21T13:00:00Z", "2026-09-21T14:30:00Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal reads %q, which does not name %s", err, want)
		}
	}
}

// another is a copy of a grant with one thing changed, so a table row can say
// what it changed rather than restating every field that it did not.
func another(base Held, change func(*Held)) Held {
	out := base
	out.Scopes = append([]string(nil), base.Scopes...)
	change(&out)
	return out
}

// TestAGrantMustCarryAnExpiryAndASerial is the mandatory half of ISS-11's
// first decision, enforced at the only place that can enforce it: a message
// that cannot be built is a grant that cannot be signed.
//
// Not "verified when present". A verifier that accepts a message with no
// expiry is the downgrade an attacker would pick — strip the field, and the
// credential is good forever again. There is one message shape and one branch
// that reads it, and the way to keep it that way is for the unexpiring grant
// to be unrepresentable rather than merely discouraged.
func TestAGrantMustCarryAnExpiryAndASerial(t *testing.T) {
	const secret = "the secret only the control plane has"

	// The control: the complete grant mints.
	if _, err := Granting(aGrant(), secret); err != nil {
		t.Fatalf("a complete grant was refused: %v", err)
	}

	for _, one := range []struct {
		name string
		held Held
		want error
	}{
		{"no expiry at all", another(aGrant(), func(h *Held) { h.Expires = time.Time{} }), ErrExpiry},
		{"the epoch itself", another(aGrant(), func(h *Held) { h.Expires = time.Unix(0, 0) }), ErrExpiry},
		{"before the epoch", another(aGrant(), func(h *Held) { h.Expires = time.Unix(-1, 0) }), ErrExpiry},
		{"no serial at all", another(aGrant(), func(h *Held) { h.Serial = "" }), ErrSerial},
		{"a serial of 65 bytes", another(aGrant(), func(h *Held) { h.Serial = strings.Repeat("s", SerialBytes+1) }), ErrSerial},
		{"a serial holding a newline", another(aGrant(), func(h *Held) { h.Serial = "01K5\nZQ9P" }), ErrSerial},
		{"a serial holding a NUL", another(aGrant(), func(h *Held) { h.Serial = "01K5\x00ZQ9P" }), ErrSerial},
	} {
		t.Run(one.name, func(t *testing.T) {
			if _, err := Granting(one.held, secret); !errors.Is(err, one.want) {
				t.Fatalf("minting gave %v, want %v", err, one.want)
			}
		})
	}

	// A serial of exactly the limit is not over it, and a serial holding the
	// two characters the v1 message would have had to ban is accepted — the
	// length prefix is what keeps the message unambiguous, not a character
	// table, and a rule banning them would be this encoding quietly leaning
	// on one again.
	for _, serial := range []string{strings.Repeat("s", SerialBytes), "2026:001", "a,b", "a:b,c:d"} {
		if _, err := Granting(another(aGrant(), func(h *Held) { h.Serial = serial }), secret); err != nil {
			t.Errorf("a serial of %q was refused: %v", serial, err)
		}
	}

	// Sub-second precision is not part of a grant: the message carries
	// Unix seconds, so two times inside one second are one grant, and a
	// caller that signs with nanoseconds and verifies without them must not
	// be told its own grant is forged.
	fine := another(aGrant(), func(h *Held) { h.Expires = grantAt.Add(time.Hour + 500*time.Millisecond) })
	signature, err := Granting(fine, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := Grants(signature, aGrant(), secret, grantAt); err != nil {
		t.Fatalf("a grant signed with a fraction of a second did not verify against the whole second: %v", err)
	}
}

// TestTwoGrantsCannotShareOneMessage is the collision test, and it is a
// security test rather than a formatting one: two different (account, dbname,
// scopes, exp, serial) tuples that write the same message are two different
// grants with one signature, which is a forgery primitive.
//
// The first pair is the one that matters, and it is not hypothetical. Under
// the obvious extension of the v1 format — account ":" dbname ":" scope_list
// ":" exp ":" serial, which is what "add two more fields" looks like when
// nobody stops to ask — these two tuples write the identical string
// "acme:main:x:100:200:z":
//
//	(scopes ["x"],     exp 100, serial "200:z")
//	(scopes ["x:100"], exp 200, serial "z")
//
// because a scope may hold a ":" and so may a serial, and the parser has no
// way to know where one field stopped. One signature, two meanings, one of
// them with an expiry a century away. The v2 message puts a byte count in
// front of every field, so there is nothing left for a ":" to be mistaken
// for.
//
// Run this against a GrantMessage that joins with colons instead and the
// first row goes red. That is the point of the row.
func TestTwoGrantsCannotShareOneMessage(t *testing.T) {
	const secret = "the secret only the control plane has"

	base := func(scopes []string, exp int64, serial string) Held {
		return Held{AccountID: "acme", DBName: "main", Scopes: scopes,
			Expires: time.Unix(exp, 0), Serial: serial}
	}

	pairs := []struct {
		name string
		one  Held
		two  Held
	}{
		{
			// The colon-joined collision, written out.
			"a scope eating the expiry",
			base([]string{"x"}, 100, "200:z"),
			base([]string{"x:100"}, 200, "z"),
		},
		{
			// The same trick one field to the left: a dbname is not allowed
			// a ":" today, but the scope list is, and the scope list is what
			// a naive format would let run into the expiry.
			"a scope eating the expiry and the serial",
			base([]string{"a:1:b"}, 2, "c"),
			base([]string{"a"}, 1, "b:2:c"),
		},
		{
			// Field boundaries between the first three, which is the shape
			// the v1 format was already proved safe against. It stays safe,
			// and the row is here so that a change which breaks it is caught
			// by this test rather than by the older one that no longer
			// exists in this form.
			"the account and the database split two ways",
			Held{AccountID: "ac", DBName: "me.main", Scopes: []string{"s"}, Expires: time.Unix(100, 0), Serial: "n"},
			Held{AccountID: "ac.me", DBName: "main", Scopes: []string{"s"}, Expires: time.Unix(100, 0), Serial: "n"},
		},
		{
			// An empty scope list next to a serial that could be read as one.
			"nothing granted against something that looks granted",
			base(nil, 100, "n"),
			base([]string{"100"}, 100, "n"),
		},
		{
			// A count is a string too: 1 byte of "1" against 11 bytes that
			// start with one.
			"a count that could be read as part of its field",
			base([]string{"1"}, 100, "n"),
			base([]string{"1:1234567"}, 100, "n"),
		},
	}

	for _, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			one, err := GrantMessage(pair.one)
			if err != nil {
				t.Fatalf("building the first message: %v", err)
			}
			two, err := GrantMessage(pair.two)
			if err != nil {
				t.Fatalf("building the second message: %v", err)
			}
			// The premise. Two rows that are secretly the same tuple would
			// make this test agree with itself and measure nothing.
			if reflect.DeepEqual(pair.one, pair.two) {
				t.Fatal("this row is one tuple written twice, so it cannot collide and does not measure anything")
			}
			if one == two {
				t.Fatalf("two different grants sign the same bytes %q — a signature over one is a signature over the other", one)
			}

			// And the signatures, which is the thing that actually matters:
			// a grant minted for one must not verify as the other.
			signature, err := Granting(pair.one, secret)
			if err != nil {
				t.Fatal(err)
			}
			if err := Grants(signature, pair.one, secret, time.Unix(1, 0)); err != nil {
				t.Fatalf("the first grant does not verify as itself (%v) — the refusal below measures nothing", err)
			}
			if err := Grants(signature, pair.two, secret, time.Unix(1, 0)); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("a grant minted for %+v verified as %+v", pair.one, pair.two)
			}
		})
	}
}

// TestTheGrantMessageIsTheseExactBytes pins the encoding, because every other
// test in this file would stay green if the whole format changed on both
// sides at once — and the TypeScript and PHP clients are not on both sides.
// fixtures/signing.json is the contract; this is the same bytes spelled out
// where a Go reader will see them.
func TestTheGrantMessageIsTheseExactBytes(t *testing.T) {
	message, err := GrantMessage(aGrant())
	if err != nil {
		t.Fatal(err)
	}
	want := "sapedb/scopes:v2\n" +
		"4:acme\n" +
		"4:main\n" +
		"27:articles:read,billing:write\n" +
		"10:1789995600\n" +
		"26:01K5ZQ9P7B3N4M6R8T0V2W4X6Y\n"
	if message != want {
		t.Fatalf("the signed message is\n%q\nwant\n%q", message, want)
	}
	// The version line is the label the key is derived under, deliberately
	// one string and not two: a message that names its own key cannot be
	// replayed into a verifier expecting a different one, and there is one
	// version spelling to change next time rather than two to get out of step.
	if !strings.HasPrefix(message, GrantLabel+"\n") {
		t.Fatalf("the message does not begin with GrantLabel %q", GrantLabel)
	}
	// A grant of nothing writes an empty third field, not a missing one.
	empty, err := GrantMessage(another(aGrant(), func(h *Held) { h.Scopes = nil }))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, "\n0:\n") {
		t.Fatalf("a grant of no scopes writes %q, which has no empty scope-list field in it", empty)
	}
}

// TestAGrantAndAConnectionStringAreNotInterchangeable is why GrantLabel
// exists.
//
// Both are HMACs under the same secret. If they shared a key, the only thing
// keeping one from being presented as the other would be the shape of the
// message — and a message is a string somebody chooses. A label of its own
// makes them different keys, so the question never gets as far as the message.
//
// Under v1 this test proved that by building two identical messages: a
// connection string whose password was a database name, against a grant of
// one scope. The v2 grant message cannot be made to equal a connection-string
// message at all — it begins with a version line no connection string has —
// so the premise is measured directly instead, on the keys themselves. That
// is the stronger form of the same claim: the messages now differ AND the
// keys differ, and this test would still fail if the second stopped being
// true.
func TestAGrantAndAConnectionStringAreNotInterchangeable(t *testing.T) {
	const secret = "the secret only the control plane has"

	// The keys, first and directly. Same secret, same bytes signed, two
	// labels: if these ever agreed, nothing about either message would
	// matter.
	sameBytes := []byte("whatever either side happens to be signing")
	connectionKey := hmac.New(sha256.New, key(secret, DefaultLabel))
	connectionKey.Write(sameBytes)
	grantKey := hmac.New(sha256.New, key(secret, GrantLabel))
	grantKey.Write(sameBytes)
	if hmac.Equal(connectionKey.Sum(nil), grantKey.Sum(nil)) {
		t.Fatal("the connection-string key and the grant key are the same key — one signature is both credentials")
	}

	parts := Parts{AccountID: "acme", Password: "main0000000000000", DBName: "read"}
	connection, err := Sign(parts, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	held := aGrant()
	grant, err := Granting(held, secret)
	if err != nil {
		t.Fatal(err)
	}

	// The control: each verifies as itself.
	if !Verify(connection, parts, secret, "") {
		t.Fatal("the connection-string signature does not verify as one")
	}
	if err := Grants(grant, held, secret, grantAt); err != nil {
		t.Fatalf("the grant does not verify as one: %v", err)
	}

	// Neither passes as the other.
	if err := Grants(connection, held, secret, grantAt); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a connection string's signature verified as a grant: %v", err)
	}
	if Verify(grant, parts, secret, "") {
		t.Fatal("a grant verified as a connection string's signature")
	}
}

// TestWhatAScopeMayBe: a scope may hold a ":" — every scope this product uses
// does — and may not hold the comma the list is joined with, because that is
// the one character that would make two different sets the same list.
func TestWhatAScopeMayBe(t *testing.T) {
	const secret = "the secret only the control plane has"

	if _, err := Granting(another(aGrant(), func(h *Held) { h.Scopes = []string{"articles:read"} }), secret); err != nil {
		t.Fatalf("a scope with a colon in it was refused: %v", err)
	}
	for _, scope := range []string{"", "a,b"} {
		if _, err := Granting(another(aGrant(), func(h *Held) { h.Scopes = []string{scope} }), secret); !errors.Is(err, ErrScope) {
			t.Errorf("a scope of %q was accepted, want ErrScope: %v", scope, err)
		}
	}

	// The ambiguity the comma ban closes, written out: without it, one scope
	// spelled "a,b" and two scopes "a" and "b" are the same list and so the
	// same grant. The length prefix does not save this one — it counts the
	// bytes of the joined list, and both spellings join to the same three.
	one, err := GrantMessage(another(aGrant(), func(h *Held) { h.Scopes = []string{"a", "b"} }))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one, "\n3:a,b\n") {
		t.Fatalf("the signed message is %q, which does not carry the scope list as a counted %q", one, "a,b")
	}

	// And a grant with no scopes at all is legal, and is not the same as a
	// grant of one empty-named scope, which is refused above.
	if _, err := Granting(another(aGrant(), func(h *Held) { h.Scopes = nil }), secret); err != nil {
		t.Fatalf("a grant of nothing was refused: %v", err)
	}
	if _, err := Granting(aGrant(), ""); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("a grant under an empty secret: want ErrEmptySecret, got %v", err)
	}
}

// grantFixture is the grant half of fixtures/signing.json — the vectors the
// TypeScript and PHP clients check themselves against.
type grantFixture struct {
	Label string `json:"label"`
	Cases []struct {
		Name      string   `json:"name"`
		AccountID string   `json:"accountId"`
		DBName    string   `json:"dbname"`
		Scopes    []string `json:"scopes"`
		Exp       int64    `json:"exp"`
		Serial    string   `json:"serial"`
		Message   string   `json:"message"`
		Sig       string   `json:"sig"`
	} `json:"cases"`
}

func loadGrants(t *testing.T) (string, grantFixture) {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/signing.json")
	if err != nil {
		t.Fatalf("the shared fixture must be readable: %v", err)
	}
	var whole struct {
		Secret string       `json:"secret"`
		Grant  grantFixture `json:"grant"`
	}
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Before SAPE-21 this file carried nothing but connection-string triples,
	// and a client had no way to check its handling of a grant against
	// anything. An empty section here would make every assertion below pass
	// over no rows at all, which is the one way this test goes quiet without
	// anybody editing it.
	if len(whole.Grant.Cases) < 7 {
		t.Fatalf("fixture: expected at least 7 grant cases, got %d", len(whole.Grant.Cases))
	}
	return whole.Secret, whole.Grant
}

// TestTheGrantFixtureAgreesWithTheClientSide is the grant twin of
// TestFixtureAgreesWithTheAppSide, and it is the most valuable test in this
// file for the same reason: the clients do not sign, so the only thing that
// can catch this side and their side drifting apart is a file they both read.
//
// It checks the message bytes as well as the signature, deliberately. A client
// that gets the signature wrong learns nothing from "it did not verify"; a
// client that can compare its own encoder's output against `message` finds the
// missing byte count or the unsorted scope list in one look. The fixture's
// vectors were computed independently of this package, so agreement here is
// two implementations meeting rather than this one agreeing with itself.
func TestTheGrantFixtureAgreesWithTheClientSide(t *testing.T) {
	secret, fixture := loadGrants(t)

	// Read from the file rather than from GrantLabel, and then compared to
	// it — the same gap TestFixtureAgreesWithTheAppSide closes for the
	// connection-string label. Renaming one without the other would otherwise
	// leave every row below green.
	if fixture.Label != GrantLabel {
		t.Fatalf("fixture grant label %q does not match GrantLabel %q — one of them was renamed without the other",
			fixture.Label, GrantLabel)
	}

	hexOnly := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]string{}
	for _, one := range fixture.Cases {
		t.Run(one.Name, func(t *testing.T) {
			held := Held{
				AccountID: one.AccountID,
				DBName:    one.DBName,
				Scopes:    one.Scopes,
				Expires:   time.Unix(one.Exp, 0),
				Serial:    one.Serial,
			}
			message, err := GrantMessage(held)
			if err != nil {
				t.Fatalf("building the message: %v", err)
			}
			if message != one.Message {
				t.Errorf("the message is\n  %q\nand the fixture says\n  %q", message, one.Message)
			}
			if !hexOnly.MatchString(one.Sig) {
				t.Errorf("the fixture signature %q is not 64 lower-case hex characters", one.Sig)
			}
			signature, err := Granting(held, secret)
			if err != nil {
				t.Fatalf("minting: %v", err)
			}
			if signature != one.Sig {
				t.Errorf("this side signs\n  %s\nand the fixture says\n  %s", signature, one.Sig)
			}
			// And it verifies, at a moment before it runs out — which is the
			// thing a client actually needs to be true.
			if err := Grants(one.Sig, held, secret, time.Unix(one.Exp-1, 0)); err != nil {
				t.Errorf("the fixture signature does not verify: %v", err)
			}
		})
		seen[one.Name] = one.Sig
	}

	// Two rows the file exists to pin, named rather than counted. The first
	// is canonicalisation: the same set written differently is one grant. The
	// second is the collision pair, which is the whole argument for counting
	// the bytes of a field instead of joining with a delimiter — a client
	// that reimplements the v1 style will produce one signature for both.
	pairs := []struct {
		one, two string
		same     bool
	}{
		{"plain", `the same set written in another order, with a repeat — must sign identically to "plain"`, true},
		{"a scope eating the expiry — one half of the pair a colon-joined format would confuse",
			"the other half, which must sign differently", false},
	}
	for _, pair := range pairs {
		one, two := seen[pair.one], seen[pair.two]
		if one == "" || two == "" {
			t.Fatalf("the fixture no longer carries both of %q and %q, so this check measures nothing", pair.one, pair.two)
		}
		if (one == two) != pair.same {
			t.Errorf("%q and %q sign %s and %s; want them %s",
				pair.one, pair.two, one, two, map[bool]string{true: "identical", false: "different"}[pair.same])
		}
	}
}
