package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// SAPE-9's guards. collision_test.go measures what happens WITHOUT namespaces
// and is deliberately untouched by this file: every test in it still passes,
// because every name in it is flat and a flat name is in the unnamed
// namespace, where nothing changed.
//
// Every expectation below is written out by hand — ":" rather than
// NamespaceSeparator, "key" and "actor" rather than ClaimedByKey and
// ClaimedByActor. A test that reads its expected value out of the same
// constant the code reads can only ever agree with itself, and would stay
// green through a change that moved both.

// TestTwoVendorsInTheirOwnNamespacesCannotTakeEachOthersNameOver is the other
// half of TestASecondDeclarationOfANameTakesOverEveryUnversionedCall: the same
// two vendors, the same two declarations, the same argument, the same
// unversioned call — and each vendor in a namespace of its own.
//
// It exercises DeclareOperation and Invoke rather than comparing name strings,
// because the claim is about behaviour: the first vendor's read still returns
// its row after the second vendor has tried, and the document the second
// vendor's delete named is still there.
func TestTwoVendorsInTheirOwnNamespacesCannotTakeEachOthersNameOver(t *testing.T) {
	store, collection := declared(t, 910)
	fill(t, collection)

	first, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("acme:orders.recent"))
	if err != nil {
		t.Fatalf("vendor-a declaring into its own namespace was refused: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("the first declaration of a namespaced name is version %d, want 1", first.Version)
	}

	// The known-positive. Without it, every "the caller still gets a read"
	// below could be a test that never reached a document.
	before, err := store.Invoke(Caller{}, "acme:orders.recent", 0, map[string]any{"id": "a0"})
	if err != nil {
		t.Fatalf("invoking the namespaced name: %v", err)
	}
	if before.Count != 1 || len(before.Rows) != 1 {
		t.Fatalf("the first vendor's read returned %d rows before anything collided — this test is measuring nothing", len(before.Rows))
	}

	// Vendor-b tries the takeover that collision_test.go measures succeeding
	// on a flat name. The name is spelled exactly as vendor-a's.
	_, err = store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("acme:orders.recent"))
	if err == nil {
		t.Fatal("vendor-b declared over vendor-a's namespaced name and was not refused")
	}
	if !errors.Is(err, ErrNamespace) {
		t.Fatalf("the refusal is %v, and it is not ErrNamespace — a client switching on the wire code cannot tell this from any other failure", err)
	}
	// The refusal names who holds it. A refusal that says only "no" leaves the
	// customer with nobody to ask.
	if !strings.Contains(err.Error(), "vendor-a") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}
	if !strings.Contains(err.Error(), "acme") {
		t.Errorf("the refusal does not name the namespace: %v", err)
	}
	t.Logf("vendor-b declaring into acme: %v", err)

	// And the behaviour, not the message: the first vendor's caller has not
	// changed a line, and it still gets its read.
	after, err := store.Invoke(Caller{Actor: "the-first-vendors-app"}, "acme:orders.recent", 0, map[string]any{"id": "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != 1 {
		t.Errorf("an unversioned call ran version %d, want 1 — nothing should have been declared over this name", after.Version)
	}
	if after.Count != 1 || after.Changed != 0 {
		t.Errorf("the call returned %d rows and changed %d documents, want 1 and 0 — the refused declaration decided what this call does after all",
			after.Count, after.Changed)
	}
	if _, found, err := collection.Get("a1"); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Error("the document vendor-b's delete named is gone, so the refusal above did not stop anything")
	}

	// Vendor-b is not locked out of the product, only out of somebody else's
	// namespace. Its own namespace is there for the taking, and the name it
	// wanted is free inside it.
	own, err := store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("beta:orders.recent"))
	if err != nil {
		t.Fatalf("vendor-b declaring into its own namespace was refused: %v", err)
	}
	if own.Version != 1 {
		t.Errorf("vendor-b's own first declaration is version %d, want 1", own.Version)
	}
}

// TestTheUnnamedNamespaceIsNeverClaimed is the guard on the one thing this
// feature must not do. Every name declared before namespaces existed is flat;
// if a flat name claimed the empty namespace for whoever declared it first,
// every existing database would refuse its owner's next declaration.
func TestTheUnnamedNamespaceIsNeverClaimed(t *testing.T) {
	store, collection := declared(t, 911)
	fill(t, collection)

	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("orders.recent")); err != nil {
		t.Fatal(err)
	}
	// A different actor, a different flat name: this is the ordinary case and
	// it must not be refused.
	if _, err := store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingRead("orders.one")); err != nil {
		t.Fatalf("a second actor declaring a flat name was refused: %v", err)
	}
	// And the takeover collision_test.go measures still happens, unchanged.
	second, err := store.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("orders.recent"))
	if err != nil {
		t.Fatalf("a flat name was protected as though it had a namespace: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("the second flat declaration is version %d, want 2", second.Version)
	}

	// Nothing was recorded against the empty namespace, asked directly.
	if _, found, err := store.NamespaceClaim(""); err != nil || found {
		t.Errorf("the unnamed namespace reads as claimed (found=%v err=%v)", found, err)
	}
}

// TestAClaimSurvivesReopeningTheDatabase: a claim that lived only in memory
// would protect a namespace until the server restarted, which is worse than
// not protecting it, because nobody would be watching at the moment it stopped.
func TestAClaimSurvivesReopeningTheDatabase(t *testing.T) {
	disk, store := fresh(t, 912)
	if _, err := store.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("acme:orders.recent")); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(912, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}

	claim, found, err := reopened.NamespaceClaim("acme")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the claim on \"acme\" is gone after a reopen")
	}
	if claim.Kind != "actor" || claim.Identity != "vendor-a" || claim.Namespace != "acme" {
		t.Errorf("the reopened claim is %+v, want namespace \"acme\", kind \"actor\", identity \"vendor-a\"", claim)
	}

	// The claim is not a record somebody has to read: it refuses, after a
	// reopen, exactly as it did before one.
	if _, err := reopened.DeclareOperation(Caller{Actor: "vendor-b"}, collidingDelete("acme:orders.recent")); !errors.Is(err, ErrNamespace) {
		t.Errorf("after a reopen, vendor-b was refused with %v, want ErrNamespace", err)
	}
	// And the holder can still declare into it, so the reopened claim is not
	// simply refusing everybody.
	if _, err := reopened.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("acme:orders.another")); err != nil {
		t.Errorf("after a reopen, the holder was refused its own namespace: %v", err)
	}
}

// TestAKeyAndAnActorThatSpellTheSameAreNotTheSameClaimant is why a claim
// records a kind and not only a string.
//
// The two identities below are the same eleven characters. One was checked by
// ed25519 over the declarations it installed; the other is a label somebody
// typed. Treating them as one claimant would let anybody who can choose their
// own actor name walk into a namespace a signing key holds.
func TestAKeyAndAnActorThatSpellTheSameAreNotTheSameClaimant(t *testing.T) {
	store, _ := declared(t, 913)

	const spelling = "same-string"

	if _, err := store.DeclareOperation(Caller{Actor: "installer", Signer: spelling}, collidingRead("acme:one")); err != nil {
		t.Fatalf("a signed declaration into a free namespace was refused: %v", err)
	}
	claim, found, err := store.NamespaceClaim("acme")
	if err != nil || !found {
		t.Fatalf("reading the claim back: found=%v err=%v", found, err)
	}
	if claim.Kind != "key" || claim.Identity != spelling {
		t.Fatalf("a signed declaration recorded %+v, want kind \"key\" and identity %q — the rest of this test is measuring nothing", claim, spelling)
	}

	// The known-positive: the same key gets back in.
	if _, err := store.DeclareOperation(Caller{Actor: "somebody-else", Signer: spelling}, collidingRead("acme:two")); err != nil {
		t.Errorf("the holding key was refused its own namespace: %v — the actor is not supposed to matter when a signer is present", err)
	}

	// And the whole point: an actor spelled identically is not that key.
	if _, err := store.DeclareOperation(Caller{Actor: spelling}, collidingDelete("acme:one")); !errors.Is(err, ErrNamespace) {
		t.Errorf("an actor named %q walked into a namespace held by the KEY %q: %v", spelling, spelling, err)
	}
}

// TestANamespacedOperationIsReachedByItsWholeName covers the resolution half
// of the design: nothing about Invoke or InvokeVersion learned to split a
// name, so the full `namespace:name` has to be the string that resolves, in
// both of them, including to an older version by number.
func TestANamespacedOperationIsReachedByItsWholeName(t *testing.T) {
	store, collection := declared(t, 914)
	fill(t, collection)

	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("acme:orders.recent")); err != nil {
		t.Fatal(err)
	}
	// A second version by the SAME holder, which is allowed and is how a
	// vendor ships an update: the namespace rule is about who, not about how
	// many times.
	second, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, byAuthorNamed("acme:orders.recent"))
	if err != nil {
		t.Fatalf("the holder's second declaration was refused: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("the holder's second declaration is version %d, want 2", second.Version)
	}

	latest, err := store.Invoke(Caller{}, "acme:orders.recent", 0, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("Invoke by whole name: %v", err)
	}
	if latest.Version != 2 || latest.Count != 3 {
		t.Errorf("Invoke ran version %d returning %d rows, want version 2 and 3 rows", latest.Version, latest.Count)
	}

	pinned, err := store.Invoke(Caller{}, "acme:orders.recent", 1, map[string]any{"id": "a0"})
	if err != nil {
		t.Fatalf("InvokeVersion by whole name: %v", err)
	}
	if pinned.Version != 1 || pinned.Count != 1 {
		t.Errorf("InvokeVersion(1) ran version %d returning %d rows, want version 1 and 1 row", pinned.Version, pinned.Count)
	}

	// The known-negative, so that "the whole name resolves" is not really
	// "anything resolves": the local half on its own is not a name here.
	if _, err := store.Invoke(Caller{}, "orders.recent", 0, map[string]any{"id": "a0"}); !errors.Is(err, ErrNoOperation) {
		t.Errorf("the name without its namespace resolved to something: %v", err)
	}
	_ = collection
}

// byAuthorNamed is byAuthor under a name the caller chooses, so a second
// version of one name can be a visibly different declaration.
func byAuthorNamed(name string) Operation {
	operation := byAuthor()
	operation.Name = name
	return operation
}

// TestWhatAnOperationNameMayBeNow is the grammar, both directions.
//
// The accepted list matters as much as the refused one: this rule is a
// tightening, and a tightening that went further than it was supposed to would
// refuse names that 1.0 has to keep. Every string here is written out.
func TestWhatAnOperationNameMayBeNow(t *testing.T) {
	store, _ := declared(t, 915)

	// Accepted. The first six are exactly the list
	// TestAnOperationNameMayHoldAnySeparatorAName holds, so this says in one
	// place that the tightening did not reach them.
	for _, name := range []string{
		"recent",
		"orders.recent",
		"orders/recent",
		"acme:orders.recent",
		"@acme/orders.recent",
		"orders.recent.v2.final.really",
		strings.Repeat("n", 128),
		"a:b",
		"Acme-Corp_1.0:orders.recent",
		"acme:@scope/orders recent",
		strings.Repeat("n", 123) + ":name",
	} {
		if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead(name)); err != nil {
			t.Errorf("declaring %q was refused: %v", name, err)
		}
	}

	// Refused, each for its own reason, and each reason spelled out here so
	// that a rule quietly doing somebody else's job shows up.
	for _, refused := range []struct {
		name string
		why  string
	}{
		{"a:b:c", "two separators"},
		{"acme::orders", "two separators, adjacent"},
		{":orders.recent", "an empty namespace"},
		{"acme:", "a namespace with no name"},
		{":", "both halves empty"},
		{"acme corp:orders", "a space in the namespace"},
		{"acme/corp:orders", "a slash in the namespace"},
		{"Αcme:orders", "a non-ASCII byte in the namespace"},
		{"acme:orders\x00recent", "a zero byte, which usableName still catches"},
		{strings.Repeat("n", 124) + ":name", "129 bytes in total, over the limit the whole name has"},
	} {
		if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead(refused.name)); err == nil {
			t.Errorf("declaring %q was accepted, and it has %s", refused.name, refused.why)
		}
	}

	// The 128-byte limit is on the WHOLE name. A namespace is not a way to get
	// a longer name, and this is the pair that says so: 123 + ":" + 4 is 128
	// and is in the accepted list above; 124 + ":" + 4 is 129 and is refused.
	if len(strings.Repeat("n", 123)+":name") != 128 {
		t.Fatal("the accepted long name is not 128 bytes, so the pair above measures nothing")
	}
	if len(strings.Repeat("n", 124)+":name") != 129 {
		t.Fatal("the refused long name is not 129 bytes, so the pair above measures nothing")
	}
}

// TestSplitOperationNameReadsBothHalves checks the split itself, against
// hand-written halves. It is not the behaviour guard — the tests above are —
// but a wrong split that happened to refuse the same set would pass all of
// them while resolving `acme:orders` into the wrong namespace.
func TestSplitOperationNameReadsBothHalves(t *testing.T) {
	for _, want := range []struct {
		name      string
		namespace string
		local     string
	}{
		{"recent", "", "recent"},
		{"orders.recent", "", "orders.recent"},
		{"@acme/orders.recent", "", "@acme/orders.recent"},
		{"acme:orders.recent", "acme", "orders.recent"},
		{"a:b", "a", "b"},
		{"Acme-Corp_1.0:orders.recent", "Acme-Corp_1.0", "orders.recent"},
	} {
		namespace, local, err := SplitOperationName(want.name)
		if err != nil {
			t.Errorf("splitting %q: %v", want.name, err)
			continue
		}
		if namespace != want.namespace || local != want.local {
			t.Errorf("splitting %q gave namespace %q and name %q, want %q and %q",
				want.name, namespace, local, want.namespace, want.local)
		}
	}
}

// TestExploreCannotClaimANamespace: Explore builds a declaration and validates
// it, and handing a shell the power to take a namespace by looking at a
// collection would be the worst possible source of a claim.
func TestExploreCannotClaimANamespace(t *testing.T) {
	store, collection := declared(t, 916)
	fill(t, collection)
	_ = collection

	if _, _, err := store.Explore(Caller{Actor: "an-operator"}, Access{Kind: "get", Collection: "articles", Key: "a0"}); err != nil {
		t.Fatalf("exploring: %v", err)
	}

	// And the case that could actually take one: a collection whose own name
	// gives the draft a namespace. Explore names its draft
	// `<collection>.<kind>`, so looking at "acme:articles" builds
	// "acme:articles.count" — a name in the namespace "acme", which a vendor
	// may be about to declare into.
	namespaced := articles()
	namespaced.Name = "acme:articles"
	if _, err := store.Declare(Caller{}, namespaced); err != nil {
		t.Fatal(err)
	}
	draft, _, err := store.Explore(Caller{Actor: "an-operator"}, Access{Kind: "count", Collection: "acme:articles"})
	if err != nil {
		t.Fatalf("exploring a collection in a namespace: %v", err)
	}
	if draft.Name != "acme:articles.count" {
		t.Fatalf("the draft is named %q, want %q — this test is measuring the wrong namespace", draft.Name, "acme:articles.count")
	}

	// Nothing anywhere is claimed after a shell session.
	for _, namespace := range []string{"articles", "acme"} {
		if _, found, err := store.NamespaceClaim(namespace); err != nil || found {
			t.Errorf("exploring claimed namespace %q (found=%v err=%v)", namespace, found, err)
		}
	}
	// Said as behaviour rather than as a record: the vendor who was going to
	// use "acme" still can, and it is a DIFFERENT identity from the operator
	// who ran the shell.
	if _, err := store.DeclareOperation(Caller{Actor: "vendor-a"}, collidingRead("acme:orders.recent")); err != nil {
		t.Errorf("a shell session took the namespace out from under a vendor: %v", err)
	}
}

// TestExploreWillNotHandBackADraftNobodyCouldDeclare is the other side of the
// shell, and the reason the name grammar is checked in validateOperation
// rather than only where a claim is taken.
//
// Explore names its draft `<collection>.<kind>` and hands it back so that
// looking around ends in something to commit. A collection name may hold a
// colon — usableName has never said otherwise — so a collection can be named
// in a way that makes that draft an impossible operation name. A shell that
// printed one anyway would be printing a declaration whose only future is a
// refusal.
func TestExploreWillNotHandBackADraftNobodyCouldDeclare(t *testing.T) {
	store, _ := declared(t, 917)

	// The known-positive: a collection whose name holds ONE colon in a usable
	// place still explores, and the draft it produces is declarable. Without
	// this row, "exploring is refused" could be a rule that refuses every
	// colon and this test would not notice.
	fine := articles()
	fine.Name = "acme:articles"
	if _, err := store.Declare(Caller{}, fine); err != nil {
		t.Fatal(err)
	}
	draft, _, err := store.Explore(Caller{Actor: "an-operator"}, Access{Kind: "count", Collection: "acme:articles"})
	if err != nil {
		t.Fatalf("exploring a collection whose name holds one colon: %v", err)
	}
	if draft.Name != "acme:articles.count" {
		t.Errorf("the draft is named %q, want %q", draft.Name, "acme:articles.count")
	}

	// And the refusal. Two colons in the collection name makes the draft name
	// hold two, which is not a name.
	awkward := articles()
	awkward.Name = "a:b:c"
	if _, err := store.Declare(Caller{}, awkward); err != nil {
		t.Fatalf("declaring a collection called %q: %v — usableName has always allowed this and this test needs it to", awkward.Name, err)
	}
	if _, _, err := store.Explore(Caller{Actor: "an-operator"}, Access{Kind: "count", Collection: "a:b:c"}); !errors.Is(err, ErrNamespace) {
		t.Errorf("exploring %q gave %v, want ErrNamespace — the shell handed back a draft that could never be declared", "a:b:c", err)
	}
}
