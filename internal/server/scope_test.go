package server

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// grantLife is how long the grants in these tests are minted for. Long enough
// that nothing here can expire while the suite runs on a slow machine, short
// enough to be a real expiry rather than a way of writing "never" — which is
// the thing ISS-11 abolished and which no test in this file may quietly
// reintroduce.
const grantLife = time.Hour

// grantSerial hands every grant in these tests a serial of its own. A serial
// is mandatory and is inside the signature, so one shared constant would make
// every grant here differ only in its scopes and hide a whole field from every
// assertion below.
var grantSerial atomic.Int64

// granting mints a grant the way whoever issues connection strings would:
// offline, with the secret, for a stated span of time.
func granting(t *testing.T, server *Server, account, name string, scopes []string) *grant {
	t.Helper()
	return grantingUntil(t, server, account, name, scopes, time.Now().Add(grantLife))
}

// grantingUntil is granting with the expiry said out loud, for the tests that
// are about the expiry.
func grantingUntil(t *testing.T, server *Server, account, name string, scopes []string, expires time.Time) *grant {
	t.Helper()
	serial := fmt.Sprintf("test-%04d", grantSerial.Add(1))
	signature, err := server.Grant(account, name, scopes, expires, serial)
	if err != nil {
		t.Fatal(err)
	}
	return &grant{Scopes: scopes, Expires: expires.Unix(), Serial: serial, Signature: signature}
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
	minted := grantingUntil(t, server, "acme", "main",
		[]string{"articles:write", "articles:read"}, time.Now().Add(grantLife))
	rewritten := &grant{
		Scopes:    []string{"articles:read", "articles:write", "articles:read"},
		Expires:   minted.Expires,
		Serial:    minted.Serial,
		Signature: minted.Signature,
	}
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

// TestAnExpiredGrantIsRefusedUnderItsOwnCode is ISS-11 measured where it
// actually has to hold: over a socket, in the JSON a client parses.
//
// Two things are checked and the second is the one with history behind it.
// The first is that an expired grant does not work — a grant was good forever
// until this, and the only way to withdraw one was to rotate the server secret
// and break every connection string on the machine along with it.
//
// The second is that it is refused under a code of its own. ISS-21 is exactly
// the mistake this avoids: a condition a program has to branch on, arriving
// with no name, so the only way to act on it is to match the prose — and the
// prose is the one part of an error a server is free to improve. An expired
// grant and a grant that does not authorise the scope are different
// situations with different answers: refresh, and give up. So they are
// different codes, and this test reads them off the wire rather than out of
// codeFor's table. A table test that only reads the table agrees with itself.
func TestAnExpiredGrantIsRefusedUnderItsOwnCode(t *testing.T) {
	server, address := running(t, false)
	declare(t, server, "acme", "main")
	declare(t, server, "acme", "other")

	client := dial(t, address)
	client.open(server, "acme", "main")
	stock(t, client)

	// The control, first and on the same connection. Every refusal below is
	// worth exactly as much as this line is.
	live := granting(t, server, "acme", "main", []string{"articles:read"})
	if len(rowsIn(t, client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, live))) == 0 {
		t.Fatal("the unexpired grant did not work, so none of the refusals below measures anything")
	}

	refusals := []struct {
		name string
		held *grant
		code string
	}{
		{
			// An hour past. The signature is perfect and the grant is dead,
			// which is the whole of what this ticket added.
			"an hour out of date",
			grantingUntil(t, server, "acme", "main", []string{"articles:read"}, time.Now().Add(-time.Hour)),
			"grant_expired",
		},
		{
			// One second past, which is where an off-by-one would hide.
			"a second out of date",
			grantingUntil(t, server, "acme", "main", []string{"articles:read"}, time.Now().Add(-time.Second)),
			"grant_expired",
		},
		{
			// Minted for the moment it was minted. Expires is the first
			// instant the grant is no longer one, not the last that it is,
			// and there is no skew allowance to lend it another second: the
			// margin belongs in the number the issuer signs, where an
			// operator can see it, not in a constant inside the verifier
			// where nobody can.
			"expiring at the instant it was minted",
			grantingUntil(t, server, "acme", "main", []string{"articles:read"}, time.Now()),
			"grant_expired",
		},
		{
			// And the other half of the distinction. A grant for another
			// database is not expired, it is not yours, and a caller that
			// reacted to it by going and fetching a fresh grant would fetch
			// the same wrong one forever.
			"issued for another database, and not expired",
			grantingUntil(t, server, "acme", "other", []string{"articles:read"}, time.Now().Add(grantLife)),
			"grant",
		},
	}
	for _, one := range refusals {
		t.Run(one.name, func(t *testing.T) {
			frame := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, one.held)
			if frame.Type == protocol.Result {
				t.Fatalf("the call ran: %s", frame.Payload)
			}
			if want := `"code":"` + one.code + `"`; !strings.Contains(string(frame.Payload), want) {
				t.Fatalf("the refusal reads %s, want %s", frame.Payload, want)
			}
		})
	}

	// A caller that edits the expiry, or the serial, on a real grant holds a
	// signature that no longer covers what it is presenting. That is a forged
	// grant and not an expired one, whichever way the number was moved —
	// including the direction that would be useful, which is forwards.
	expired := grantingUntil(t, server, "acme", "main", []string{"articles:read"}, time.Now().Add(-time.Hour))
	for _, edit := range []struct {
		name   string
		change func(*grant)
	}{
		{"the expiry pushed into the future", func(g *grant) { g.Expires = time.Now().Add(grantLife).Unix() }},
		{"the serial rewritten", func(g *grant) { g.Serial = "not-the-one-that-was-signed" }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			edited := *expired
			edit.change(&edited)
			frame := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, &edited)
			if frame.Type == protocol.Result {
				t.Fatalf("a caller rewrote a signed field of its own grant and the call ran: %s", frame.Payload)
			}
			if !strings.Contains(string(frame.Payload), `"code":"grant"`) {
				t.Fatalf("the refusal reads %s, want the grant code — an edited grant is forged, not expired", frame.Payload)
			}
		})
	}

	// And the last shape a client can send: a grant object with no expiry in
	// it at all, which is what a client written against the older wire would
	// produce. There is no second message shape for it to be read as. It is
	// a grant whose exp is zero, it does not verify, and it is refused —
	// never accepted as "no expiry stated, so no expiry".
	noExpiry := &grant{Scopes: []string{"articles:read"}, Signature: live.Signature}
	frame := client.invokeHolding("articles.secret", map[string]any{"author": "ann"}, noExpiry)
	if frame.Type == protocol.Result {
		t.Fatalf("a grant with no expiry ran: %s", frame.Payload)
	}
	if !strings.Contains(string(frame.Payload), `"code":"grant"`) {
		t.Fatalf("a grant sent with no expiry was refused as %s, want the grant code", frame.Payload)
	}
}

// TestTheExpiredGrantCodeIsOnTheTable is the half of the previous test that
// cannot be seen from the wire: that ErrGrantExpired reaches codeFor at all,
// and that it does so without depending on where it sits in the table.
//
// It is written because the wire test above could pass with ErrGrantExpired
// wrapping ErrGrant and the two rows in the right order — and then go wrong
// the day somebody sorts the table. Here the error is handed to codeFor
// directly and the ordering assumption is named rather than relied on.
func TestTheExpiredGrantCodeIsOnTheTable(t *testing.T) {
	expired := fmt.Errorf("%w: %q on %q", ErrGrantExpired, "acme", "main")
	if got := codeFor(expired); got != "grant_expired" {
		t.Errorf("codeFor(%v) = %q, want %q", expired, got, "grant_expired")
	}
	// The control: the older condition still has its own code, so the two
	// can be told apart rather than one having eaten the other.
	forged := fmt.Errorf("%w: %q on %q", ErrGrant, "acme", "main")
	if got := codeFor(forged); got != "grant" {
		t.Errorf("codeFor(%v) = %q, want %q", forged, got, "grant")
	}
	// Independent sentinels, not one wrapping the other. If either of these
	// becomes true, codeFor's answer starts depending on the order of two
	// rows in a table nobody thinks of as ordered.
	if errors.Is(expired, ErrGrant) {
		t.Error("an expired grant satisfies errors.Is(err, ErrGrant), so codeFor's answer now depends on which row comes first")
	}
	if errors.Is(forged, ErrGrantExpired) {
		t.Error("a forged grant satisfies errors.Is(err, ErrGrantExpired), so codeFor's answer now depends on which row comes first")
	}
}
