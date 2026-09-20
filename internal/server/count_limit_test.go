package server

import (
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/store"
)

// TestACountDeclaredOverTheWireMustSayHowFarItWalks is the count's half of the
// rule its sibling test measures for the scan.
//
// A scan has been made to declare a limit since there were scans. A count was
// not, and a count walks the same stretch — it just hands back a number
// instead of the rows. So until this rule covered it, `"action": "count"` with
// no `"limit"` was the one declaration in this store that said nothing about
// what it costs, and the cost it said nothing about was a walk of the whole
// collection.
//
// Measured over the wire because that is where a declaration now arrives from
// (the Declare frame), and because the Explore path CANNOT measure it: the
// shell's asOperation fills Limit in for a count exactly as it does for a
// scan, before validateOperation ever sees the operation. A test written
// through Explore would be green whether this rule existed or not. The last
// case below pins that, so this file says out loud which path it is not
// measuring.
func TestACountDeclaredOverTheWireMustSayHowFarItWalks(t *testing.T) {
	const rule = "sapedb/store: the declaration does not make sense: a count must declare how far it walks — it hands back a number rather than rows, so the walk is the whole of what it costs"

	server, address := running(t, false)
	declare(t, server, "acme", "main")

	client := dial(t, address)
	greeting := decode[welcome](t, client.open(server, "acme", "main"))
	if frame := client.operate(greeting, secret); frame.Type != protocol.Result {
		t.Fatalf("elevating: %s %s", frame.Type, frame.Payload)
	}

	// The control that makes the refusal mean something: the same count WITH a
	// limit goes through, and comes back stored. A test whose only measurement
	// is a red line cannot tell "the rule fired" from "nothing on this
	// connection works at all".
	withLimit := store.Operation{
		Name: "articles.how_many", Collection: "articles", Action: store.ActionCount,
		Index: "by_author", Limit: 100,
	}
	frame := client.declare(withLimit)
	if frame.Type != protocol.Result {
		t.Fatalf("a count that declares its limit was refused: %s", frame.Payload)
	}
	stored := decode[declared](t, frame).Operation
	if stored.Version != 1 {
		t.Fatalf("the first version of a new name came back as %d", stored.Version)
	}
	if stored.Limit != 100 {
		t.Fatalf("the stored count declares a limit of %d, want the 100 it was declared with", stored.Limit)
	}

	// The same declaration with the limit taken out.
	limitless := withLimit
	limitless.Limit = 0
	if got := reason(t, client.declare(limitless)); got != rule {
		t.Fatalf("a limitless count was refused with\n got  %q\n want %q", got, rule)
	}

	// Refused by the rule, not by the name being taken.
	limitless.Name = "articles.nobody_has_declared_this_count"
	if got := reason(t, client.declare(limitless)); got != rule {
		t.Fatalf("a limitless count under a fresh name was refused with\n got  %q\n want %q", got, rule)
	}

	// A negative limit is refused by the same words. It used to be caught
	// further down validateOperation, by a `Limit < 0` branch that was the
	// only thing standing between a count and a number nobody checked; that
	// branch is gone because this rule now covers it, and this case is what
	// says so.
	negative := withLimit
	negative.Name = "articles.negative_count"
	negative.Limit = -5
	if got := reason(t, client.declare(negative)); got != rule {
		t.Fatalf("a count declaring a limit of -5 was refused with\n got  %q\n want %q", got, rule)
	}

	// A scan keeps its own words. Two actions, two sentences: somebody reading
	// the refusal for a count should not be told about rows it never returns.
	const scanRule = "sapedb/store: the declaration does not make sense: a scan must declare how many rows it may return"
	scan := store.Operation{
		Name: "articles.limitless_scan", Collection: "articles", Action: store.ActionScan,
		Index: "by_author",
	}
	if got := reason(t, client.declare(scan)); got != scanRule {
		t.Fatalf("a limitless scan was refused with\n got  %q\n want %q", got, scanRule)
	}

	// And the path this test exists because it cannot use. Explore accepts the
	// same limitless count and quietly hands it store.MostRows, exactly as it
	// does for a scan — so the shell is still a shell, and a count typed at it
	// still cannot be the way this rule is measured.
	client.send(protocol.Explore, exploring{Access: store.Access{
		Kind: "count", Collection: "articles", Index: "by_author",
	}})
	explore := client.read()
	if explore.Type != protocol.Result {
		t.Fatalf("Explore refused a limitless count, so this test's premise is stale: %s", explore.Payload)
	}
	if got := decode[explored](t, explore).Draft.Limit; got != store.MostRows {
		t.Fatalf("Explore's count draft came back with a limit of %d, want the %d cap it quietly applies", got, store.MostRows)
	}
}
