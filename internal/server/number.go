package server

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/sapedb/sapedb/internal/number"
)

// The rule about a number that cannot be stored as sent lives in
// internal/number, and this file is how this package reaches it.
//
// It was written here, for the invoke frame, in ISS-35's first half. The
// second half found the same silence on every other door a declaration or a
// constant arrives through — `sapedb apply`, `sapedb install`, the `declare`,
// `establish` and `explore` frames, the operator shell — and internal/bundle
// and internal/cli cannot import this package. So the rule moved down to a
// leaf package every door can reach, and nothing about it changed: the
// boundary, the wording of the refusal, the sentinel and the code are the same
// ones, because they are now literally the same code. See internal/number for
// why the check can only live at the decode.

// ErrPrecision is the sentinel internal/number refuses with, re-exported under
// the name this package's callers and codeFor already use.
//
// It is the same error value, not a copy: errors.Is against either name
// answers for a refusal raised anywhere, and codeFor's row for it keeps
// answering "precision" whichever door refused.
var ErrPrecision = number.ErrPrecision

// carries is internal/number.Carries. Kept as a local name so that this
// package's tests, which are where the boundary is pinned on the wire, read
// against the same short spelling the rule was written under.
func carries(literal json.Number) (value float64, instead string, ok bool) {
	return number.Carries(literal)
}

// exactArguments is internal/number.ExactArguments: every number an invoke
// carries, checked, and replaced with the float64 the rest of the server has
// always been handed.
func exactArguments(arguments map[string]any) error {
	return number.ExactArguments(arguments)
}

// unrounded reads a frame payload without turning its numbers into float64s,
// so that the check that follows it has digits to look at.
//
// It is json.Unmarshal's behaviour with UseNumber on and nothing else changed.
// The More() check is the part that is easy to drop and would be a second bug:
// json.Unmarshal refuses trailing bytes and json.Decoder does not, so a
// payload holding two JSON objects would start reading as its first one the
// moment a frame stopped using Unmarshal. Every caller here used Unmarshal
// before ISS-35, and none of them is being loosened by it.
//
// The decode error comes back bare. Each frame says in its own words what did
// not read — "the declaration", "the access" — and flattening those into one
// sentence here would make every refusal say "payload".
func unrounded(payload []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("it holds more than one value")
	}
	return nil
}
