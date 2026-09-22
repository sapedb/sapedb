package bundle

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/number"
)

// ISS-35's second half, for the artifact that travels.
//
// A bundle is a set of declarations somebody else wrote, signed, and handed
// out. Until this, a constant inside one — `"n": {"value": 9007199254740993}`
// — became a float64 at Parse's Decode, which meant three things in a row, all
// silent:
//
//   - `seal` signed 9007199254740992. The author's own file said one number
//     and the artifact they published said another, and nothing said so.
//   - `verify` reported on the rounded declaration as though it were the
//     file's.
//   - `install` declared it, on every host, for the life of the bundle.
//
// And the signature did not help, which is the part worth writing down: it is
// computed over the declarations as they were PARSED, so a file edited from
// 9007199254740992 back to 9007199254740993 verifies under the author's
// signature — both texts parse to the same float64, so both produce the same
// signed message. That is measured below rather than argued.

// bundleText is a bundle written out as an author would write one, with the
// constant as literal text. Built as text on purpose: marshalling a Go value
// would round the number in the test before Parse ever saw it, which is the
// bug, and would leave these tests green against a Parse that does nothing.
func bundleText(constant string) []byte {
	return []byte(fmt.Sprintf(`{
  "format": %q,
  "name": "ids",
  "version": "1.0.0",
  "signer": "acme-eng",
  "collections": [
    {"name": "ids", "key": {"path": "id", "type": "string", "auto": "ulid"}, "indexes": []}
  ],
  "operations": [
    {"name": "ids.fixed", "collection": "ids", "action": "insert",
     "document": {"n": {"value": %s, "constant": true}}}
  ]
}`, Label, constant))
}

// TestParseRefusesAConstantThatCannotBeStoredAsSent is the refusal, at the
// door all three commands come through.
func TestParseRefusesAConstantThatCannotBeStoredAsSent(t *testing.T) {
	_, err := Parse(bundleText("9007199254740993"))
	if err == nil {
		t.Fatal("a bundle carrying 9007199254740993 was read, and it would have been installed as 9007199254740992")
	}
	if !errors.Is(err, number.ErrPrecision) {
		t.Fatalf("the refusal is not ErrPrecision, so it reaches a client as something else: %v", err)
	}
	// Not ErrBundle. The file is a perfectly good bundle; one value in it
	// cannot be stored as written. Those are two different sentences, and an
	// author told "this is not a bundle" goes looking at the wrong thing.
	if errors.Is(err, ErrBundle) {
		t.Errorf("the refusal says the file is not a bundle: %v", err)
	}
	if !strings.Contains(err.Error(), "9007199254740993") || !strings.Contains(err.Error(), "9007199254740992") {
		t.Errorf("the refusal does not name the value and what would have been stored: %v", err)
	}
	if !strings.Contains(err.Error(), `operation "ids.fixed".document.n.value`) {
		t.Errorf("the refusal does not say which constant it was: %v", err)
	}
}

// TestTheBundleBoundaryIsRepresentabilityNotMagnitude pins both sides here
// too. A Parse that refused every large integer would pass a table of refusals
// and would reject working authors: 9007199254740994 and 9223372036854775808
// are both larger than the refused 9007199254740993 and both survive exactly.
func TestTheBundleBoundaryIsRepresentabilityNotMagnitude(t *testing.T) {
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
			b, err := Parse(bundleText(one.sent))

			if one.refused {
				if err == nil {
					t.Fatalf("%s (%s) was read rather than refused", one.sent, one.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s (%s) was refused: %v", one.sent, one.why, err)
			}
			// And it is a float64 afterwards, not a json.Number: store.Declare,
			// internal/keys and the catalogue's own marshal all expect one.
			if _, ok := b.Operations[0].Document["n"].Value.(float64); !ok {
				t.Fatalf("the constant survived as %T, want float64", b.Operations[0].Document["n"].Value)
			}
		})
	}
}

// TestASignatureNeverProtectedTheNumber is why this check has to be at Parse
// and not at install.
//
// Seal signs the declarations as they were parsed. Before ISS-35 that meant
// both 9007199254740992 and 9007199254740993 produced the same signed message,
// so an author's signature over the first verified the second — anybody could
// edit a published bundle's identifier to a neighbouring value and it would
// still install, under the author's key, as the original. The signature is
// over what was READ, and what was read had already lost the digits.
//
// This test pins that the two texts still produce one message (which is a fact
// about float64, not something to fix) and that Parse now refuses the text
// that needed the protection.
func TestASignatureNeverProtectedTheNumber(t *testing.T) {
	public, _ := keyFrom(t, 9)

	rounded, err := Parse(bundleText("9007199254740992"))
	if err != nil {
		t.Fatalf("the rounded form is a legal bundle: %v", err)
	}
	signed, err := Message(rounded, public)
	if err != nil {
		t.Fatal(err)
	}

	// The same declaration with the digits the author meant, parsed the way
	// Parse worked before this change: straight to float64.
	asBefore := rounded
	term := asBefore.Operations[0].Document["n"]
	term.Value = float64(9007199254740993)
	asBefore.Operations[0].Document["n"] = term
	other, err := Message(asBefore, public)
	if err != nil {
		t.Fatal(err)
	}
	if signed != other {
		t.Fatal("the two spellings no longer sign the same message, so this test's premise has changed — read its comment")
	}

	// Which is exactly why the refusal is at Parse, before Check is ever
	// reached: the signature cannot tell these apart, so the reader has to.
	if _, err := Parse(bundleText("9007199254740993")); !errors.Is(err, number.ErrPrecision) {
		t.Fatalf("the text the signature could not protect is still read: %v", err)
	}
}
