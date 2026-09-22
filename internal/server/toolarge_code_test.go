package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAnAnswerThatDoesNotFitHasACodeOfItsOwn is ISS-38 at this layer: the
// refusal has a word a client can switch on, and it is not one of the words
// that already existed.
//
// The three it is measured against are the three it would plausibly have been
// folded into, and each would tell its reader to do the wrong thing.
// "ceiling" is the declaration's own row limit, and its reader decides
// whether to pay for a longer walk — but no limit this caller can declare
// raises the server's budget. "argument" says the values passed were wrong,
// and they were not: the identical call on a smaller collection answers.
// "failed" is the one ISS-21 exists to record, and says nothing at all.
func TestAnAnswerThatDoesNotFitHasACodeOfItsOwn(t *testing.T) {
	if got := codeFor(store.ErrTooLarge); got != "too_large" {
		t.Fatalf("codeFor(store.ErrTooLarge) = %q, want %q", got, "too_large")
	}
	for _, other := range []struct {
		err  error
		code string
	}{
		{store.ErrCeiling, "ceiling"},
		{store.ErrArgument, "argument"},
	} {
		if codeFor(other.err) != other.code {
			t.Fatalf("codeFor(%v) = %q, and this test is comparing against the wrong thing", other.err, codeFor(other.err))
		}
		if codeFor(store.ErrTooLarge) == other.code {
			t.Fatalf("an answer that does not fit arrives under the same code as %q", other.code)
		}
	}

	// And it reaches a client as a payload with that code in it, not as prose.
	body := failure(store.ErrTooLarge)
	said := struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}{}
	if err := json.Unmarshal(body, &said); err != nil {
		t.Fatalf("the refusal does not read as JSON: %v", err)
	}
	if said.Code != "too_large" {
		t.Fatalf("the refusal carries code %q", said.Code)
	}
	if said.Message == "" {
		t.Fatal("the refusal carries no message beside its code")
	}
}

// TestAResultOverTheFrameCapIsSentAsARefusalAndNotAsASilence measures the
// backstop, which is the half of ISS-38 that does not depend on the budget.
//
// The budget in internal/store refuses before an oversized answer is built,
// and it counts the rows; an answer also carries an envelope around them. So
// a reply can still arrive here a few dozen bytes over the cap, and before
// this it would have been protocol.Encode returning an error, write()
// passing it up and Handle closing the connection — a bare EOF, which is what
// the benchmark rig had to redial to tell from a dead server.
//
// Driven through answer() with a payload built by hand, because the thing
// under test is what happens to a reply that does not fit and not how one
// gets that large.
func TestAResultOverTheFrameCapIsSentAsARefusalAndNotAsASilence(t *testing.T) {
	held := &bytes.Buffer{}
	out := &sender{conn: held}

	oversized := bytes.Repeat([]byte("x"), protocol.MaxPayload+1)
	if err := answer(out, 7, oversized); err != nil {
		t.Fatalf("a reply over the cap came back as an error rather than as a refusal on the wire: %v", err)
	}

	frame, err := protocol.NewReader(held).Read()
	if err != nil {
		t.Fatalf("nothing readable was written: %v", err)
	}
	if frame.Type != protocol.Failure {
		t.Fatalf("a reply over the cap was sent as a %s frame", frame.Type)
	}
	if frame.ID != 7 {
		t.Fatalf("the refusal answers request %d, not the 7 that asked", frame.ID)
	}

	said := struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}{}
	if err := json.Unmarshal(frame.Payload, &said); err != nil {
		t.Fatalf("the refusal does not read as JSON: %v", err)
	}
	if said.Code != "too_large" {
		t.Fatalf("the refusal carries code %q, want %q", said.Code, "too_large")
	}
	if !strings.Contains(said.Message, "one reply may carry") {
		t.Fatalf("the refusal reads %q and does not say what the limit was", said.Message)
	}

	// The control: a reply that fits is still a Result, so the check above is
	// a boundary and not a server that refuses everything.
	held.Reset()
	if err := answer(out, 8, []byte(`{"rows":[]}`)); err != nil {
		t.Fatal(err)
	}
	fits, err := protocol.NewReader(held).Read()
	if err != nil {
		t.Fatal(err)
	}
	if fits.Type != protocol.Result {
		t.Fatalf("an ordinary reply was sent as a %s frame", fits.Type)
	}
}
