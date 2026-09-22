package server

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// SAPE-13: the request body is the half of the wire contract fixtures/frames.json
// never pinned. invoke's own case carries a body, but a body that appears in
// one example is not a specification -- a client author reading only that
// example cannot tell which fields are required, what type each one is, or
// what happens to a field it invented. That gap is why the "op"/"command"
// mismatch (fixed for the fixture case in fixture_decode_test.go) was ever
// possible: the fixture had an example, not a schema, and nothing said the
// example was binding.
//
// fixtures/frames.json's requestBodies section is the schema. This file keeps
// it honest against the struct that actually decodes each frame -- generated
// from the same json tags reflect.StructTag already reads, not retyped by
// hand and left to drift the way the fixture itself once did. It is
// deliberately narrow: a field whose value is itself a named struct
// (store.Operation, store.Spec, store.Access) is described only as "object"
// here, not expanded field by field. Those three are already a promise
// internal/store keeps and tests on their own terms (see COMPATIBILITY.md's
// "Declaration semantics"), and restating their shape in a second file would
// be a second copy of a rule that already has one keeper -- which is the
// exact failure mode ("both sides agreed with itself") this ticket is about
// avoiding, not reproducing one level up.
//
// This test proves the artifact agrees with the struct it was read off of.
// It does NOT prove a client reading the artifact would agree with the
// server -- a schema copied faithfully from the wrong struct would pass this
// test and still be wrong. TestARequestBuiltFromTheArtifactIsWhatARealServerAccepts,
// below, is what measures that: it sends bytes built from nothing but the
// artifact's own field names to a real server on a real socket.

// fieldRule is one field the way requestBodies records it.
type fieldRule struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Shape    string `json:"shape,omitempty"`
}

type frameBody struct {
	Fields map[string]fieldRule `json:"fields"`
}

type requestBodiesArtifact struct {
	UnknownFieldPolicy string               `json:"unknownFieldPolicy"`
	Frames             map[string]frameBody `json:"frames"`
	Shapes             map[string]frameBody `json:"shapes"`
}

// readFrameFixture is the one place that names fixtures/frames.json's path,
// shared by everything in this package that reads the artifact raw.
func readFrameFixture(t *testing.T) ([]byte, error) {
	t.Helper()
	return os.ReadFile("../../fixtures/frames.json")
}

func loadRequestBodiesArtifact(t *testing.T) requestBodiesArtifact {
	t.Helper()
	raw, err := readFrameFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		RequestBodies requestBodiesArtifact `json:"requestBodies"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.RequestBodies
}

// describeFields reads the same information the artifact records straight off
// a struct's exported fields: the json tag's name, whether that tag carries
// omitempty, and a coarse category for the field's Go type. A field pointing
// at the grant struct is the one nested shape this reads by name rather than
// leaving opaque, because the artifact documents grant as a shape of its own
// (invoke.grant references it) rather than folding it into "object" the way
// store.Operation, store.Spec and store.Access are left.
func describeFields(t *testing.T, value any) map[string]fieldRule {
	t.Helper()
	fields := map[string]fieldRule{}
	rt := reflect.TypeOf(value)
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		tag, has := field.Tag.Lookup("json")
		if !has || tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			name = field.Name
		}
		required := true
		for _, option := range parts[1:] {
			if option == "omitempty" {
				required = false
			}
		}

		rule := fieldRule{Required: required}
		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			if fieldType.Elem().Kind() == reflect.Struct && fieldType.Elem().Name() == "grant" {
				rule.Type = "object"
				rule.Shape = "grant"
				fields[name] = rule
				continue
			}
			fieldType = fieldType.Elem()
		}

		switch fieldType.Kind() {
		case reflect.String:
			rule.Type = "string"
		case reflect.Bool:
			rule.Type = "boolean"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			rule.Type = "integer"
		case reflect.Slice, reflect.Array:
			rule.Type = "array"
		case reflect.Map, reflect.Struct, reflect.Interface:
			rule.Type = "object"
		default:
			t.Fatalf("field %q of %s has a Go kind (%s) this describer has no category for -- teach it one before trusting the comparison", name, rt.Name(), fieldType.Kind())
		}
		fields[name] = rule
	}
	return fields
}

// compareFieldSets is the two-way check: every field the artifact names must
// be on the struct with the same type, required-ness and shape, and every
// field the struct has must be named in the artifact. Either direction alone
// would let one side grow without the other noticing.
func compareFieldSets(t *testing.T, subject string, got, want map[string]fieldRule) {
	t.Helper()
	for name, rule := range want {
		gotRule, ok := got[name]
		if !ok {
			t.Errorf("%s: requestBodies names field %q, but the struct has no json tag by that name", subject, name)
			continue
		}
		if gotRule.Type != rule.Type {
			t.Errorf("%s.%s: requestBodies says type %q, the struct's field is %q", subject, name, rule.Type, gotRule.Type)
		}
		if gotRule.Required != rule.Required {
			t.Errorf("%s.%s: requestBodies says required=%v, the struct's omitempty tag says required=%v", subject, name, rule.Required, gotRule.Required)
		}
		if rule.Shape != "" && gotRule.Shape != rule.Shape {
			t.Errorf("%s.%s: requestBodies says shape %q, the struct points at %q", subject, name, rule.Shape, gotRule.Shape)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: the struct has a %q field with a json tag, requestBodies does not describe it", subject, name)
		}
	}
}

// TestFixtureRequestBodiesMatchTheStructsTheyDescribe is fixtures/frames.json's
// requestBodies checked against the structs it claims to describe, the way
// TestEveryRequestFixtureDecodesIntoTheStructThatServesIt checks `cases`
// against the same structs. Two different failure modes, so two different
// tests: that one catches an example that does not decode; this one catches a
// schema that says something the struct does not.
func TestFixtureRequestBodiesMatchTheStructsTheyDescribe(t *testing.T) {
	artifact := loadRequestBodiesArtifact(t)

	if artifact.UnknownFieldPolicy != "ignore" {
		t.Errorf(`requestBodies.unknownFieldPolicy = %q, want "ignore" -- measured fact: every request decode in this package uses json.Unmarshal or a json.Decoder on the same defaults for field names (invoke calls UseNumber, for ISS-35, which reads numbers differently and field names identically), none call DisallowUnknownFields`, artifact.UnknownFieldPolicy)
	}

	// Every request frame a client sends whose payload is JSON this package
	// decodes into a named struct. hello is included here even though
	// fixture_decode_test.go's table leaves it out -- that table is about the
	// running connection's frame switch, which starts after the handshake;
	// this one is about the struct, which exists before it.
	structs := map[string]any{
		"hello":     hello{},
		"invoke":    call{},
		"elevate":   elevating{},
		"explore":   exploring{},
		"declare":   declaring{},
		"establish": establishing{},
		"subscribe": subscribe{},
	}

	for frameName, sample := range structs {
		frameName, sample := frameName, sample
		t.Run(frameName, func(t *testing.T) {
			body, ok := artifact.Frames[frameName]
			if !ok {
				t.Fatalf("requestBodies.frames has no entry for %q, which %T decodes", frameName, sample)
			}
			compareFieldSets(t, frameName, describeFields(t, sample), body.Fields)
		})
	}

	// grant is not a frame type -- it has no entry in fixtures/frames.json's
	// `types` table -- it is a shape invoke.grant points at. Checked
	// separately from the loop above for exactly that reason.
	t.Run("grant shape", func(t *testing.T) {
		shape, ok := artifact.Shapes["grant"]
		if !ok {
			t.Fatal(`requestBodies.shapes has no entry for "grant", which invoke.grant points at`)
		}
		compareFieldSets(t, "grant", describeFields(t, grant{}), shape.Fields)
	})

	// The same "forgetting is invisible" trap fixture_decode_test.go guards
	// against: a frame named in the artifact that no struct here describes
	// would otherwise pass every check above by never being looked at.
	for name := range artifact.Frames {
		if _, ok := structs[name]; !ok {
			t.Errorf("requestBodies.frames names %q, which no struct in this test's table describes", name)
		}
	}
}
