package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
)

// TestEveryRequestFixtureDecodesIntoTheStructThatServesIt is the general form
// of a guard that existed for exactly one frame, which is why the others were
// free to drift.
//
// The declare case had TestTheDeclareFixtureIsTheStructTheServerDecodes. The
// invoke case had nothing, and it drifted: fixtures/frames.json carried
// {"op": "orders.list", ...} while call's field has always been
// `json:"command"`. Encoding and decoding that case round-tripped perfectly on
// both sides of the wire, because both sides were reading the same wrong file.
// The fixture is published as the conformance specification other languages
// build against, so the mistake was on its way out to them.
//
// A table rather than a test per frame, because the failure was never "this
// one case is wrong". It was "nobody checks this one case", and a table makes
// forgetting visible: a request frame with no decoder here fails the last
// assertion by name.
func TestEveryRequestFixtureDecodesIntoTheStructThatServesIt(t *testing.T) {
	// Every frame type a client sends that carries a body the server decodes.
	// hello is handled by the handshake before this table's world begins, and
	// the rest (welcome, result, failure, event, pong, goodbye) travel the
	// other way.
	decoders := map[protocol.Type]func(*json.Decoder) error{
		protocol.Invoke: func(d *json.Decoder) error {
			asked := call{}
			if err := d.Decode(&asked); err != nil {
				return err
			}
			if asked.Command == "" {
				return errEmpty("command")
			}
			return nil
		},
		protocol.Elevate: func(d *json.Decoder) error {
			asked := elevating{}
			return d.Decode(&asked)
		},
		protocol.Explore: func(d *json.Decoder) error {
			asked := exploring{}
			return d.Decode(&asked)
		},
		protocol.Declare: func(d *json.Decoder) error {
			asked := declaring{}
			if err := d.Decode(&asked); err != nil {
				return err
			}
			if asked.Operation.Name == "" {
				return errEmpty("operation.name")
			}
			return nil
		},
		protocol.Establish: func(d *json.Decoder) error {
			asked := establishing{}
			if err := d.Decode(&asked); err != nil {
				return err
			}
			if asked.Spec.Name == "" {
				return errEmpty("spec.name")
			}
			return nil
		},
		// SAPE-13: subscribe was missing from this table -- not excluded on
		// purpose like hello below, just never added, which is exactly the
		// kind of gap this table exists to make loud instead of silent.
		// Nothing here checks From for emptiness: 0 is its valid zero value
		// ("from the beginning of the log"), not a sign the field was
		// misnamed -- see fixtures/frames.json's requestBodies note.
		protocol.Subscribe: func(d *json.Decoder) error {
			asked := subscribe{}
			return d.Decode(&asked)
		},
	}

	raw, err := os.ReadFile("../../fixtures/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		Types map[string]uint8 `json:"types"`
		Cases []struct {
			Name string          `json:"name"`
			Type uint8           `json:"type"`
			JSON json.RawMessage `json:"json"`
		} `json:"cases"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	measured := map[protocol.Type]int{}
	for _, one := range fixture.Cases {
		decode, mine := decoders[protocol.Type(one.Type)]
		if !mine || len(one.JSON) == 0 {
			continue
		}
		measured[protocol.Type(one.Type)]++

		decoder := json.NewDecoder(strings.NewReader(string(one.JSON)))
		// The strict decode is the whole point. Without it a field the server
		// does not have is dropped in silence, which is how "op" survived
		// beside a struct that reads "command".
		decoder.DisallowUnknownFields()
		if err := decode(decoder); err != nil {
			t.Errorf("%s: the fixture does not decode as the struct that serves this frame: %v", one.Name, err)
		}
	}

	// Without this, a fixture holding no case for a frame passes the loop
	// above by having nothing to fail on -- the empty-set answer that looks
	// exactly like agreement.
	for frame := range decoders {
		if measured[frame] == 0 {
			t.Errorf("no fixture case carries a body for frame %v, so nothing here measured it", frame)
		}
	}
}

type errEmpty string

func (e errEmpty) Error() string {
	return "decoded into an empty " + string(e) + ", so the case names a field this struct does not read"
}
