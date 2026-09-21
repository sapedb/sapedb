package bundle

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// keyHex is a deterministic public key as an operator would paste it.
func keyHex(t *testing.T, seed byte) string {
	t.Helper()
	public, _ := keyFrom(t, seed)
	return hex.EncodeToString(public)
}

// TestATrustListIsReadFromOneEnvironmentVariable is what an operator
// actually types. Every case below is a line somebody could plausibly write,
// and the point of the table is that the ones that are refused are refused
// by name rather than silently producing a shorter list than was written —
// a trust list that quietly drops an entry is a server that refuses a
// bundle nobody can work out why it refused.
func TestATrustListIsReadFromOneEnvironmentVariable(t *testing.T) {
	first, second := keyHex(t, 1), keyHex(t, 2)

	for _, tc := range []struct {
		name    string
		value   string
		signers int
		labels  []string
		refused string
	}{
		{name: "one signer", value: "acme-eng=" + first, signers: 1, labels: []string{"acme-eng"}},
		{
			name:    "two, comma separated, as a shell writes them",
			value:   "acme-eng=" + first + ",partner-co=" + second,
			signers: 2, labels: []string{"acme-eng", "partner-co"},
		},
		{
			name:    "two, newline separated, as an env file writes them",
			value:   "acme-eng=" + first + "\npartner-co=" + second + "\n",
			signers: 2, labels: []string{"acme-eng", "partner-co"},
		},
		{
			name:    "spaces around the entries survive",
			value:   "  acme-eng = " + first + " ,  partner-co = " + second + "  ",
			signers: 2, labels: []string{"acme-eng", "partner-co"},
		},
		{name: "a label may hold spaces", value: "ACME Engineering=" + first, signers: 1, labels: []string{"ACME Engineering"}},
		{name: "a trailing comma is not an entry", value: "acme-eng=" + first + ",", signers: 1, labels: []string{"acme-eng"}},
		{name: "blank lines are not entries", value: "\n\nacme-eng=" + first + "\n\n", signers: 1, labels: []string{"acme-eng"}},

		// Not set, and set to nothing, are the same thing and neither is an
		// error. Neither is permission either — see the test below.
		{name: "unset", value: "", signers: 0},
		{name: "whitespace only", value: "  \n , \n ", signers: 0},

		// Everything below is refused, and each is refused for a different
		// reason an operator can act on.
		{
			name:    "a key with no label",
			value:   first,
			refused: "is not label=key",
		},
		{
			name:    "a label with no key",
			value:   "acme-eng",
			refused: "is not label=key",
		},
		{
			name:    "an empty label",
			value:   "=" + first,
			refused: "has no usable label",
		},
		{
			name:    "a key that is not 64 hex characters",
			value:   "acme-eng=abc123",
			refused: "expected 64 hex characters, got 6",
		},
		{
			name:    "a key written in upper case",
			value:   "acme-eng=" + strings.ToUpper(first),
			refused: "hex must be lower case",
		},
		{
			name:    "one key under two labels",
			value:   "acme-eng=" + first + ",the-same-people=" + first,
			refused: "already on the list",
		},
		{
			// The mistake NewTrust's own duplicate check cannot see, because
			// from its side these are two different keys. The label is what
			// `verify` prints and what the change log records, so one label
			// over two keys makes both of those say something untrue.
			name:    "one label over two keys",
			value:   "acme-eng=" + first + ",acme-eng=" + second,
			refused: "calls two different keys",
		},
		{
			name:    "a label carrying an = of its own",
			value:   "acme=eng=" + first,
			refused: "expected 64 hex characters",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust, err := ParseTrust(tc.value)

			if tc.refused != "" {
				if err == nil {
					t.Fatalf("ParseTrust(%q) was accepted, and holds %d signer(s)", tc.value, trust.Signers())
				}
				if !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("ParseTrust(%q) refused with %q, which does not say %q", tc.value, err, tc.refused)
				}
				if !errors.Is(err, ErrKey) {
					t.Errorf("a refused trust list must arrive as ErrKey, so internal/server names it bundle_key; got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseTrust(%q): %v", tc.value, err)
			}
			if trust.Signers() != tc.signers {
				t.Fatalf("ParseTrust(%q) holds %d signer(s), want %d", tc.value, trust.Signers(), tc.signers)
			}

			// The labels are checked by verifying a bundle signed by the key
			// they were written beside, rather than by reading Trust's map:
			// the label is only ever worth anything as the thing Verify hands
			// back, and a test that read the map would pass even if Verify
			// returned the bundle's own self-asserted Signer instead.
			for i, label := range tc.labels {
				public, private := keyFrom(t, byte(i+1))
				b := sealed(t, private)
				got, err := trust.Verify(b)
				if err != nil {
					t.Fatalf("a bundle signed by %s was refused: %v", hex.EncodeToString(public), err)
				}
				if got != label {
					t.Errorf("Verify returned %q for the key written beside %q", got, label)
				}
			}
		})
	}
}

// TestAnEmptyTrustListIsNotPermission is the fail-closed rule, asked of the
// configuration surface rather than of Trust: SAPE-10 made an empty list
// refuse everything, and the job of a parser is to make sure no spelling of
// SAPEDB_TRUST can produce a list that means "anything".
//
// The three values below are what somebody would reach for if they wanted
// that, and none of them is a wildcard: each is either an empty list or a
// refusal. There is no fourth thing to try, because the only shape this
// parser accepts at all is a 64-character hex key.
func TestAnEmptyTrustListIsNotPermission(t *testing.T) {
	_, private := keyFrom(t, 7)
	b := sealed(t, private)

	for _, value := range []string{"", "   ", "\n", "everyone=*", "*", "any=any"} {
		t.Run(value, func(t *testing.T) {
			trust, err := ParseTrust(value)
			if err != nil {
				return // Refused outright, which is also not permission.
			}
			if trust.Signers() != 0 {
				t.Fatalf("ParseTrust(%q) produced %d signer(s)", value, trust.Signers())
			}
			if _, err := trust.Verify(b); !errors.Is(err, ErrNoTrust) {
				t.Fatalf("a bundle presented to the list from %q was answered with %v, not ErrNoTrust", value, err)
			}
		})
	}
}

// TestTheEnvironmentIsReadUnderOneName pins the variable's name and the
// lookup it goes through. Both packages that read a trust list read it here,
// so a rename in one of them cannot leave the other reading the old one —
// but only for as long as the name is in this package, which is what this
// test is for.
func TestTheEnvironmentIsReadUnderOneName(t *testing.T) {
	if TrustEnv != "SAPEDB_TRUST" {
		t.Errorf("the trust list variable is %q; every operator's configuration names it", TrustEnv)
	}

	asked := []string{}
	lookup := func(name string) (string, bool) {
		asked = append(asked, name)
		if name == TrustEnv {
			return "acme-eng=" + keyHex(t, 3), true
		}
		return "", false
	}

	trust, err := TrustFromEnv(lookup)
	if err != nil {
		t.Fatalf("TrustFromEnv: %v", err)
	}
	if trust.Signers() != 1 {
		t.Fatalf("TrustFromEnv read %d signer(s) from one entry", trust.Signers())
	}
	if len(asked) != 1 || asked[0] != TrustEnv {
		t.Errorf("TrustFromEnv looked up %v; it must read exactly one variable", asked)
	}

	// A variable that is set to nothing is an empty list, not a fallback to
	// something else: there is nothing for it to fall back to.
	empty, err := TrustFromEnv(func(string) (string, bool) { return "", true })
	if err != nil {
		t.Fatalf("TrustFromEnv on an empty variable: %v", err)
	}
	if empty.Signers() != 0 {
		t.Errorf("an empty %s produced %d signer(s)", TrustEnv, empty.Signers())
	}
}
