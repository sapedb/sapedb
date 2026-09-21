package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The declaration says who declared it (ISS-32).
//
// Every test here goes through DeclareOperation and reads the answer back out
// of the store, rather than inspecting the struct it was handed: the field is
// only worth anything if it is what got WRITTEN, and a value still sitting in
// a returned struct proves nothing about the bytes under the key.

// declarerNamed is what a declarer read back should equal, spelled out on the
// test side. The kind strings are hand-written literals here rather than the
// ClaimedByKey/ClaimedByActor constants on purpose: a test that takes its
// expectation from the constants it is checking agrees with itself whatever
// either constant is changed to, and these two strings reach the wire.
func declarerNamed(t *testing.T, store *Store, name string, version int) *Declarer {
	t.Helper()
	stored, found, err := store.Operation(name, version)
	if err != nil {
		t.Fatalf("reading %q version %d back: %v", name, version, err)
	}
	if !found {
		t.Fatalf("%q version %d is not there", name, version)
	}
	return stored.DeclaredBy
}

// TestADeclarationRecordsWhoDeclaredIt is the whole feature at its narrowest:
// declare, reopen the record, and find the identity on it.
//
// Three callers, because the three answers come from three different branches
// and a plumbing mistake that hits one misses the others. The empty caller is
// the known negative and it is a real one: without it, an identity found on
// the other two could be anything this path always writes.
func TestADeclarationRecordsWhoDeclaredIt(t *testing.T) {
	_, store := fresh(t, 900)
	if _, err := store.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}

	byActor := byAuthor()
	byActor.Name = "articles.by_actor"
	if _, err := store.DeclareOperation(Caller{Actor: "ann@acme"}, byActor); err != nil {
		t.Fatal(err)
	}

	bySigner := byAuthor()
	bySigner.Name = "articles.by_signer"
	// A signer AND an actor: a bundle install has both, and the checked one
	// has to win. If it did not, every signed declaration would be recorded
	// against whatever name the connection happened to carry.
	if _, err := store.DeclareOperation(Caller{Actor: "ann@acme", Signer: "9f2c"}, bySigner); err != nil {
		t.Fatal(err)
	}

	byNobody := byAuthor()
	byNobody.Name = "articles.by_nobody"
	if _, err := store.DeclareOperation(Caller{}, byNobody); err != nil {
		t.Fatal(err)
	}

	got := declarerNamed(t, store, "articles.by_actor", 1)
	if got == nil {
		t.Fatalf("a declaration made by the actor %q came back with no declarer at all", "ann@acme")
	}
	if got.Kind != "actor" || got.Identity != "ann@acme" {
		t.Errorf("declared by actor ann@acme, recorded as kind %q identity %q, want kind \"actor\" identity \"ann@acme\"", got.Kind, got.Identity)
	}

	got = declarerNamed(t, store, "articles.by_signer", 1)
	if got == nil {
		t.Fatalf("a declaration made under the signing key %q came back with no declarer at all", "9f2c")
	}
	if got.Kind != "key" || got.Identity != "9f2c" {
		t.Errorf("declared under signing key 9f2c with actor ann@acme alongside it, recorded as kind %q identity %q, want kind \"key\" identity \"9f2c\" — the checked identity has to win over the named one", got.Kind, got.Identity)
	}

	// The known negative. An actor spelled "" is not a declarer, it is the
	// absence of one, and recording it as a kind with an empty identity would
	// assert a fact nobody supplied.
	if got := declarerNamed(t, store, "articles.by_nobody", 1); got != nil {
		t.Errorf("a declaration made through an empty Caller is recorded as kind %q identity %q — the identity is coming from somewhere other than the caller", got.Kind, got.Identity)
	}
}

// TestTheDeclarerIsNeverTheOneTheCallerSubmitted: the field is the store's
// account of who declared, so what arrives in it is thrown away.
//
// A field a client can set is a field a client can lie in, and this one would
// be worth lying in — it is the record that says an operation came from a
// particular vendor's key. Both directions are measured: a submitted value
// does not survive a declaration made by somebody else, and it does not
// survive a declaration made by nobody either. That second case is the one a
// naive "fill it in if it is empty" implementation passes the first check and
// fails.
func TestTheDeclarerIsNeverTheOneTheCallerSubmitted(t *testing.T) {
	_, store := fresh(t, 901)
	if _, err := store.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}

	forged := byAuthor()
	forged.Name = "articles.forged"
	forged.DeclaredBy = &Declarer{Kind: "key", Identity: "deadbeef"}
	if _, err := store.DeclareOperation(Caller{Actor: "mallory"}, forged); err != nil {
		t.Fatal(err)
	}

	got := declarerNamed(t, store, "articles.forged", 1)
	if got == nil {
		t.Fatal("a declaration submitted with a forged declarer came back with none — the caller's value was dropped, but nothing replaced it")
	}
	if got.Identity == "deadbeef" {
		t.Errorf("a declaration submitted with declarer key %q kept it — a caller can write their own audit record", "deadbeef")
	}
	if got.Kind != "actor" || got.Identity != "mallory" {
		t.Errorf("declared by actor mallory with a forged declarer in the payload, recorded as kind %q identity %q, want kind \"actor\" identity \"mallory\"", got.Kind, got.Identity)
	}

	// Nobody declaring it at all. The submitted value must not be kept as a
	// fallback: absent is the right answer and a forged value is not better
	// than nothing.
	orphan := byAuthor()
	orphan.Name = "articles.orphan"
	orphan.DeclaredBy = &Declarer{Kind: "key", Identity: "deadbeef"}
	if _, err := store.DeclareOperation(Caller{}, orphan); err != nil {
		t.Fatal(err)
	}
	if got := declarerNamed(t, store, "articles.orphan", 1); got != nil {
		t.Errorf("a declaration submitted with a forged declarer through an EMPTY caller came back as kind %q identity %q, want none — either the submitted value is kept when the store has nothing of its own, or an empty caller is being written down as a declarer", got.Kind, got.Identity)
	}
}

// TestEachVersionRecordsTheIdentityThatDeclaredThatVersion: redeclaring does
// not rewrite history.
//
// Old versions are kept rather than replaced — that is what makes an audit
// record naming an operation version readable years later — so each of them
// has to carry the identity that made THAT one. Read back by number, because
// version 0 is the newest and would hide a version 1 that had been overwritten.
func TestEachVersionRecordsTheIdentityThatDeclaredThatVersion(t *testing.T) {
	_, store := fresh(t, 902)
	if _, err := store.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}

	first := byAuthor()
	first.Name = "articles.shared"
	stored, err := store.DeclareOperation(Caller{Actor: "ann@acme"}, first)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 {
		t.Fatalf("the first declaration is version %d, want 1 — this test's version numbers are wrong from here on", stored.Version)
	}

	second := byAuthor()
	second.Name = "articles.shared"
	second.Limit = 7
	stored, err = store.DeclareOperation(Caller{Actor: "bo@acme"}, second)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 {
		t.Fatalf("the redeclaration is version %d, want 2", stored.Version)
	}

	third := byAuthor()
	third.Name = "articles.shared"
	third.Limit = 9
	if _, err := store.DeclareOperation(Caller{Signer: "9f2c"}, third); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		version  int
		kind     string
		identity string
	}{
		{1, "actor", "ann@acme"},
		{2, "actor", "bo@acme"},
		{3, "key", "9f2c"},
	} {
		got := declarerNamed(t, store, "articles.shared", want.version)
		if got == nil {
			t.Errorf("version %d came back with no declarer, want kind %q identity %q", want.version, want.kind, want.identity)
			continue
		}
		if got.Kind != want.kind || got.Identity != want.identity {
			t.Errorf("version %d is recorded as kind %q identity %q, want kind %q identity %q — a redeclaration is rewriting the versions before it",
				want.version, got.Kind, got.Identity, want.kind, want.identity)
		}
	}

	// And the latest is the latest: version 0 reaches the newest one, which is
	// the third identity rather than the first.
	if got := declarerNamed(t, store, "articles.shared", 0); got == nil || got.Identity != "9f2c" {
		t.Errorf("the latest version is recorded against %v, want the identity that declared version 3", got)
	}
}

// TestTheDeclarerSurvivesADumpAndRestore is why the field is on the
// declaration at all.
//
// The change log already says who declared what — DeclareOperation writes an
// Attribution — and a dump carries none of the log: dump.go's `line` has no
// attribution on it and Restore handles only collections, operations and
// documents. So the log's answer is gone the moment a database is rebuilt from
// a dump, which is the ordinary way a replica comes into existence. This is the
// measurement that says the field's answer is not.
func TestTheDeclarerSurvivesADumpAndRestore(t *testing.T) {
	_, primary := fresh(t, 903)
	if _, err := primary.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}

	first := byAuthor()
	first.Name = "articles.shared"
	if _, err := primary.DeclareOperation(Caller{Actor: "ann@acme"}, first); err != nil {
		t.Fatal(err)
	}
	second := byAuthor()
	second.Name = "articles.shared"
	second.Limit = 7
	if _, err := primary.DeclareOperation(Caller{Signer: "9f2c"}, second); err != nil {
		t.Fatal(err)
	}
	anonymous := byAuthor()
	anonymous.Name = "articles.anonymous"
	if _, err := primary.DeclareOperation(Caller{}, anonymous); err != nil {
		t.Fatal(err)
	}
	if err := primary.Commit(); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if _, err := primary.Dump(out); err != nil {
		t.Fatal(err)
	}

	// The dump has to be carrying it as text, or what is read back on the
	// other side could be coming from anywhere. Both halves asserted: the
	// identity that was recorded is in the bytes, and the one that was never
	// recorded is not.
	dumped := out.String()
	if !strings.Contains(dumped, `"identity":"ann@acme"`) {
		t.Errorf("the dump does not carry the actor that declared articles.shared version 1")
	}
	if !strings.Contains(dumped, `"identity":"9f2c"`) {
		t.Errorf("the dump does not carry the signing key that declared articles.shared version 2")
	}
	if strings.Contains(dumped, `"identity":""`) {
		t.Errorf("the dump carries a declarer with an empty identity, which is a claim about a caller that named nobody")
	}

	_, replica := fresh(t, 904)
	if _, err := replica.Restore(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Read back out of the RESTORED database, by version number.
	for _, want := range []struct {
		name     string
		version  int
		kind     string
		identity string
	}{
		{"articles.shared", 1, "actor", "ann@acme"},
		{"articles.shared", 2, "key", "9f2c"},
	} {
		got := declarerNamed(t, replica, want.name, want.version)
		if got == nil {
			t.Errorf("%s version %d lost its declarer in the dump and restore, want kind %q identity %q", want.name, want.version, want.kind, want.identity)
			continue
		}
		if got.Kind != want.kind || got.Identity != want.identity {
			t.Errorf("%s version %d came out of the restore as kind %q identity %q, want kind %q identity %q",
				want.name, want.version, got.Kind, got.Identity, want.kind, want.identity)
		}
	}
	if got := declarerNamed(t, replica, "articles.anonymous", 1); got != nil {
		t.Errorf("an operation declared by nobody came out of the restore as kind %q identity %q — the restore is inventing a declarer", got.Kind, got.Identity)
	}

	// The restore is not a special path that only declarations reach: the
	// operation still runs afterwards, so what came back is the whole
	// declaration and not a husk carrying one field.
	notes := map[string]any{"id": "k1", "author": "ann", "published": 1.0, "title": "t", "slug": "s1"}
	collection, err := replica.Collection("articles")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collection.Put(notes); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.Invoke(Caller{}, "articles.shared", 1, map[string]any{"author": "ann"}); err != nil {
		t.Fatalf("the restored declaration does not run: %v", err)
	}
}

// TestADeclarationStoredWithoutADeclarerStaysWithoutOne: absent is a state
// this field has to read correctly forever.
//
// Every operation declared before ISS-32 is stored as JSON with no declaredBy
// in it, and those bytes are inside databases nobody is going to re-apply. An
// old declaration read back must not grow a value it never had, and a new one
// with nothing to record must not write an empty one.
//
// The JSON here is hand-written rather than produced by marshaling an
// Operation: a fixture generated from the type under test agrees with that
// type however the type changes, which is the one thing this test must not do.
func TestADeclarationStoredWithoutADeclarerStaysWithoutOne(t *testing.T) {
	const older = `{"name":"articles.old","collection":"articles","action":"scan","index":"by_author","limit":3,"version":1}`

	operation := Operation{}
	if err := json.Unmarshal([]byte(older), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Name != "articles.old" {
		t.Fatalf("the fixture decoded into an operation named %q — it is not being read at all, so nothing below is measured", operation.Name)
	}
	if operation.DeclaredBy != nil {
		t.Errorf("a declaration stored before this field existed reads back with declarer kind %q identity %q", operation.DeclaredBy.Kind, operation.DeclaredBy.Identity)
	}

	// The positive control for the line above: the same shape WITH the field
	// present has to come back carrying it, or the nil result proves only that
	// the field is never populated by anything.
	const newer = `{"name":"articles.new","collection":"articles","action":"scan","index":"by_author","limit":3,"version":1,"declaredBy":{"kind":"actor","identity":"ann@acme"}}`
	operation = Operation{}
	if err := json.Unmarshal([]byte(newer), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.DeclaredBy == nil {
		t.Fatal(`a declaration carrying "declaredBy" reads back with none — the json tag does not match what is written`)
	}
	if operation.DeclaredBy.Kind != "actor" || operation.DeclaredBy.Identity != "ann@acme" {
		t.Errorf(`"declaredBy":{"kind":"actor","identity":"ann@acme"} read back as kind %q identity %q`, operation.DeclaredBy.Kind, operation.DeclaredBy.Identity)
	}

	// And the other direction, both ways round. A nil declarer writes no key
	// at all, so a 1.0 reader meets exactly the bytes it met before; a present
	// one writes the key under the spelling above. The second half is what
	// stops the first from passing because the field never marshals.
	encoded, err := json.Marshal(Operation{Name: "articles.old", Collection: "articles", Action: ActionScan, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "declaredBy") {
		t.Errorf("an operation with no declarer marshaled as %s — a declaration that records nobody is writing a field anyway", encoded)
	}
	encoded, err = json.Marshal(Operation{
		Name: "articles.new", Collection: "articles", Action: ActionScan, Version: 1,
		DeclaredBy: &Declarer{Kind: "actor", Identity: "ann@acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"declaredBy":{"kind":"actor","identity":"ann@acme"}`) {
		t.Errorf(`an operation with a declarer marshaled as %s, which does not hold "declaredBy":{"kind":"actor","identity":"ann@acme"}`, encoded)
	}
}

// TestRecordingADeclarerChangesNoNamespaceAnswer: this field is a record, not
// a rule.
//
// The risk in adding an identity to a declaration is that something starts
// reading it to decide who may declare. Namespace ownership is settled by
// claimFor against spaceClaims and by nothing else, and the two facts can
// disagree — a namespace claimed by ann can be declared into again by ann
// whatever any single version records, and a stranger is refused whatever the
// declaration they submitted said about itself. Both measured here, because a
// refusal that started consulting this field would still look right in the
// ordinary case.
func TestRecordingADeclarerChangesNoNamespaceAnswer(t *testing.T) {
	_, store := fresh(t, 905)
	if _, err := store.Declare(Caller{}, articles()); err != nil {
		t.Fatal(err)
	}

	mine := byAuthorNamed("acme:orders")
	if _, err := store.DeclareOperation(Caller{Actor: "ann@acme"}, mine); err != nil {
		t.Fatal(err)
	}

	// The holder comes back. Redeclaring is still allowed for them.
	again := byAuthorNamed("acme:orders")
	again.Limit = 5
	if _, err := store.DeclareOperation(Caller{Actor: "ann@acme"}, again); err != nil {
		t.Fatalf("the namespace's own holder was refused a redeclaration: %v", err)
	}

	// A stranger is still refused, and submitting the holder's identity in the
	// declaration does not help: the claim record is what is consulted.
	impostor := byAuthorNamed("acme:orders")
	impostor.DeclaredBy = &Declarer{Kind: "actor", Identity: "ann@acme"}
	_, err := store.DeclareOperation(Caller{Actor: "mallory"}, impostor)
	if err == nil {
		t.Fatal("a stranger declared into acme: by writing the holder's identity into the declaration — the field is being read as a rule")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("the refusal reads %q, which does not name the namespace", err)
	}

	// And the claim itself is untouched by any of this.
	claim, found, err := store.NamespaceClaim("acme")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the namespace acme is held by nobody after two declarations into it")
	}
	if claim.Kind != "actor" || claim.Identity != "ann@acme" {
		t.Errorf("acme is held by kind %q identity %q, want kind \"actor\" identity \"ann@acme\"", claim.Kind, claim.Identity)
	}
}
