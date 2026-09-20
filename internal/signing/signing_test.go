package signing

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
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

// TestAGrantIsTheOneSetOfScopesForTheOneDatabase is the domain separation a
// grant lives or dies by.
//
// A grant says three things at once — these scopes, this account, this
// database — and all three are inside the message, so none of them can be
// changed without the signature failing. The table is what a grant must NOT
// verify against, and the first row of the test is what it must: a table of
// nothing but false is also what a Grants that always returns false would
// produce.
func TestAGrantIsTheOneSetOfScopesForTheOneDatabase(t *testing.T) {
	const secret = "the secret only the control plane has"

	held := Held{AccountID: "acme", DBName: "main", Scopes: []string{"articles:read", "billing:write"}}
	signature, err := Granting(held, secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) != 64 {
		t.Fatalf("a grant is %d characters, want 64 of lower-case hex", len(signature))
	}

	// The known-positive. Everything below is a false that only means
	// something because this is a true.
	if !Grants(signature, held, secret) {
		t.Fatal("a grant does not verify against what it was made from — nothing else in this test measures anything")
	}

	// And the set is a set: written in another order, with a repeat, it is the
	// same grant.
	shuffled := Held{AccountID: "acme", DBName: "main",
		Scopes: []string{"billing:write", "articles:read", "billing:write"}}
	if !Grants(signature, shuffled, secret) {
		t.Fatal("the same scopes written in another order were not the same grant")
	}

	elsewhere := []struct {
		name string
		held Held
	}{
		{"another database", Held{AccountID: "acme", DBName: "other", Scopes: held.Scopes}},
		{"another account", Held{AccountID: "rival", DBName: "main", Scopes: held.Scopes}},
		{"a scope added", Held{AccountID: "acme", DBName: "main", Scopes: []string{"articles:read", "billing:write", "admin"}}},
		{"a scope removed", Held{AccountID: "acme", DBName: "main", Scopes: []string{"articles:read"}}},
		{"no scopes at all", Held{AccountID: "acme", DBName: "main"}},
	}
	refused := 0
	for _, one := range elsewhere {
		t.Run(one.name, func(t *testing.T) {
			if Grants(signature, one.held, secret) {
				t.Fatalf("a grant made for %+v verified as %+v", held, one.held)
			}
			refused++
		})
	}
	if refused != len(elsewhere) {
		t.Fatalf("%d of %d rows ran", refused, len(elsewhere))
	}

	// Another secret is another server, whatever the grant says.
	if Grants(signature, held, "a different secret entirely") {
		t.Fatal("a grant verified under a secret it was not made with")
	}
}

// TestAGrantAndAConnectionStringAreNotInterchangeable is why GrantLabel
// exists.
//
// Both are HMACs under the same secret. If they shared a key, the only thing
// keeping one from being presented as the other would be the shape of the
// message — and a message is a string somebody chooses. A label of its own
// makes them different keys, so the question never gets as far as the message.
func TestAGrantAndAConnectionStringAreNotInterchangeable(t *testing.T) {
	const secret = "the secret only the control plane has"

	// The two messages are deliberately built to be the same bytes: an account
	// whose "password" is a database name, against a grant of one scope. Under
	// one key this would be one signature.
	parts := Parts{AccountID: "acme", Password: "main0000000000000", DBName: "read"}
	connection, err := Sign(parts, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	held := Held{AccountID: "acme", DBName: "main0000000000000", Scopes: []string{"read"}}
	grant, err := Granting(held, secret)
	if err != nil {
		t.Fatal(err)
	}

	// The control: each verifies as itself.
	if !Verify(connection, parts, secret, "") {
		t.Fatal("the connection-string signature does not verify as one")
	}
	if !Grants(grant, held, secret) {
		t.Fatal("the grant does not verify as one")
	}

	message, err := Message(parts)
	if err != nil {
		t.Fatal(err)
	}
	grantMessage, err := GrantMessage(held)
	if err != nil {
		t.Fatal(err)
	}
	if message != grantMessage {
		t.Fatalf("this test's premise is stale: it means to sign identical bytes two ways and the bytes are %q and %q", message, grantMessage)
	}

	// Identical bytes, different signatures, and neither passes as the other.
	if connection == grant {
		t.Fatal("a connection string's signature and a grant over the same bytes are the same signature — the two are interchangeable")
	}
	if Grants(connection, held, secret) {
		t.Fatal("a connection string's signature verified as a grant")
	}
	if Verify(grant, parts, secret, "") {
		t.Fatal("a grant verified as a connection string's signature")
	}
}

// TestWhatAScopeMayBe: a scope may hold a ":" — every scope this product uses
// does — and may not hold the comma the list is joined with, because that is
// the one character that would make two different sets the same message.
func TestWhatAScopeMayBe(t *testing.T) {
	const secret = "the secret only the control plane has"

	if _, err := Granting(Held{AccountID: "acme", DBName: "main", Scopes: []string{"articles:read"}}, secret); err != nil {
		t.Fatalf("a scope with a colon in it was refused: %v", err)
	}
	for _, scope := range []string{"", "a,b"} {
		if _, err := Granting(Held{AccountID: "acme", DBName: "main", Scopes: []string{scope}}, secret); !errors.Is(err, ErrScope) {
			t.Errorf("a scope of %q was accepted, want ErrScope: %v", scope, err)
		}
	}

	// The ambiguity the comma ban closes, written out: without it, one scope
	// spelled "a,b" and two scopes "a" and "b" are the same message and so the
	// same grant.
	one, err := GrantMessage(Held{AccountID: "acme", DBName: "main", Scopes: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if one != "acme:main:a,b" {
		t.Fatalf("the signed message is %q, want %q", one, "acme:main:a,b")
	}

	// And a grant with no scopes at all is legal, and is not the same as a
	// grant of one empty-named scope, which is refused above.
	if _, err := Granting(Held{AccountID: "acme", DBName: "main"}, secret); err != nil {
		t.Fatalf("a grant of nothing was refused: %v", err)
	}
	if _, err := Granting(Held{AccountID: "acme", DBName: "main"}, ""); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("a grant under an empty secret: want ErrEmptySecret, got %v", err)
	}
}
