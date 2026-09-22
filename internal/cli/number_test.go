package cli

import (
	"encoding/json"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// TestTheShellDoesNotRoundANumberBeforeTheWire is the half of ISS-35 that is
// easy to miss and would have made the rest of it pointless.
//
// The server's refusal is the authority, and it works by looking at the digits
// the caller wrote. This command used to unmarshal a declared `number` into a
// float64 before sending it, which means it destroyed those digits itself:
// typing n=9007199254740993 handed the daemon 9007199254740992, and the daemon
// then accepted it, correctly, because by then nobody had told it anything
// else. A server-side fix alone would have been unreachable through the tool
// this project ships — and unreachable through the exact command ISS-35 used
// to measure the bug.
//
// So this asserts what goes on the wire, by marshalling what valueFor returns,
// rather than asserting a Go type. The wire is what the server reads.
func TestTheShellDoesNotRoundANumberBeforeTheWire(t *testing.T) {
	for _, one := range []struct {
		declared string
		written  string
		wire     string
	}{
		// The value ISS-35 calls the clearest: off by one, silently.
		{store.TypeNumber, "9007199254740993", `9007199254740993`},
		{store.TypeNumber, "9223372036854775807", `9223372036854775807`},
		{store.TypeNumber, "1234567890123456789", `1234567890123456789`},

		// Values that always round-tripped still go out as written.
		{store.TypeNumber, "9007199254740992", `9007199254740992`},
		{store.TypeNumber, "42", `42`},
		{store.TypeNumber, "-1", `-1`},
		{store.TypeNumber, "1.5", `1.5`},

		// An "any" argument is usually a whole document, so the number at risk
		// is a nested one nobody is looking at while they type.
		{store.TypeAny, `{"id":1234567890123456789,"qty":2}`, `{"id":1234567890123456789,"qty":2}`},
		{store.TypeAny, `[9007199254740993]`, `[9007199254740993]`},
		{store.TypeAny, `"a string"`, `"a string"`},
	} {
		t.Run(one.declared+"/"+one.written, func(t *testing.T) {
			value, err := valueFor(store.Parameter{Name: "n", Type: one.declared}, one.written)
			if err != nil {
				t.Fatalf("%s was refused by the shell: %v", one.written, err)
			}
			sent, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(sent) != one.wire {
				t.Fatalf("typed %s, sent %s, want %s", one.written, sent, one.wire)
			}
		})
	}
}

// TestTheShellStillRefusesWhatIsNotANumber is the control.
//
// Reading a number without turning it into a float64 means the float64 is no
// longer doing the validating, so the refusals it used to provide have to
// still be here. `null` is in the table because it is the one input whose
// answer is historical rather than obvious: unmarshalling `null` into a
// float64 left that float64 at its zero value, so zero is what this command
// has always sent, and ISS-35 is not the ticket that changes it.
func TestTheShellStillRefusesWhatIsNotANumber(t *testing.T) {
	for _, one := range []struct {
		written string
		refused bool
		wire    string
	}{
		{written: "abc", refused: true},
		{written: `"12"`, refused: true},
		{written: "true", refused: true},
		{written: "[1]", refused: true},
		{written: "{}", refused: true},
		{written: "", refused: true},
		{written: "1 2", refused: true},
		{written: "null", refused: false, wire: `0`},
	} {
		t.Run(one.written, func(t *testing.T) {
			value, err := valueFor(store.Parameter{Name: "n", Type: store.TypeNumber}, one.written)
			if one.refused {
				if err == nil {
					t.Fatalf("%q was accepted as a number", one.written)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was refused: %v", one.written, err)
			}
			sent, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(sent) != one.wire {
				t.Fatalf("%q was sent as %s, want %s", one.written, sent, one.wire)
			}
		})
	}
}
