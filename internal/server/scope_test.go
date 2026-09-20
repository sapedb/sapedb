package server

import (
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// invokeHolding is a call that presents a set of scopes and a grant for them.
// A nil grant presents nothing at all, which is what every client sent before
// grants existed.
func (c *client) invokeHolding(name string, arguments map[string]any, held *grant) protocol.Frame {
	c.t.Helper()
	c.send(protocol.Invoke, call{Command: name, Arguments: arguments, Grant: held})
	return c.read()
}

// granting mints a grant the way whoever issues connection strings would.
func granting(t *testing.T, server *Server, account, name string, scopes []string) *grant {
	t.Helper()
	signature, err := server.Grant(account, name, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return &grant{Scopes: scopes, Signature: signature}
}

// rowsIn is the rows of a successful call, and a failure otherwise.
func rowsIn(t *testing.T, frame protocol.Frame) []map[string]any {
	t.Helper()
	if frame.Type != protocol.Result {
		t.Fatalf("wanted rows and got a %s: %s", frame.Type, frame.Payload)
	}
	return decode[store.Result](t, frame).Rows
}

// stock puts one article where a scan can find it, so that a successful call
// can be told from a call that succeeded at returning nothing.
func stock(t *testing.T, c *client) {
	t.Helper()
	if frame := c.invoke("articles.add", map[string]any{"title": "on scopes", "author": "ann"}); frame.Type != protocol.Result {
		t.Fatalf("stocking the collection: %s", frame.Payload)
	}
}

// TestAnOperationThatDeclaresAScopeRunsForWhoeverWasGrantedIt is the case that
// could not happen at all until this landed.
//
// `Operation.Scopes` has been checked fail-closed by store.allowed since it
// existed, and nothing on the wire could ever present one: internal/server
// built its store.Caller with no Scopes, over a comment saying a caller that
// names its own permissions has none. The comment was right about the danger.
// The consequence was that an operation declaring a scope was an operation
// nobody could call — a field that refused everything and permitted nothing,
// which is not a permission system but a way of disabling an operation by
// mentioning a word. A security mechanism that has never run is one nobody can
// say works.
//
// So the positive case is the whole point of this test, and everything else
// here is arranged around making it mean something: an operation declaring
// `articles:read` is CALLED, SUCCESSFULLY, by a caller holding a grant for it,
// and comes back with rows.
func TestAnOperationThatDeclaresAScopeRunsForWhoeverWasGrantedIt(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")
	stock(t, client)

	// The control that comes first, because a green line on a refusal proves
	// nothing about a connection that might refuse everything. articles.by_author
	// declares no scope: it must run with no grant at all, and return a row.
	unscoped := rowsIn(t, client.invokeHolding("articles.by_author", map[string]any{"author": "ann"}, nil))
	if len(unscoped) == 0 {
		t.Fatal("an operation that declares no scope came back with no rows — the collection is empty, so nothing below can tell a scope refusal from an empty answer")
	}

	// The negative. articles.secret declares articles:read, and a caller that
	// presents nothing holds nothing.
	refused := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, nil)
	if refused.Type != protocol.Failure {
		t.Fatalf("an operation needing a scope ran for a caller presenting none: %s", refused.Payload)
	}
	if !strings.Contains(string(refused.Payload), `"code":"not_allowed"`) {
		t.Errorf("the refusal reads %s, want not_allowed", refused.Payload)
	}
	if !strings.Contains(string(refused.Payload), "articles:read") {
		t.Errorf("the refusal does not name the scope that was missing: %s", refused.Payload)
	}

	// The positive. The same operation, the same connection, the same
	// arguments — and a grant.
	rows := rowsIn(t, client.invokeHolding("articles.secret",
		map[string]any{"author": "ann"}, granting(t, server, "acme", "main", []string{"articles:read"})))
	if len(rows) == 0 {
		t.Fatal("the scoped operation was allowed to run and came back with no rows — a call that returns nothing cannot be told from one that was refused quietly")
	}
	if got := rows[0]["author"]; got != "ann" {
		t.Fatalf("the scoped operation returned %v, which is not the row that was written", rows[0])
	}

	// A grant is for the scopes it names, not for having a grant. A properly
	// signed grant for a different scope is refused exactly as none is.
	wrong := client.invokeHolding("articles.secret",
		map[string]any{"author": "ann"}, granting(t, server, "acme", "main", []string{"articles:write"}))
	if wrong.Type != protocol.Failure {
		t.Fatalf("a grant for articles:write ran an operation that needs articles:read: %s", wrong.Payload)
	}
	if !strings.Contains(string(wrong.Payload), `"code":"not_allowed"`) {
		t.Errorf("the refusal reads %s, want not_allowed", wrong.Payload)
	}

	// Holding more than is asked for is not holding less: the check is that
	// every scope the operation names is present, never that the sets match.
	more := rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"},
		granting(t, server, "acme", "main", []string{"articles:write", "articles:read", "billing"})))
	if len(more) == 0 {
		t.Fatal("a grant holding the scope among others came back with no rows")
	}

	// Order and repetition are not part of what was granted. The grant is
	// minted over one spelling of the set and presented in another, and the
	// signature still stands — because Granting sorts and de-duplicates before
	// it signs.
	signature, err := server.Grant("acme", "main", []string{"articles:write", "articles:read"})
	if err != nil {
		t.Fatal(err)
	}
	rewritten := &grant{Scopes: []string{"articles:read", "articles:write", "articles:read"}, Signature: signature}
	if len(rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, rewritten))) == 0 {
		t.Fatal("the same set of scopes written in another order was not the same grant")
	}
}

// TestACallerCannotGrantItselfAScope is the property the whole mechanism rests
// on, measured rather than argued.
//
// Nothing a caller can compute makes a grant. The signature is an HMAC under a
// key derived from the server's own secret — the same secret that signs
// connection strings, under a label of its own so neither can be presented as
// the other — over the account, the database and the exact set of scopes. A
// caller holds a connection string, which was minted by whoever holds that
// secret; it does not hold the secret.
//
// Five ways to try, and every one of them is refused with `grant` rather than
// `not_allowed`: a grant that does not verify is a bad credential, not an
// absence of permission, and a caller whose grant was issued for the wrong
// database needs to be told THAT and not be left reading "you need
// articles:read" about a grant that says articles:read.
func TestACallerCannotGrantItselfAScope(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")
	declare(t, server, "acme", "other")
	declare(t, server, "rival", "main")

	client := dial(t, address)
	client.open(server, "acme", "main")
	stock(t, client)

	// The control, first and on the same connection: the real grant works.
	// Every refusal below is worth exactly as much as this line is.
	real := granting(t, server, "acme", "main", []string{"articles:read"})
	if len(rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, real))) == 0 {
		t.Fatal("the correctly minted grant did not work, so none of the refusals below measures anything")
	}

	forged := []struct {
		name string
		held *grant
	}{
		{
			// The obvious one, and the one the old comment was written about:
			// naming your own permissions.
			name: "scopes with no signature at all",
			held: &grant{Scopes: []string{"articles:read"}},
		},
		{
			name: "scopes with a made-up signature",
			held: &grant{Scopes: []string{"articles:read"}, Signature: strings.Repeat("00", 32)},
		},
		{
			// A real signature, over a real grant, with the list edited
			// afterwards. This is what makes the signature cover the scopes
			// rather than merely accompany them.
			name: "a real grant with a scope added to the list",
			held: &grant{Scopes: []string{"articles:read", "articles:write"}, Signature: real.Signature},
		},
		{
			// A real grant, minted by the same server under the same secret,
			// for another database of the same account.
			name: "a real grant for another database",
			held: granting(t, server, "acme", "other", []string{"articles:read"}),
		},
		{
			// A real grant for another account, which is what a tenant that
			// got hold of somebody else's grant would have.
			name: "a real grant belonging to another account",
			held: granting(t, server, "rival", "main", []string{"articles:read"}),
		},
	}

	refusals := 0
	for _, one := range forged {
		t.Run(one.name, func(t *testing.T) {
			frame := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, one.held)
			if frame.Type != protocol.Failure {
				t.Fatalf("a caller granted itself a scope and the call ran: %s", frame.Payload)
			}
			if !strings.Contains(string(frame.Payload), `"code":"grant"`) {
				t.Fatalf("refused with %s, want the grant code — a bad credential is not the same thing as a missing permission", frame.Payload)
			}
			refusals++
		})
	}
	if refusals != len(forged) {
		t.Fatalf("%d of %d forgeries were measured", refusals, len(forged))
	}

	// And the connection still works afterwards, with the real grant: a
	// refused grant refuses one call, it does not poison the session.
	if len(rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, real))) == 0 {
		t.Fatal("the real grant stopped working after a refused one")
	}
}

// TestProvingTheServerSecretIsNotTheSameAsHoldingAScope keeps two permissions
// apart that a later reader might reasonably assume were one.
//
// An operator has proved possession of the server's secret, which is strictly
// more than anybody else on the wire can do — and whoever holds that secret
// can mint themselves any grant they like. It would be easy, and wrong, to
// conclude that elevating should therefore carry every scope: what an operator
// may do is explore and declare, and running a declared operation is the
// ordinary path with the ordinary rules. Minting a grant is a deliberate act
// that leaves an account and a database written into it; inheriting one
// silently is not.
func TestProvingTheServerSecretIsNotTheSameAsHoldingAScope(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	stock(t, client)

	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// The control: elevating really did work, and this connection really can
	// do the thing an operator may do.
	if frame := client.explore(store.Access{Kind: "scan", Collection: "articles", Limit: 5}); frame.Type != protocol.Result {
		t.Fatalf("an elevated connection could not explore, so this test proves nothing about what it cannot do: %s", frame.Payload)
	}

	frame := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, nil)
	if frame.Type != protocol.Failure {
		t.Fatalf("proving the server secret carried a scope with it: %s", frame.Payload)
	}
	if !strings.Contains(string(frame.Payload), `"code":"not_allowed"`) {
		t.Errorf("the refusal reads %s, want not_allowed", frame.Payload)
	}

	// And with a grant, on the same elevated connection, it runs — so what was
	// refused above was the scope and not the connection.
	if len(rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"},
		granting(t, server, "acme", "main", []string{"articles:read"})))) == 0 {
		t.Fatal("an elevated connection presenting a real grant came back with no rows")
	}
}

// TestComposingIsNotAWayRoundAScopeOverTheWire measures the rule composed
// operations landed with, end to end, on a socket.
//
// store.allowed checks the operation a caller NAMED, and a caller names only
// the outermost one. Without a further rule, composing would be the way round
// every scope in the database: declare a batch with no scopes that calls the
// guarded operation, and call the batch. So validateComposedStep enforces the
// union at declare time — a parent must ask for every scope its callees ask
// for — and that rule has a test of its own in internal/store.
//
// What that test could not reach is the other half of the sentence. In-process
// it can build a Caller holding a scope and watch the composed operation run;
// over the wire nothing could present a scope at all, so "and then a caller
// that does not hold it is refused at call time" had never once been observed
// through a socket, and neither had its positive. The union rule was a rule
// about something unreachable.
//
// Here both halves are on one connection: the refusal when the parent does not
// ask, the acceptance when it does, and then the parent actually running —
// refused without a grant, and returning rows with one.
func TestComposingIsNotAWayRoundAScopeOverTheWire(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	stock(t, client)

	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// The guarded leg: an ordinary scan that asks for a scope.
	guarded := store.Operation{
		Name: "articles.guarded", Collection: "articles", Action: store.ActionScan,
		Index: "by_author", Scopes: []string{"articles:read"},
		Input: []store.Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:  &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
		To:    &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
		Limit: 5,
	}
	frame := client.declare(guarded)
	if frame.Type != protocol.Result {
		t.Fatalf("declaring the guarded leg: %s", frame.Payload)
	}
	version := decode[declared](t, frame).Operation.Version

	// The parent that calls it and asks for nothing. This is the way round,
	// and it is refused where it is written rather than where it would run.
	sneak := store.Operation{
		Name: "articles.roundabout", Collection: "articles", Action: store.ActionBatch,
		Input: []store.Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		Limit: 10,
		Steps: []store.Step{{
			Name: "rows", Operation: guarded.Name, Version: version,
			With: map[string]store.Term{"author": {Arg: "author"}},
		}},
	}
	refusal := reason(t, client.declare(sneak))
	if !strings.Contains(refusal, "articles:read") {
		t.Fatalf("a batch calling a scoped operation without asking for the scope was refused with %q, which does not name the scope", refusal)
	}

	// The control for that refusal: ask for it, and the same declaration is
	// stored. Without this the refusal above could be anything at all — a
	// batch this server will not take, a step shape it does not like.
	sneak.Scopes = []string{"articles:read"}
	if frame := client.declare(sneak); frame.Type != protocol.Result {
		t.Fatalf("a batch that asks for the scope its leg asks for was still refused: %s", frame.Payload)
	}

	// And now the half that had never been observed over a socket. The parent
	// is refused to a caller holding nothing...
	without := client.invokeHolding(sneak.Name, map[string]any{"author": "ann"}, nil)
	if without.Type != protocol.Failure {
		t.Fatalf("the composed operation ran for a caller holding no scope: %s", without.Payload)
	}
	if !strings.Contains(string(without.Payload), `"code":"not_allowed"`) {
		t.Errorf("the refusal reads %s, want not_allowed", without.Payload)
	}

	// ...and runs for one holding the scope, and comes back with the row the
	// guarded leg read. An empty answer here would be indistinguishable from a
	// refusal nobody reported.
	rows := rowsIn(t, client.invokeHolding(sneak.Name, map[string]any{"author": "ann"},
		granting(t, server, "acme", "main", []string{"articles:read"})))
	if len(rows) == 0 {
		t.Fatal("the composed operation was allowed to run and came back with no rows")
	}
	if got := rows[0]["author"]; got != "ann" {
		t.Fatalf("the composed operation returned %v, which is not the row the guarded leg reads", rows[0])
	}
}
