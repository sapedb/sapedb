package server

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

func (c *client) declare(operation store.Operation) protocol.Frame {
	c.t.Helper()
	c.send(protocol.Declare, declaring{Operation: operation})
	return c.read()
}

// reason is the message of a Failure frame.
func reason(t *testing.T, frame protocol.Frame) string {
	t.Helper()
	if frame.Type != protocol.Failure {
		t.Fatalf("wanted a refusal and got a %s: %s", frame.Type, frame.Payload)
	}
	return decode[struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}](t, frame).Message
}

// TestAScanDeclaredOverTheWireMustSayHowManyRowsItMayReturn is the rule this
// frame was most at risk of quietly dropping.
//
// internal/store's Explore says it checks a typed access "with the same
// validation a declaration gets". For this one rule that has never been true:
// asOperation caps Limit at MostRows when it is missing, BEFORE
// validateOperation runs, so "a scan must declare how many rows it may return"
// cannot fire through Explore — no matter what an operator types. The comment
// promises a mechanism the code does not have.
//
// Which is the exact mistake a second path into the engine invites. So this
// test measures both paths side by side on one server: the same limitless scan
// is refused by Declare, in the store's own words, and accepted by Explore
// with a limit nobody asked for. The asymmetry is deliberate — a shell caps,
// a declaration must not, because a declaration is a promise about cost that
// somebody else has to keep — and it is pinned here so that "Declare
// validates" stays a measurement rather than a sentence in a doc comment.
func TestAScanDeclaredOverTheWireMustSayHowManyRowsItMayReturn(t *testing.T) {
	const rule = "sapedb/store: the declaration does not make sense: a scan must declare how many rows it may return"

	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// The control that makes the refusal below mean something: the same
	// declaration WITH a limit goes through. Without this, a refusal could be
	// coming from anywhere in the frame — a bad payload, a wrong database, a
	// handler that refuses everything.
	withLimit := store.Operation{
		Name: "articles.recent", Collection: "articles", Action: store.ActionScan,
		Index: "by_author", Limit: 10,
	}
	frame := client.declare(withLimit)
	if frame.Type != protocol.Result {
		t.Fatalf("a scan that declares its limit was refused: %s", frame.Payload)
	}
	if got := decode[declared](t, frame).Operation.Version; got != 1 {
		t.Fatalf("the first version of a new name came back as %d", got)
	}

	// The same declaration with the limit taken out.
	limitless := withLimit
	limitless.Limit = 0
	if got := reason(t, client.declare(limitless)); got != rule {
		t.Fatalf("a limitless scan was refused with\n got  %q\n want %q", got, rule)
	}

	// And it is refused by the rule, not by a fluke of the name being taken:
	// a name nobody has used is refused with the same words.
	limitless.Name = "articles.nobody_has_declared_this"
	if got := reason(t, client.declare(limitless)); got != rule {
		t.Fatalf("a limitless scan under a fresh name was refused with\n got  %q\n want %q", got, rule)
	}

	// Explore, for contrast: the same shape as a typed access is accepted and
	// silently given store.MostRows. This is not a bug being pinned as
	// correct — it is the reason Declare could not simply reuse Explore's way
	// in, recorded where the next person to widen this frame will read it.
	client.send(protocol.Explore, exploring{Access: store.Access{
		Kind: "scan", Collection: "articles", Index: "by_author",
	}})
	explore := client.read()
	if explore.Type != protocol.Result {
		t.Fatalf("Explore refused a limitless scan, so this test's premise is stale: %s", explore.Payload)
	}
	if got := decode[explored](t, explore).Draft.Limit; got != store.MostRows {
		t.Fatalf("Explore's draft limit came back %d, want the %d cap it quietly applies", got, store.MostRows)
	}
}

// TestDeclaringNeedsMoreThanAConnectionString: a connection string says which
// database to reach, never what may be declared once there. Declare writes to
// the catalogue — the one thing in this database that decides what everybody
// else may do — so it is gated exactly as Explore is, and by the same proof.
func TestDeclaringNeedsMoreThanAConnectionString(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	good := store.Operation{
		Name: "articles.recent", Collection: "articles", Action: store.ActionScan,
		Index: "by_author", Limit: 10,
	}

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))

	frame := client.declare(good)
	if frame.Type != protocol.Failure {
		t.Fatalf("an ordinary connection declared an operation: %s %s", frame.Type, frame.Payload)
	}
	if code := decode[struct {
		Code string `json:"code"`
	}](t, frame).Code; code != "not_operator" {
		t.Errorf("refused with code %q, want not_operator", code)
	}

	// Nothing was stored by the attempt.
	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, found, _ := db.Operation(good.Name, 0)
	release()
	if found {
		t.Fatal("a refused declaration was stored anyway")
	}

	// The control: the same frame, on the same connection, after the proof.
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}
	if frame := client.declare(good); frame.Type != protocol.Result {
		t.Fatalf("an operator was refused: %s", frame.Payload)
	}
}

// TestDeclaringTheSameNameAgainIsANewVersionAndTheOldOneStillRuns pins
// store.DeclareOperation's own behaviour as reached over the wire: a
// redeclaration adds a version, it does not replace one. Everything invoked
// between the two declarations keeps working, which is the point of versioning
// a declaration at all — and it is only true over the wire if this frame
// passes the operation through untouched.
func TestDeclaringTheSameNameAgainIsANewVersionAndTheOldOneStillRuns(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}
	for _, author := range []string{"ann", "bo", "cy"} {
		if frame := client.invoke("articles.add", map[string]any{"title": "t", "author": author}); frame.Type != protocol.Result {
			t.Fatalf("writing something to read: %s", frame.Payload)
		}
	}

	one := store.Operation{
		Name: "articles.recent", Collection: "articles", Action: store.ActionScan,
		Index: "by_author", Limit: 1,
	}
	first := decode[declared](t, client.declare(one)).Operation
	if first.Version != 1 {
		t.Fatalf("a new name came back at version %d, want 1", first.Version)
	}

	two := one
	two.Limit = 3
	second := decode[declared](t, client.declare(two)).Operation
	if second.Version != 2 {
		t.Fatalf("a redeclaration came back at version %d, want 2", second.Version)
	}

	// The latest is what an Invoke that names no version runs.
	if got := decode[store.Result](t, client.invoke("articles.recent", nil)).Count; got != 3 {
		t.Fatalf("the unversioned call returned %d rows, want the 3 the newest version declares", got)
	}

	// And version 1 is still there, still the one-row scan it was declared as.
	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	old, found, err := db.Operation("articles.recent", 1)
	if err != nil || !found {
		t.Fatalf("version 1 is gone after a redeclaration: found=%v err=%v", found, err)
	}
	if old.Limit != 1 {
		t.Fatalf("version 1 now declares a limit of %d, want the 1 it was declared with", old.Limit)
	}
}

// TestTheDeclareFixtureIsTheStructTheServerDecodes makes fixtures/frames.json
// carry its weight for this frame.
//
// The fixture is copied byte for byte into the TypeScript client's repository
// and nothing compares the two copies, so the only thing keeping it honest on
// this side is a test that decodes it with the real struct. Without this, the
// declare case is a hex string somebody typed: it could name a field
// `declaring` does not have and the suite would still be green, and the client
// repository would inherit the mistake with no way to find out.
func TestTheDeclareFixtureIsTheStructTheServerDecodes(t *testing.T) {
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
	if got := fixture.Types["declare"]; got != uint8(protocol.Declare) {
		t.Fatalf("the fixture calls declare %d and this side calls it %d", got, protocol.Declare)
	}

	seen := 0
	for _, one := range fixture.Cases {
		if one.Type != uint8(protocol.Declare) {
			continue
		}
		seen++

		asked := declaring{}
		decoder := json.NewDecoder(strings.NewReader(string(one.JSON)))
		// The strict decode is the whole point: a field the server does not
		// have would otherwise be dropped in silence, which is exactly how a
		// fixture drifts away from the code it is supposed to describe.
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&asked); err != nil {
			t.Fatalf("%s: the fixture does not decode as the server's own declaring struct: %v", one.Name, err)
		}
		if asked.Operation.Name == "" || asked.Operation.Action == "" || asked.Operation.Collection == "" {
			t.Fatalf("%s: decoded into an empty-looking operation: %+v", one.Name, asked.Operation)
		}
	}
	if seen == 0 {
		t.Fatal("the fixture holds no declare case, so nothing here was measured")
	}
}

// TestDeclaringFromManyConnectionsAtOnceIsOneWriterAtATime is the measurement
// behind declare()'s db.mutex.Lock().
//
// Declare never goes near store.SharedRead — the read-sharing branch invoke()
// takes for an operation that does not write — so there is no filter here that
// could be widened to let it in by mistake. What there is, is a database whose
// mutex became an RWMutex, and a handler right next door (explore) that takes
// the write lock while reading. A Declare on the read lock would be several
// goroutines mutating one btree, which is what this asks for on purpose: eight
// connections declaring eight different names into one database at the same
// moment.
//
// Under -race a shared lock here reports. Without -race it still fails, on the
// versions: each name is new, so every one of them must come back version 1
// and every one must be readable afterwards. A lost write shows up as a name
// that is not there.
func TestDeclaringFromManyConnectionsAtOnceIsOneWriterAtATime(t *testing.T) {
	const connections = 8

	server, address := running(t, false)
	declare(t, server, "acme", "main")

	clients := make([]*client, connections)
	for i := range clients {
		clients[i] = dial(t, address)
		greeting := decode[welcome](t, clients[i].open(server, "acme", "main"))
		if frame := clients[i].operate(greeting, secret); frame.Type != protocol.Result {
			t.Fatalf("elevating %d: %s", i, frame.Payload)
		}
	}

	names := make([]string, connections)
	for i := range names {
		names[i] = "articles.at_once_" + strings.Repeat("x", i+1)
	}

	start := make(chan struct{})
	wait := sync.WaitGroup{}
	versions := make([]int, connections)
	failures := make([]string, connections)
	for i, one := range clients {
		wait.Add(1)
		go func(i int, one *client) {
			defer wait.Done()
			<-start
			frame := one.declare(store.Operation{
				Name: names[i], Collection: "articles", Action: store.ActionScan,
				Index: "by_author", Limit: i + 1,
			})
			if frame.Type != protocol.Result {
				failures[i] = string(frame.Payload)
				return
			}
			versions[i] = decode[declared](t, frame).Operation.Version
		}(i, one)
	}
	close(start)
	wait.Wait()

	for i := range clients {
		if failures[i] != "" {
			t.Errorf("%s was refused: %s", names[i], failures[i])
		}
		if versions[i] != 1 {
			t.Errorf("%s came back at version %d, want 1 — every name here is new", names[i], versions[i])
		}
	}

	db, release, err := server.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for i, name := range names {
		stored, found, err := db.Operation(name, 0)
		if err != nil || !found {
			t.Errorf("%s is not in the database afterwards: found=%v err=%v", name, found, err)
			continue
		}
		if stored.Limit != i+1 {
			t.Errorf("%s came back declaring a limit of %d, want %d — one declaration overwrote another's",
				name, stored.Limit, i+1)
		}
	}
}

// TestADeclarationOverTheWireSurvivesTheServerBeingRestarted is the
// measurement behind declare()'s db.store.Commit().
//
// store.DeclareOperation writes the key and the change-log entry and leaves
// the commit to whoever called — which is what `sapedb apply` does at the end
// of its run. Skipping it here would be invisible to everything else in this
// file: the declaration is in the in-memory tree at once, so every read and
// every call in the same process finds it. It would be gone at the next
// restart, and the first person to notice would be whoever restarted the
// daemon on a Tuesday.
//
// So the assertion has to cross a restart, and nothing may write in between:
// a later write would commit the pending transaction, declaration included,
// and hide exactly the bug this is for.
func TestADeclarationOverTheWireSurvivesTheServerBeingRestarted(t *testing.T) {
	dir := t.TempDir()

	first := serverIn(t, dir)
	declare(t, first, "acme", "main")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = first.Serve(listener) }()

	client := dial(t, listener.Addr().String())
	greeting := decode[welcome](t, client.open(first, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	wanted := store.Operation{
		Name: "articles.recent", Collection: "articles", Action: store.ActionScan,
		Index: "by_author", Limit: 7,
	}
	if frame := client.declare(wanted); frame.Type != protocol.Result {
		t.Fatalf("declaring: %s", frame.Payload)
	}

	// Nothing writes between the declaration and the shutdown, on purpose.
	_ = listener.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := serverIn(t, dir)
	t.Cleanup(func() { _ = second.Close() })

	db, release, err := second.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	stored, found, err := db.Operation(wanted.Name, 0)
	if err != nil || !found {
		t.Fatalf("the declaration did not survive the restart: found=%v err=%v", found, err)
	}
	if stored.Limit != wanted.Limit || stored.Version != 1 {
		t.Fatalf("it came back as %+v, want limit %d at version 1", stored, wanted.Limit)
	}
}
