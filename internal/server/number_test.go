package server

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// declareIDs sets up the collection ISS-35 was measured against: one field
// declared `number`, indexed, so the value goes through internal/keys as well
// as into the document body.
func declareIDs(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "ids",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_n",
			Fields: []store.Field{{Path: "n", Type: store.TypeNumber, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []store.Operation{
		{
			Name: "ids.put", Collection: "ids", Action: store.ActionInsert,
			Input:    []store.Parameter{{Name: "n", Type: store.TypeNumber, Required: true}},
			Document: map[string]store.Term{"n": {Arg: "n"}},
		},
		{
			// An "any" argument carrying a whole body, so the walk into a
			// nested value is measured rather than assumed: a number three
			// levels down was rounded exactly as silently as a top-level one.
			Name: "ids.put_body", Collection: "ids", Action: store.ActionInsert,
			Input:    []store.Parameter{{Name: "body", Type: store.TypeAny, Required: true}},
			Document: map[string]store.Term{"body": {Arg: "body"}},
		},
	} {
		if _, err := db.DeclareOperation(store.Caller{}, operation); err != nil {
			t.Fatalf("declare %q: %v", operation.Name, err)
		}
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestTheFourMeasuredValuesOfISS35 is the ticket's own measurement, run on the
// wire rather than described: one collection, one insert declaring a field as
// `number`, the four values ISS-35 sent through a real daemon.
//
// Before this, all four answered `changed 1` and exit 0, and three of them
// were a different number afterwards. Three are now refused by name, and the
// one that always round-tripped still goes in.
//
// The values are sent as json.Number so the digits reach the socket as typed.
// A float64 here would round in this test before the server ever saw it, which
// is precisely the bug — and would leave this test green against a server that
// does nothing.
func TestTheFourMeasuredValuesOfISS35(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")

	client := dial(t, address)
	if frame := client.open(server, "acme", "main"); frame.Type != protocol.Welcome {
		t.Fatalf("the handshake answered with a %s: %s", frame.Type, frame.Payload)
	}

	for _, one := range []struct {
		sent    string
		refused bool
		instead string
	}{
		{sent: "9007199254740992", refused: false},
		{sent: "9007199254740993", refused: true, instead: "9007199254740992"},
		{sent: "9223372036854775807", refused: true, instead: "9223372036854776000"},
		{sent: "1234567890123456789", refused: true, instead: "1234567890123456800"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			frame := client.invoke("ids.put", map[string]any{"n": json.Number(one.sent)})

			if !one.refused {
				if frame.Type != protocol.Result {
					t.Fatalf("a value that round-trips was refused: %s", frame.Payload)
				}
				return
			}

			if frame.Type != protocol.Failure {
				t.Fatalf("%s was stored rather than refused: %s %s", one.sent, frame.Type, frame.Payload)
			}
			refusal := decode[struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			}](t, frame)

			// The value by name. A refusal that says "a number was out of
			// range" leaves the caller to guess which of their fields it was.
			if !strings.Contains(refusal.Message, one.sent) {
				t.Errorf("the refusal does not name the value sent: %q", refusal.Message)
			}
			// And what would have been stored, which is the half that makes
			// the refusal readable: the caller can see the neighbouring number
			// they would otherwise have found in the database.
			if !strings.Contains(refusal.Message, one.instead) {
				t.Errorf("the refusal does not say what would have been stored (%s): %q", one.instead, refusal.Message)
			}
			// Its own code, never "failed" — the shape ISS-21 cost this
			// project once. A client switches on this byte, not on the prose.
			if refusal.Code != "precision" {
				t.Errorf("the refusal reached the client as %q, want %q", refusal.Code, "precision")
			}
		})
	}
}

// TestTheNumberBoundaryIsRepresentabilityNotMagnitude pins BOTH sides of it.
//
// One side alone is not a boundary. A fix that refused every integer above
// 2^53 would pass a test that only checked the three refusals in ISS-35's
// table, and it would be wrong: 9007199254740994 and 9223372036854775808 are
// both larger than 2^53 and both come back exactly as sent. Refusing them
// would break working callers to fix a bug they never had, which is the worse
// of the two outcomes.
//
// The accepted rows are therefore not padding. They are the half of the claim
// that says this is a rule about what float64 holds, not about how big a
// number looks.
func TestTheNumberBoundaryIsRepresentabilityNotMagnitude(t *testing.T) {
	for _, one := range []struct {
		sent    string
		refused bool
		why     string
	}{
		{"9007199254740992", false, "2^53, exactly a float64"},
		{"9007199254740993", true, "2^53+1, the nearest float64 is 2^53"},
		{"9007199254740994", false, "2^53+2 — larger than the refused one, and exact"},
		{"18014398509481984", false, "2^54, exact"},
		{"9223372036854775807", true, "2^63-1, not a float64"},
		{"9223372036854775808", false, "2^63 — vastly larger than the refused one, and exact"},
		{"-9007199254740993", true, "the same hole below zero"},
		{"-9007199254740992", false, "-2^53, exact"},
		{"0", false, "zero"},
		{"-0", false, "negative zero"},
		{"-1", false, "an ordinary negative"},
		{"42", false, "an ordinary integer"},
		{"1.5", false, "a fraction float64 holds exactly"},
		{"0.1", false, "a fraction it does not — and never has, for anybody"},
		{"1.5e300", false, "integer-valued, written as a float, and printed back as written"},
		{"1e400", true, "outside float64 altogether"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			_, _, ok := carries(json.Number(one.sent))
			if ok == one.refused {
				t.Fatalf("%s (%s): accepted = %v, want accepted = %v", one.sent, one.why, ok, !one.refused)
			}
		})
	}
}

// TestAnAcceptedNumberIsTheNumberThatComesBack is the accepted half measured
// rather than trusted: every value the check lets through is put through the
// same float64 the store and internal/keys use, and read back.
//
// Two rows are deliberately separated, because they are not the same claim:
//
//   - Most accepted values come back as the identical text.
//   - 9223372036854775808 does not. The float64 holds 2^63 exactly — nothing
//     is lost, and reading it back gives the same number — but the shortest
//     decimal that identifies that float64 is 9223372036854776000, so that is
//     what a dump prints. The value survives; its spelling does not. That is
//     stated here rather than papered over, because somebody comparing a dump
//     against what they sent will meet it.
func TestAnAcceptedNumberIsTheNumberThatComesBack(t *testing.T) {
	for _, one := range []struct {
		sent    string
		printed string
	}{
		{"9007199254740992", "9007199254740992"},
		{"9007199254740994", "9007199254740994"},
		{"18014398509481984", "18014398509481984"},
		{"-9007199254740992", "-9007199254740992"},
		{"0", "0"},
		{"42", "42"},
		{"1.5", "1.5"},

		// The one accepted value whose printed form differs. Same number,
		// different spelling.
		{"9223372036854775808", "9223372036854776000"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			held, _, ok := carries(json.Number(one.sent))
			if !ok {
				t.Fatalf("%s was refused, and it round-trips", one.sent)
			}
			if got := strconv.FormatFloat(held, 'f', -1, 64); got != one.printed {
				t.Fatalf("%s came back as %s, want %s", one.sent, got, one.printed)
			}
		})
	}
}

// TestANumberBuriedInABodyIsCheckedToo walks the nesting.
//
// A document body is unconstrained JSON, and a Snowflake id three levels down
// inside one was rounded exactly as quietly as a top-level argument. The
// refusal names where it found it, because "a number in your call" is not
// something a caller can act on when the call carries two hundred of them.
func TestANumberBuriedInABodyIsCheckedToo(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")

	client := dial(t, address)
	if frame := client.open(server, "acme", "main"); frame.Type != protocol.Welcome {
		t.Fatalf("the handshake answered with a %s: %s", frame.Type, frame.Payload)
	}

	frame := client.invoke("ids.put_body", map[string]any{"body": map[string]any{
		"order": map[string]any{
			"lines": []any{
				map[string]any{"sku": "a", "qty": json.Number("2")},
				map[string]any{"sku": "b", "id": json.Number("1234567890123456789")},
			},
		},
	}})

	if frame.Type != protocol.Failure {
		t.Fatalf("a nested value was stored rather than refused: %s %s", frame.Type, frame.Payload)
	}
	refusal := decode[struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}](t, frame)
	if refusal.Code != "precision" {
		t.Errorf("code %q, want %q", refusal.Code, "precision")
	}
	// The path, not just the value: which field, in a body full of them.
	if !strings.Contains(refusal.Message, `"body".order.lines[1].id`) {
		t.Errorf("the refusal does not say where the number was: %q", refusal.Message)
	}
}

// TestAGoodCallStillCarriesItsNumbersAsFloat64 is the quiet half of the fix.
//
// The check decodes with UseNumber, which means every number in a call arrives
// as a json.Number rather than a float64. Everything downstream — store's
// `matches`, internal/keys, the change log — expects float64 and says so, so a
// json.Number reaching any of them would be a new bug wearing this fix as a
// disguise, and one that only shows up on the values that were fine all along.
//
// Measured on the value, not on the absence of a crash.
func TestAGoodCallStillCarriesItsNumbersAsFloat64(t *testing.T) {
	arguments := map[string]any{
		"n":    json.Number("42"),
		"body": map[string]any{"deep": []any{json.Number("1.5"), "text", nil}},
	}
	if err := exactArguments(arguments); err != nil {
		t.Fatalf("a call of ordinary numbers was refused: %v", err)
	}

	if _, ok := arguments["n"].(float64); !ok {
		t.Errorf("the top-level number is %T, want float64", arguments["n"])
	}
	deep := arguments["body"].(map[string]any)["deep"].([]any)
	if _, ok := deep[0].(float64); !ok {
		t.Errorf("the nested number is %T, want float64", deep[0])
	}
	if deep[1] != "text" || deep[2] != nil {
		t.Errorf("the walk disturbed a non-number: %#v", deep)
	}
}

// TestThePrecisionRefusalIsItsOwnCode is the ISS-21 shape, checked on the byte
// a client switches on rather than on the Go value.
//
// errors.Is alone would still pass if the sentinel were right and codeFor's
// table had no row for it — which is exactly how a known, precise, permanent
// condition reached a client as "failed" the last time.
func TestThePrecisionRefusalIsItsOwnCode(t *testing.T) {
	err := exactArguments(map[string]any{"n": json.Number("9007199254740993")})
	if err == nil {
		t.Fatal("9007199254740993 was accepted")
	}
	if !errors.Is(err, ErrPrecision) {
		t.Errorf("errors.Is(err, ErrPrecision) is false for %v", err)
	}
	if got := codeFor(err); got != "precision" {
		t.Errorf("codeFor(%v) = %q, want %q", err, got, "precision")
	}
	if got := codeFor(err); got == "failed" || got == "argument" {
		t.Errorf("the refusal came back as %q", got)
	}
}

// TestTheExponentHoleIsKnownAndNotPretendedAway pins what this check does NOT
// catch, so that it is a recorded limit rather than a surprise.
//
// 9.007199254740993e15 is the same value as 9007199254740993, which is
// refused — and it is accepted, because the rule is about a literal written as
// a plain integer. That is deliberate: the shape of the literal is the only
// statement of intent that reaches this server, and a caller who writes an
// exponent has chosen float notation, exactly as one who writes 1.5e300 has.
// Widening the rule to every integer-valued literal would refuse 1.5e300 too,
// and that is a working caller.
//
// If this test ever goes red because somebody made the rule catch it, read the
// comment in number.go before deleting the test: the question is whether
// 1.5e300 is still accepted.
func TestTheExponentHoleIsKnownAndNotPretendedAway(t *testing.T) {
	if _, _, ok := carries(json.Number("9007199254740993")); ok {
		t.Fatal("the plain integer is no longer refused, which is the check itself")
	}
	if _, _, ok := carries(json.Number("9.007199254740993e15")); !ok {
		t.Error("the exponent form is now refused — see this test's comment, and check 1.5e300")
	}
	if _, _, ok := carries(json.Number("9007199254740993.0")); !ok {
		t.Error("the trailing-.0 form is now refused — see this test's comment, and check 1.5e300")
	}
	if _, _, ok := carries(json.Number("1.5e300")); !ok {
		t.Error("1.5e300 is refused, and it is printed back exactly as written")
	}
}
