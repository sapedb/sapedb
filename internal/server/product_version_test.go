package server

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/build"
	"github.com/sapedb/sapedb/internal/protocol"
)

// stampedForTest is what the servers below are told to call themselves. It is
// written out here by hand, and it is deliberately not anything build.Version
// could ever hold — not the "dev" default, not a tag. Asserting the welcome
// against build.Version instead would be asserting a value against the
// variable that produced it, which agrees with itself even when the field is
// never filled in at all.
const stampedForTest = "0.0.0-a-version-nothing-else-in-this-tree-spells"

// runningAs is running(), with a product version this test chose.
func runningAs(t *testing.T, version string) (*Server, string) {
	t.Helper()

	server, err := New(Options{Dir: t.TempDir(), Secret: secret, ProductVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()

	return server, listener.Addr().String()
}

// TestTheWelcomeCarriesTheProductVersion is the frame half of the change: a
// client that has only just connected, and has proved nothing and asked for
// nothing, already knows which build it is talking to.
//
// The protocol version is checked alongside it, and not because it is at
// risk. It is the control that gives the other assertion meaning: these are
// two different numbers answering two different questions, and before this
// field the welcome carried only the one that never moves — which is why two
// builds six months apart introduced themselves identically.
func TestTheWelcomeCarriesTheProductVersion(t *testing.T) {
	server, address := runningAs(t, stampedForTest)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	frame := client.open(server, "acme", "main")
	if frame.Type != protocol.Welcome {
		t.Fatalf("the handshake answered with a %s: %s", frame.Type, frame.Payload)
	}

	greeting := decode[welcome](t, frame)
	if greeting.ProductVersion != stampedForTest {
		t.Errorf("the welcome says the product version is %q, want %q", greeting.ProductVersion, stampedForTest)
	}
	if greeting.Version != protocol.Version {
		t.Errorf("the welcome says the protocol version is %d, want %d", greeting.Version, protocol.Version)
	}

	// The raw payload, not the struct: a field that is spelled differently on
	// the wire than in Go would satisfy every assertion above and still be a
	// different contract from the one other languages are being asked to
	// implement. The name is the thing being fixed, so the name is what is
	// pinned, written out here rather than taken from the struct tag.
	if !strings.Contains(string(frame.Payload), `"productVersion":"`+stampedForTest+`"`) {
		t.Errorf("the welcome on the wire does not carry productVersion as written: %s", frame.Payload)
	}
}

// TestAServerThatWasToldNothingStillNamesItsBuild is the second half of the
// default in New. A welcome that omits this field is indistinguishable from a
// server too old to have it, so there must be no way to start a server that
// sends it empty — including the way every server in this repository's own
// tests is started, which passes no ProductVersion at all.
func TestAServerThatWasToldNothingStillNamesItsBuild(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))

	if greeting.ProductVersion == "" {
		t.Fatal("a server started without a product version sent an empty one, which a client cannot tell from a server too old to send one at all")
	}
	// Compared against build.Version on purpose here, and only here: the
	// claim being measured is "New falls back to that variable", so that
	// variable is the right-hand side. It is not a claim about the value,
	// which is what stampedForTest above exists to pin independently.
	if greeting.ProductVersion != build.Version {
		t.Errorf("a server told nothing says %q, want the linked-in %q", greeting.ProductVersion, build.Version)
	}
}

// welcomeBeforeTheProductVersion is the welcome struct exactly as it was
// before this field was added — every field, spelled the way it was spelled,
// and nothing else.
//
// It is a copy rather than a reference to anything real, because the thing it
// stands for no longer exists in this tree: it is the client that is already
// out there, compiled against the old shape, which cannot be recompiled by
// anybody here. The rule for 1.0.0 is that a field may be added and never
// renamed or removed, and the promise that rule makes is exactly this: such a
// client keeps working. A promise nobody measures is a promise nobody keeps.
type welcomeBeforeTheProductVersion struct {
	Version   uint8  `json:"version"`
	Account   string `json:"account"`
	DBName    string `json:"dbname,omitempty"`
	Mode      string `json:"mode"`
	LSN       uint64 `json:"lsn,omitempty"`
	Encrypted bool   `json:"encrypted"`
	Challenge string `json:"challenge,omitempty"`
}

// TestAClientThatPredatesTheProductVersionStillReadsTheWelcome is the
// wire-change guard. The new field must be invisible to a client built before
// it existed — not merely "probably fine because JSON", which is a reason and
// not a measurement.
func TestAClientThatPredatesTheProductVersionStillReadsTheWelcome(t *testing.T) {
	server, address := runningAs(t, stampedForTest)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	frame := client.open(server, "acme", "main")
	if frame.Type != protocol.Welcome {
		t.Fatalf("the handshake answered with a %s: %s", frame.Type, frame.Payload)
	}

	// The control, first: the payload really does carry the new field. Decode
	// an old struct from a welcome that never had the field in it and it
	// passes for the wrong reason — there was nothing to survive.
	if !strings.Contains(string(frame.Payload), `"productVersion"`) {
		t.Fatalf("this welcome carries no productVersion, so nothing below would be measuring a client surviving one: %s", frame.Payload)
	}

	old := welcomeBeforeTheProductVersion{}
	if err := json.Unmarshal(frame.Payload, &old); err != nil {
		t.Fatalf("a client from before this field cannot read the welcome any more: %v", err)
	}

	// Surviving is not enough; it has to still get everything it came for.
	if old.Version != protocol.Version {
		t.Errorf("the old client reads the protocol version as %d, want %d", old.Version, protocol.Version)
	}
	if old.Account != "acme" || old.DBName != "main" {
		t.Errorf("the old client reads account %q database %q, want acme/main", old.Account, old.DBName)
	}
	if old.Mode != ModeBound {
		t.Errorf("the old client reads mode %q, want %q", old.Mode, ModeBound)
	}
	if old.Challenge == "" {
		t.Error("the old client got no challenge, so it could no longer become an operator")
	}
}
