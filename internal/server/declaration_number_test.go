package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// keyedByNumber declares a collection whose PRIMARY KEY is a number, which is
// the shape a `get` at the operator shell needs: ISS-35's identifiers — a
// Snowflake id, an account number — are exactly the things people key a
// collection on.
func keyedByNumber(t *testing.T, server *Server, account, name string) {
	t.Helper()

	db, release, err := server.Store(account, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "keyed",
		Key:  store.Key{Path: "id", Type: store.TypeNumber},
		Indexes: []store.Index{{
			Name:   "by_n",
			Fields: []store.Field{{Path: "n", Type: store.TypeNumber, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}
}

// sendRaw writes a payload exactly as given, without marshalling it — the only
// way to put two JSON documents in one frame, which is what the trailing-bytes
// control below needs.
func (c *client) sendRaw(kind protocol.Type, payload string) {
	c.t.Helper()

	c.next++
	frame, err := protocol.Encode(protocol.Frame{
		Version: protocol.Version, Type: kind, ID: c.next, Payload: []byte(payload),
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatal(err)
	}
}

// ISS-35's second half, on the three frames that carry a declaration or a
// constant rather than a call.
//
// The first half closed `invoke`. Declare, Establish and Explore do not pass
// it: each has its own handler and each used json.Unmarshal, so a constant
// written into an operation — `{"value": 9007199254740993}` — or a key typed
// at the operator shell became a float64 before anything could look at it.
//
// Every value below is sent as json.Number so the digits reach the socket as
// typed. A float64 in these tables would round in the test before the server
// saw it, which is the bug, and would leave them green against a server that
// does nothing.

// refusal is the body of a Failure frame: the sentence and the code a client
// switches on.
type refusal struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// operating opens a connection and proves the server secret, which is what
// all three of these frames require.
func operating(t *testing.T, server *Server, address string) *client {
	t.Helper()

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}
	return client
}

// TestADeclarationOverTheWireRefusesAConstantItCannotStore is the Declare
// frame, which is how a running server is given new vocabulary — and how an
// External Operation module installs itself without anybody stopping the
// database.
//
// A rounded constant here is worse than a rounded argument: it is stored once,
// in the catalogue, and then written again by every call that ever runs the
// operation.
func TestADeclarationOverTheWireRefusesAConstantItCannotStore(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")
	client := operating(t, server, address)

	frame := client.declare(store.Operation{
		Name: "ids.fixed", Collection: "ids", Action: store.ActionInsert,
		Document: map[string]store.Term{"n": {Value: json.Number("9007199254740993"), Constant: true}},
	})
	if frame.Type != protocol.Failure {
		t.Fatalf("the declaration was stored rather than refused: %s %s", frame.Type, frame.Payload)
	}

	said := decode[refusal](t, frame)
	if said.Code != "precision" {
		t.Errorf("the refusal reached the client as %q, want %q", said.Code, "precision")
	}
	if !strings.Contains(said.Message, "9007199254740993") || !strings.Contains(said.Message, "9007199254740992") {
		t.Errorf("the refusal does not name the value and what would have been stored: %q", said.Message)
	}
	if !strings.Contains(said.Message, `operation "ids.fixed".document.n.value`) {
		t.Errorf("the refusal does not say which constant it was: %q", said.Message)
	}
}

// TestTheDeclareBoundaryIsRepresentabilityNotMagnitude pins both sides on this
// frame. The accepted rows are not padding: a handler that refused every
// integer above 2^53 would pass a table of refusals and would break callers
// whose numbers always round-tripped.
func TestTheDeclareBoundaryIsRepresentabilityNotMagnitude(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")
	client := operating(t, server, address)

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
			frame := client.declare(store.Operation{
				Name: "ids.fixed", Collection: "ids", Action: store.ActionInsert,
				Document: map[string]store.Term{"n": {Value: json.Number(one.sent), Constant: true}},
			})

			if one.refused {
				if frame.Type != protocol.Failure {
					t.Fatalf("%s (%s) was declared rather than refused: %s", one.sent, one.why, frame.Payload)
				}
				if code := decode[refusal](t, frame).Code; code != "precision" {
					t.Errorf("%s was refused as %q, want %q", one.sent, code, "precision")
				}
				return
			}
			if frame.Type != protocol.Result {
				t.Fatalf("%s (%s) was refused: %s", one.sent, one.why, frame.Payload)
			}
			// And it is stored as a float64: the answer echoes the
			// declaration as the catalogue now holds it.
			stored := decode[declared](t, frame)
			if _, ok := stored.Operation.Document["n"].Value.(float64); !ok {
				t.Errorf("the stored constant is %T, want float64", stored.Operation.Document["n"].Value)
			}
		})
	}
}

// TestATypedAccessRefusesAKeyItCannotStore is the Explore frame, which is the
// operator shell.
//
// This is the worst-looking shape of ISS-35, because it answers. A `get` on
// 9007199254740993 silently became a `get` on 9007199254740992, which either
// says "nothing" for a document that exists or hands back the row stored under
// the neighbouring key — somebody else's row, printed at an operator's
// terminal with nothing to say it is the wrong one.
func TestATypedAccessRefusesAKeyItCannotStore(t *testing.T) {
	server, address := running(t, false)
	keyedByNumber(t, server, "acme", "main")
	client := operating(t, server, address)

	frame := client.explore(store.Access{
		Kind: "get", Collection: "keyed", Key: json.Number("9007199254740993"),
	})
	if frame.Type != protocol.Failure {
		t.Fatalf("the access ran rather than being refused: %s %s", frame.Type, frame.Payload)
	}
	said := decode[refusal](t, frame)
	if said.Code != "precision" {
		t.Errorf("the refusal reached the shell as %q, want %q", said.Code, "precision")
	}
	if !strings.Contains(said.Message, "9007199254740993") || !strings.Contains(said.Message, "9007199254740992") {
		t.Errorf("the refusal does not name the value and what would have been stored: %q", said.Message)
	}
	if !strings.Contains(said.Message, `access "get".key`) {
		t.Errorf("the refusal does not say which part of the access it was: %q", said.Message)
	}
}

// TestAScansBoundsAreCheckedTooAndTheBoundaryIsPinnedBothWays covers the other
// place a typed access carries a constant, and pins the boundary on this frame.
//
// A bound is where the shape of the walk is easiest to get wrong: `scan ids
// from 9007199254740993` used to start the walk one document early, silently,
// and the rows that came back were a different answer to a different question.
func TestAScansBoundsAreCheckedTooAndTheBoundaryIsPinnedBothWays(t *testing.T) {
	server, address := running(t, false)
	keyedByNumber(t, server, "acme", "main")
	client := operating(t, server, address)

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
		{"1.5e300", false, "integer-valued, written as a float, printed back as written"},
		{"0.1", false, "a fraction float64 does not hold — and never has, for anybody"},
		{"42", false, "an ordinary integer"},
	} {
		t.Run(one.sent, func(t *testing.T) {
			frame := client.explore(store.Access{
				Kind: "scan", Collection: "keyed", Index: "by_n", Limit: 5,
				From: &store.Bound{Values: []any{json.Number(one.sent)}},
			})

			if one.refused {
				if frame.Type != protocol.Failure {
					t.Fatalf("%s (%s) was walked rather than refused: %s", one.sent, one.why, frame.Payload)
				}
				said := decode[refusal](t, frame)
				if said.Code != "precision" {
					t.Errorf("%s was refused as %q, want %q", one.sent, said.Code, "precision")
				}
				if !strings.Contains(said.Message, `access "scan".from.values[0]`) {
					t.Errorf("the refusal does not say which value of which bound: %q", said.Message)
				}
				return
			}
			if frame.Type != protocol.Result {
				t.Fatalf("%s (%s) was refused: %s", one.sent, one.why, frame.Payload)
			}
		})
	}
}

// TestTheDraftAnExploreHandsBackCannotCarryARoundedConstant is the half of the
// Explore hole that outlives the session.
//
// An explore answers with the operation the access would have to be declared
// as, for the operator to paste into a schema file. So an access that rounded
// also handed back a declaration that rounded — `{"value": 9007199254740992}`
// for a key typed as 9007199254740993 — and the bug went into the repository
// with a human's name on the commit. There is now no draft to copy, because
// there is no access.
func TestTheDraftAnExploreHandsBackCannotCarryARoundedConstant(t *testing.T) {
	server, address := running(t, false)
	keyedByNumber(t, server, "acme", "main")
	client := operating(t, server, address)

	frame := client.explore(store.Access{
		Kind: "get", Collection: "keyed", Key: json.Number("9007199254740993"),
	})
	if frame.Type != protocol.Failure {
		answer := decode[explored](t, frame)
		t.Fatalf("a draft came back for a key that cannot be stored as sent: %+v", answer.Draft)
	}

	// The control: an access whose key survives still produces its draft, and
	// the draft carries the number that was typed.
	frame = client.explore(store.Access{
		Kind: "get", Collection: "keyed", Key: json.Number("9007199254740994"),
	})
	if frame.Type != protocol.Result {
		t.Fatalf("a key that round-trips was refused: %s", frame.Payload)
	}
	draft := decode[explored](t, frame).Draft
	if draft.Key == nil || draft.Key.Value != float64(9007199254740994) {
		t.Fatalf("the draft carries %v, want 9007199254740994 as a float64", draft.Key)
	}
}

// TestEstablishingACollectionIsWalkedEvenThoughASpecCarriesNoConstantToday is
// the honest version of a check that finds nothing.
//
// A store.Spec has no `any` field: a key path, an index field, a rollup group
// and a partition are all typed, so there is no constant in a collection
// declaration for this walk to refuse, and this test says so out loud rather
// than leaving a reader to wonder whether Establish was forgotten.
//
// The walk is there anyway, because it is over the shape rather than over a
// list of field names. The day a collection gains a declared default, it is
// covered without anybody remembering this file exists — and the failure mode
// of remembering is silent, which is the failure ISS-35 is about.
//
// What is measured is that Establish still works with the decoder changed
// under it, which is not nothing: it now reads with UseNumber, and a typed
// field that decoded through float64 before still has to arrive intact.
func TestEstablishingACollectionIsWalkedEvenThoughASpecCarriesNoConstantToday(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")
	client := operating(t, server, address)

	frame := client.establish(store.Spec{
		Name: "counted",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_n",
			Fields: []store.Field{{Path: "n", Type: store.TypeNumber, Descending: true, Missing: store.MissingLast}},
		}},
		Rollups: []store.Rollup{{Name: "all", Count: true, Sum: []string{"n"}}},
	})
	if frame.Type != protocol.Result {
		t.Fatalf("an ordinary collection was refused: %s %s", frame.Type, frame.Payload)
	}

	// The numbers in a Spec are typed fields — ids, and the flags on an index
	// — and every one of them has to have survived the decoder change.
	got := decode[established](t, frame).Spec
	if len(got.Indexes) != 1 || !got.Indexes[0].Fields[0].Descending ||
		got.Indexes[0].Fields[0].Missing != store.MissingLast {
		t.Errorf("the index came back as %+v", got.Indexes)
	}
	if len(got.Rollups) != 1 || !got.Rollups[0].Count || len(got.Rollups[0].Sum) != 1 {
		t.Errorf("the rollup came back as %+v", got.Rollups)
	}
	if got.ID == 0 || got.NextIndexID == 0 {
		t.Errorf("the ids the store assigns came back as %d/%d", got.ID, got.NextIndexID)
	}
}

// TestTheseFramesStillRefuseTwoDocumentsInOnePayload is the control for
// swapping json.Unmarshal for a json.Decoder.
//
// Unmarshal refuses trailing bytes; a Decoder does not. Without the explicit
// check each of these frames would have started reading a payload holding two
// JSON objects as its first one — a request that means something different
// from what was sent, accepted silently, which is the shape of the bug being
// fixed rather than a new one to introduce while fixing it.
func TestTheseFramesStillRefuseTwoDocumentsInOnePayload(t *testing.T) {
	server, address := running(t, false)
	declareIDs(t, server, "acme", "main")
	client := operating(t, server, address)

	for _, one := range []struct {
		frame   protocol.Type
		payload string
	}{
		{protocol.Declare, `{"operation":{"name":"a.b","collection":"ids","action":"get"}} {"operation":{}}`},
		{protocol.Establish, `{"spec":{"name":"ids","key":{"path":"id","type":"string"}}} {"spec":{}}`},
		{protocol.Explore, `{"catalogue":true} {"catalogue":true}`},
	} {
		t.Run(one.frame.String(), func(t *testing.T) {
			client.sendRaw(one.frame, one.payload)
			if frame := client.read(); frame.Type != protocol.Failure {
				t.Fatalf("a payload holding two documents was read as its first one: %s %s", frame.Type, frame.Payload)
			}
		})
	}
}
