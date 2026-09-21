package server

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/signing"
)

// SAPE-13's strongest test: TestFixtureRequestBodiesMatchTheStructsTheyDescribe
// (fixture_body_test.go) only proves the artifact agrees with the struct that
// generated it -- which is exactly the weakness a generated artifact has by
// construction, and exactly why the hand-written frame tables in the three
// clients exist in the first place. It would pass unchanged if every field in
// `call` were renamed to something wrong, as long as the artifact was
// regenerated from the same wrong struct.
//
// This file does not touch the structs at all. It builds a JSON body from
// nothing but the field names requestBodies.frames names, sends it to a real
// *Server over a real socket, and checks the server accepts it -- then renames
// one required field and checks the same server refuses it. That is the
// question a client author actually has: "if I spell this field the way the
// artifact says, does a real server take it?"

// renamed copies body with one key renamed -- the exact shape of the bug this
// ticket is about: a field present, correctly typed, just spelled wrong.
func renamed(body map[string]any, from, to string) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		if k == from {
			out[to] = v
			continue
		}
		out[k] = v
	}
	return out
}

func requireAccepted(t *testing.T, label string, frame protocol.Frame, want protocol.Type) {
	t.Helper()
	if frame.Type != want {
		t.Fatalf("%s: valid body was refused, not accepted: got %s %s, want %s", label, frame.Type, frame.Payload, want)
	}
}

func requireRefused(t *testing.T, label string, frame protocol.Frame) {
	t.Helper()
	if frame.Type != protocol.Failure {
		t.Errorf("%s: a body with a renamed required field was accepted: %s %s", label, frame.Type, frame.Payload)
	}
}

// TestARequestBuiltFromTheArtifactIsWhatARealServerAccepts is the round trip:
// for each request frame requestBodies describes, a body built purely from its
// field names is accepted by a real server, and the same body with one
// required field renamed is refused by the same server. Where a required
// field's zero value is itself meaningful (requestBodies says so with
// zeroValueIsMeaningful), renaming it does not manifest as a refusal -- that
// is measured separately, below, rather than asserted here as a false
// positive.
func TestARequestBuiltFromTheArtifactIsWhatARealServerAccepts(t *testing.T) {
	artifact := loadRequestBodiesArtifact(t)

	requiredFieldToRename := func(t *testing.T, frame string) string {
		t.Helper()
		body, ok := artifact.Frames[frame]
		if !ok {
			t.Fatalf("requestBodies.frames has no entry for %q", frame)
		}
		for name, rule := range body.Fields {
			if rule.Required && rule.Shape == "" {
				if _, meaningful := zeroValueMeaningfulFields[frame][name]; meaningful {
					continue
				}
				return name
			}
		}
		t.Fatalf("requestBodies.frames[%q] names no required field this test knows how to mutate", frame)
		return ""
	}

	t.Run("hello", func(t *testing.T) {
		server, address := running(t, false)
		signature, err := server.Sign("acme", password, "main")
		if err != nil {
			t.Fatal(err)
		}
		valid := map[string]any{"account": "acme", "password": password, "dbname": "main", "sig": signature, "mode": "bound"}

		good := dial(t, address)
		good.send(protocol.Hello, valid)
		requireAccepted(t, "hello", good.read(), protocol.Welcome)

		field := requiredFieldToRename(t, "hello")
		bad := dial(t, address)
		bad.send(protocol.Hello, renamed(valid, field, field+"_typo"))
		requireRefused(t, "hello."+field+" renamed", bad.read())
	})

	t.Run("invoke", func(t *testing.T) {
		server, address := running(t, false)
		declare(t, server, "acme", "main")

		c := dial(t, address)
		requireAccepted(t, "invoke handshake", c.open(server, "acme", "main"), protocol.Welcome)

		// This is exactly the historical shape of ISS's invoke bug: `command`
		// carries the operation name, and a client that spelled it any other
		// way passed every unit test that only checked itself.
		valid := map[string]any{"command": "articles.add", "args": map[string]any{"title": "one", "author": "ann"}}
		c.send(protocol.Invoke, valid)
		requireAccepted(t, "invoke", c.read(), protocol.Result)

		field := requiredFieldToRename(t, "invoke")
		c.send(protocol.Invoke, renamed(valid, field, field+"_typo"))
		requireRefused(t, "invoke."+field+" renamed", c.read())
	})

	t.Run("elevate", func(t *testing.T) {
		server, address := running(t, false)

		proofFor := func(t *testing.T, c *client) map[string]any {
			t.Helper()
			greeting := decode[welcome](t, c.open(server, "acme", "main"))
			challenge, err := hex.DecodeString(greeting.Challenge)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := signing.Operating(secret, challenge)
			if err != nil {
				t.Fatal(err)
			}
			return map[string]any{"proof": proof}
		}

		good := dial(t, address)
		good.send(protocol.Elevate, proofFor(t, good))
		requireAccepted(t, "elevate", good.read(), protocol.Result)

		field := requiredFieldToRename(t, "elevate")
		bad := dial(t, address)
		valid := proofFor(t, bad)
		bad.send(protocol.Elevate, renamed(valid, field, field+"_typo"))
		requireRefused(t, "elevate."+field+" renamed", bad.read())
	})

	t.Run("explore", func(t *testing.T) {
		server, address := running(t, false)
		declare(t, server, "acme", "main")

		c := dial(t, address)
		greeting := decode[welcome](t, c.open(server, "acme", "main"))
		requireAccepted(t, "explore operate", c.operate(greeting, secret), protocol.Result)

		valid := map[string]any{"access": map[string]any{"kind": "scan", "collection": "articles", "index": "by_author"}}
		c.send(protocol.Explore, valid)
		requireAccepted(t, "explore", c.read(), protocol.Result)

		field := requiredFieldToRename(t, "explore")
		c.send(protocol.Explore, renamed(valid, field, field+"_typo"))
		requireRefused(t, "explore."+field+" renamed", c.read())
	})

	t.Run("declare", func(t *testing.T) {
		server, address := running(t, false)
		declare(t, server, "acme", "main")

		c := dial(t, address)
		greeting := decode[welcome](t, c.open(server, "acme", "main"))
		requireAccepted(t, "declare operate", c.operate(greeting, secret), protocol.Result)

		valid := map[string]any{"operation": map[string]any{
			"name": "articles.built_from_artifact", "collection": "articles",
			"action": "scan", "index": "by_author", "limit": 10,
		}}
		c.send(protocol.Declare, valid)
		requireAccepted(t, "declare", c.read(), protocol.Result)

		field := requiredFieldToRename(t, "declare")
		c.send(protocol.Declare, renamed(valid, field, field+"_typo"))
		requireRefused(t, "declare."+field+" renamed", c.read())
	})

	t.Run("establish", func(t *testing.T) {
		server, address := running(t, false)
		declare(t, server, "acme", "main")

		c := dial(t, address)
		greeting := decode[welcome](t, c.open(server, "acme", "main"))
		requireAccepted(t, "establish operate", c.operate(greeting, secret), protocol.Result)

		valid := map[string]any{"spec": map[string]any{
			"name": "widgets",
			"key":  map[string]any{"path": "id", "type": "string", "auto": "ulid"},
		}}
		c.send(protocol.Establish, valid)
		requireAccepted(t, "establish", c.read(), protocol.Result)

		field := requiredFieldToRename(t, "establish")
		c.send(protocol.Establish, renamed(valid, field, field+"_typo"))
		requireRefused(t, "establish."+field+" renamed", c.read())
	})

	t.Run("subscribe", func(t *testing.T) {
		server, address := running(t, false)
		// A log with something in it, so a correct subscribe has somewhere
		// real to start from -- LSNs begin at 1, never 0.
		declare(t, server, "acme", "main")

		c := dial(t, address)
		requireAccepted(t, "subscribe handshake", c.open(server, "acme", "main"), protocol.Welcome)

		valid := map[string]any{"from": 1}
		c.send(protocol.Subscribe, valid)
		requireAccepted(t, "subscribe", c.read(), protocol.Result)

		field := requiredFieldToRename(t, "subscribe")
		bad := dial(t, address)
		requireAccepted(t, "subscribe handshake (mutation)", bad.open(server, "acme", "main"), protocol.Welcome)
		bad.send(protocol.Subscribe, renamed(valid, field, field+"_typo"))
		requireRefused(t, "subscribe."+field+" renamed", bad.read())
	})
}

// zeroValueMeaningfulFields mirrors requestBodies' own zeroValueIsMeaningful
// notes: required fields whose zero value is a legitimate request on its own,
// so renaming them is not expected to produce a refusal. Kept as Go rather
// than read from the fixture so a note added to the JSON without a matching
// entry here fails loudly in the test below, instead of quietly weakening the
// mutation table above.
//
// subscribe.from is NOT in this table, even though its zero value looked at
// first like "from the beginning of the log" the same way hello.mode's does.
// Measured and found false: store.CanFollow requires from > latest when
// nothing has been trimmed yet (oldest == 0) and from >= oldest otherwise --
// either way 0 only ever passes when latest is also negative, which it never
// is, because LSNs start at 1. So from=0 is refused on an empty database and
// a used one alike, and a renamed `from` is caught by the mutation test above
// like any ordinary required field.
var zeroValueMeaningfulFields = map[string]map[string]struct{}{
	"hello": {"dbname": {}, "mode": {}},
}

// TestZeroValueMeaningfulFieldsAreNotSilentlyOutOfDate makes sure the map
// above still matches what fixtures/frames.json claims: every field it marks
// zeroValueIsMeaningful must be in the map, and the map must not name a field
// the fixture no longer marks that way.
func TestZeroValueMeaningfulFieldsAreNotSilentlyOutOfDate(t *testing.T) {
	raw := struct {
		RequestBodies struct {
			Frames map[string]struct {
				Fields map[string]struct {
					Required              bool   `json:"required"`
					ZeroValueIsMeaningful string `json:"zeroValueIsMeaningful,omitempty"`
				} `json:"fields"`
			} `json:"frames"`
		} `json:"requestBodies"`
	}{}
	fromFile(t, &raw)

	for frame, body := range raw.RequestBodies.Frames {
		for field, rule := range body.Fields {
			_, tracked := zeroValueMeaningfulFields[frame][field]
			marked := rule.ZeroValueIsMeaningful != ""
			if marked != tracked {
				t.Errorf("%s.%s: fixture marks zeroValueIsMeaningful=%v, this test's table tracks it=%v -- keep them in sync", frame, field, marked, tracked)
			}
		}
	}
}

func fromFile(t *testing.T, into any) {
	t.Helper()
	raw, err := readFrameFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// TestSubscribeFromZeroIsRefusedWhetherOrNotTheLogIsEmpty pins the measurement
// behind zeroValueMeaningfulFields' comment on subscribe.from: the field
// looked at first like it might have a meaningful zero value the way
// hello.mode's does, and it does not. A renamed `from` decodes as 0, and 0 is
// refused in both states a database can be in -- LSNs start at 1, so there is
// no "beginning of the log" that 0 names.
func TestSubscribeFromZeroIsRefusedWhetherOrNotTheLogIsEmpty(t *testing.T) {
	server, address := running(t, false)

	empty := dial(t, address)
	requireAccepted(t, "subscribe handshake (empty log)", empty.open(server, "acme", "main"), protocol.Welcome)
	empty.send(protocol.Subscribe, map[string]any{"from": 0})
	if frame := empty.read(); frame.Type != protocol.Failure {
		t.Errorf("subscribe from=0 on an empty log was accepted instead of refused: %s %s", frame.Type, frame.Payload)
	}

	declare(t, server, "acme", "main")

	used := dial(t, address)
	requireAccepted(t, "subscribe handshake (non-empty log)", used.open(server, "acme", "main"), protocol.Welcome)
	used.send(protocol.Subscribe, map[string]any{"from": 0})
	if frame := used.read(); frame.Type != protocol.Failure {
		t.Errorf("subscribe from=0 on a non-empty log was accepted instead of refused: %s %s", frame.Type, frame.Payload)
	}
}
