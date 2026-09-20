package bundle

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/store"
)

// keyFrom makes a deterministic key pair, so that a failing test prints the
// same key every time and a fixture could be written from it later.
func keyFrom(t *testing.T, seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = seed
	}
	private := ed25519.NewKeyFromSeed(raw)
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("an ed25519 private key must carry a public half")
	}
	return public, private
}

// catalogue is a bundle with something in both lists: one collection with an
// index and a rollup, and two operations, one of which is scoped. Small, but
// not so small that a field of a declaration is untouched by the signature.
func catalogue() Bundle {
	return Bundle{
		Name:    "acme-catalog",
		Version: "1.4.0",
		Signer:  "ACME Engineering",
		Collections: []store.Spec{{
			Name: "articles",
			Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
			Indexes: []store.Index{{
				Name: "by_published",
				Fields: []store.Field{
					{Path: "published", Type: store.TypeNumber, Descending: true, Missing: store.MissingSkip},
				},
				Include: []string{"title"},
			}},
			Rollups: []store.Rollup{{Name: "per_author", Group: []store.Field{
				{Path: "author", Type: store.TypeString, Missing: store.MissingSkip},
			}, Count: true}},
		}},
		Operations: []store.Operation{
			{
				Name:       "article_by_id",
				Collection: "articles",
				Action:     store.ActionGet,
				Input:      []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
				Key:        &store.Term{Arg: "id"},
			},
			{
				Name:       "recent_articles",
				Collection: "articles",
				Action:     store.ActionScan,
				Index:      "by_published",
				Limit:      50,
				Scopes:     []string{"articles:read"},
				Projection: []string{"id", "title"},
			},
		},
	}
}

func sealed(t *testing.T, key ed25519.PrivateKey) Bundle {
	t.Helper()
	b := catalogue()
	if err := Seal(&b, key); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	return b
}

func trusting(t *testing.T, signers ...TrustedSigner) *Trust {
	t.Helper()
	trust, err := NewTrust(signers)
	if err != nil {
		t.Fatalf("building the trust list: %v", err)
	}
	return trust
}

// TestASignedBundleVerifies is the one everything else is measured against. If
// this goes red, every refusal below is refusing something for the wrong
// reason.
func TestASignedBundleVerifies(t *testing.T) {
	public, private := keyFrom(t, 1)
	b := sealed(t, private)

	if b.Format != Label {
		t.Errorf("Seal must stamp the format: got %q", b.Format)
	}
	if b.Signature.Algorithm != Algorithm {
		t.Errorf("algorithm: got %q, want %q", b.Signature.Algorithm, Algorithm)
	}
	if b.Signature.PublicKey != hex.EncodeToString(public) {
		t.Errorf("the sealed bundle names a key that is not the signing one")
	}
	if len(b.Signature.Signature) != ed25519.SignatureSize*2 {
		t.Errorf("signature: %d hex characters, want %d", len(b.Signature.Signature), ed25519.SignatureSize*2)
	}
	if b.Signature.PublicKey != strings.ToLower(b.Signature.PublicKey) ||
		b.Signature.Signature != strings.ToLower(b.Signature.Signature) {
		t.Error("a sealed bundle must be written in lower-case hex")
	}

	// Through a file, which is how one actually arrives.
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	read, err := Parse(raw)
	if err != nil {
		t.Fatalf("parsing a bundle this package just wrote: %v", err)
	}
	if !reflect.DeepEqual(read, b) {
		t.Error("a bundle did not survive a round trip through JSON")
	}

	trust := trusting(t, TrustedSigner{Label: "acme (from the 2026 key handover)", PublicKey: hex.EncodeToString(public)})
	label, err := trust.Verify(read)
	if err != nil {
		t.Fatalf("a bundle signed by a trusted key must verify: %v", err)
	}
	// The label is the operator's, never the bundle's own claim about itself.
	if label == read.Signer {
		t.Error("this test is not measuring anything if the two names are the same string")
	}
	if !strings.Contains(label, "handover") {
		t.Errorf("Verify must return the operator's label for the key, got %q", label)
	}
}

// TestAChangedByteAnywhereIsRefused is SAPE-10's second acceptance criterion,
// taken literally: every single byte of the file, changed in place, one at a
// time, on the artifact rather than on a struct in memory.
//
// Two bits per byte. 0x01 is the ordinary "something changed" flip. 0x20 is
// the one that turns a letter's case, and it is here because it is the only
// edit a reader would assume is covered and might not be: hex.Decode reads
// "AB" and "ab" as the same byte, and encoding/json matches an object's keys
// case-insensitively, so both are places where a file could have two
// spellings. The first is closed by lowerHex; the second is not closed at all
// and cannot be from here, so this test states it rather than not reaching it.
//
// What "refused" means is therefore made exact rather than assumed. A change
// that leaves the PARSED bundle identical — `"format"` written `"Format"`, the
// case of a key encoding/json was going to fold anyway — changed the file and
// not the declarations, and it must still verify, for the same reason
// re-indenting the file must. Every other change must be refused, by
// ErrBundle or ErrUnsigned or ErrBadSignature; which of the three does not
// matter, and a change that is accepted is a hole whatever it touched.
//
// The positive control is the unflipped file at the top: if that does not
// verify, every refusal below is free.
func TestAChangedByteAnywhereIsRefused(t *testing.T) {
	public, private := keyFrom(t, 2)
	b := sealed(t, private)
	trust := trusting(t, TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(public)})

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	// The control. Same bytes, untouched.
	original, err := Parse(raw)
	if err != nil {
		t.Fatalf("the unflipped file must parse: %v", err)
	}
	if _, err := trust.Verify(original); err != nil {
		t.Fatalf("the unflipped file must verify, or the %d refusals below measure nothing: %v", len(raw), err)
	}

	var malformed, refused, cosmetic, accepted, changes int
	for i := range raw {
		for _, bit := range []byte{0x01, 0x20} {
			flipped := make([]byte, len(raw))
			copy(flipped, raw)
			flipped[i] ^= bit
			changes++

			read, err := Parse(flipped)
			if err != nil {
				if !errors.Is(err, ErrBundle) {
					t.Fatalf("byte %d: a file that will not parse must be ErrBundle, got %v", i, err)
				}
				malformed++
				continue
			}

			// A change the parser folds away is a change to the file's
			// spelling and not to what it declares, and it is held to the
			// opposite standard: it MUST still verify.
			same := reflect.DeepEqual(read, original)
			_, err = trust.Verify(read)
			switch {
			case same && err != nil:
				t.Errorf("byte %d (%q -> %q) declares exactly what the original did and was refused anyway: %v",
					i, raw[i], flipped[i], err)
			case same:
				cosmetic++
			case err == nil:
				accepted++
				t.Errorf("byte %d (%q -> %q) changed a declaration and the bundle still verified", i, raw[i], flipped[i])
			case errors.Is(err, ErrBundle), errors.Is(err, ErrBadSignature), errors.Is(err, ErrUnsigned):
				refused++
			default:
				t.Errorf("byte %d: refused for an unexpected reason: %v", i, err)
			}
		}
	}

	if accepted != 0 {
		t.Fatalf("%d of %d single-byte changes altered a declaration and went unnoticed", accepted, changes)
	}
	t.Logf("%d bytes, %d single-byte changes: %d would not parse, %d refused, %d folded away by the JSON parser and still verify, %d accepted",
		len(raw), changes, malformed, refused, cosmetic, accepted)
}

// TestAnUntrustedKeyIsRefusedSeparately: the file is perfect, and the answer
// is still no. This and the signature refusal must never collapse into one
// code — an operator whose bundle is intact but signed by a stranger has to
// add a key, and one whose bundle was altered has to fetch it again.
func TestAnUntrustedKeyIsRefusedSeparately(t *testing.T) {
	trustedPublic, _ := keyFrom(t, 3)
	strangerPublic, strangerPrivate := keyFrom(t, 4)

	b := sealed(t, strangerPrivate)
	trust := trusting(t, TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(trustedPublic)})

	// The control: the file itself is sound, so the refusal below is about
	// the key and nothing else.
	if _, err := Check(b); err != nil {
		t.Fatalf("the stranger's bundle must be intact, or this test measures a broken file: %v", err)
	}

	_, err := trust.Verify(b)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a bundle from an unknown key must be ErrUntrusted, got %v", err)
	}
	if errors.Is(err, ErrBadSignature) {
		t.Error("an unknown signer must not be reported as a bad signature")
	}
	if !strings.Contains(err.Error(), hex.EncodeToString(strangerPublic)) {
		t.Errorf("the refusal must name the key an operator would have to add: %v", err)
	}

	// And the same bundle, once that key is on the list.
	wider := trusting(t,
		TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(trustedPublic)},
		TrustedSigner{Label: "the stranger", PublicKey: hex.EncodeToString(strangerPublic)})
	if label, err := wider.Verify(b); err != nil || label != "the stranger" {
		t.Errorf("adding the key must be the whole of the fix: %q, %v", label, err)
	}
}

// TestAnUnsignedBundleIsRefused: SAPE-10 asks for "signature absent" to be its
// own answer, distinct from "signature does not verify".
func TestAnUnsignedBundleIsRefused(t *testing.T) {
	public, _ := keyFrom(t, 5)
	trust := trusting(t, TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(public)})

	for _, unsigned := range []Bundle{
		func() Bundle { b := catalogue(); b.Format = Label; return b }(),
		func() Bundle {
			b := catalogue()
			b.Format = Label
			b.Signature = &Signature{Algorithm: Algorithm, PublicKey: hex.EncodeToString(public)}
			return b
		}(),
	} {
		if _, err := trust.Verify(unsigned); !errors.Is(err, ErrUnsigned) {
			t.Errorf("an unsigned bundle must be ErrUnsigned, got %v", err)
		}
	}

	// A bundle with no format line at all is not an unsigned bundle, it is
	// not a bundle. Both are refusals and they are not the same refusal.
	if _, err := trust.Verify(catalogue()); !errors.Is(err, ErrBundle) {
		t.Errorf("a file with no format line must be ErrBundle, got %v", err)
	}
}

// TestAnEmptyTrustListRefusesEverythingAndSaysSo is SAPE-10's seventh
// criterion. The bundle below is flawless; the server is the problem.
func TestAnEmptyTrustListRefusesEverythingAndSaysSo(t *testing.T) {
	public, private := keyFrom(t, 6)
	b := sealed(t, private)

	empty := trusting(t)
	if empty.Signers() != 0 {
		t.Fatal("this list is supposed to be empty")
	}
	_, err := empty.Verify(b)
	if !errors.Is(err, ErrNoTrust) {
		t.Fatalf("an empty trust list must fail closed with ErrNoTrust, got %v", err)
	}
	if errors.Is(err, ErrUntrusted) {
		t.Error("an empty list is a server that trusts nobody, not a key that is missing from a list")
	}
	if !strings.Contains(err.Error(), "no signer is trusted") {
		t.Errorf("the refusal must say nobody is trusted, in those words: %v", err)
	}

	// The control: the same bundle, the same everything, one key added.
	one := trusting(t, TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(public)})
	if _, err := one.Verify(b); err != nil {
		t.Fatalf("the bundle itself must be fine, or the refusal above is about the wrong thing: %v", err)
	}
}

// naive is the delimiter-joined encoding: the obvious thing to write when
// nobody stops to ask, and the form internal/signing had to abandon for a
// grant. It is here only as the control for the sweep below.
func naive(b Bundle, key ed25519.PublicKey) string {
	parts := []string{b.Name, b.Version, b.Signer, hex.EncodeToString(key)}
	for _, spec := range b.Collections {
		encoded, _ := json.Marshal(spec)
		parts = append(parts, string(encoded))
	}
	for _, operation := range b.Operations {
		encoded, _ := json.Marshal(operation)
		parts = append(parts, string(encoded))
	}
	return Label + "\n" + strings.Join(parts, ":")
}

// TestNoTwoBundlesShareOneMessage is the collision test, and it is a security
// test rather than a formatting one: two different bundles that write the same
// bytes are two bundles with one signature, so a signature over the first is a
// signature over the second.
//
// It sweeps a grid rather than asserting a handful of pairs, because a grid
// can be counted and a handful can only be argued about — and it runs the
// delimiter-joined encoding over the identical grid as a positive control. A
// sweep that finds no collision proves nothing unless the same sweep finds
// them in a form that has them.
func TestNoTwoBundlesShareOneMessage(t *testing.T) {
	public, private := keyFrom(t, 7)

	// Values that can be split two ways across a ":" delimiter: "a:b" then
	// "c" writes what "a" then "b:c" writes. Nothing here is exotic — a
	// version string of "1:2" and a bundle name holding a ":" are both things
	// an author may reasonably write, and neither is banned, because the
	// encoding counts bytes instead of trusting a character table.
	alphabet := []string{"a", "b", "c", "a:b", "b:c", "a:b:c", "1", "1:2"}

	spec := func(name string) store.Spec {
		return store.Spec{Name: name, Key: store.Key{Path: "id", Type: store.TypeString}, Indexes: []store.Index{}}
	}
	operation := func(name string) store.Operation {
		return store.Operation{Name: name, Collection: "c", Action: store.ActionGet, Key: &store.Term{Arg: "id"}}
	}
	collectionShapes := [][]store.Spec{
		nil,
		{spec("one")},
		{spec("two")},
		{spec("one"), spec("two")},
	}
	operationShapes := [][]store.Operation{
		nil,
		{operation("read")},
		{operation("write")},
		{operation("read"), operation("write")},
	}

	type tuple struct {
		name, version, signer string
		collections           int
		operations            int
	}

	var tuples []tuple
	for _, name := range alphabet {
		for _, version := range alphabet {
			for _, signer := range alphabet {
				for c := range collectionShapes {
					for o := range operationShapes {
						tuples = append(tuples, tuple{name, version, signer, c, o})
					}
				}
			}
		}
	}

	build := func(item tuple) Bundle {
		return Bundle{
			Name: item.name, Version: item.version, Signer: item.signer,
			Collections: collectionShapes[item.collections],
			Operations:  operationShapes[item.operations],
		}
	}

	// A sweep whose rows are secretly the same tuple would agree with itself
	// and measure nothing.
	seen := make(map[tuple]bool, len(tuples))
	for _, item := range tuples {
		if seen[item] {
			t.Fatalf("the grid lists %+v twice, so its collision count is meaningless", item)
		}
		seen[item] = true
	}

	count := func(encode func(Bundle, ed25519.PublicKey) string) (int, [][2]tuple) {
		byMessage := make(map[string]tuple, len(tuples))
		var pairs [][2]tuple
		collisions := 0
		for _, item := range tuples {
			message := encode(build(item), public)
			if first, already := byMessage[message]; already {
				collisions++
				if len(pairs) < 3 {
					pairs = append(pairs, [2]tuple{first, item})
				}
				continue
			}
			byMessage[message] = item
		}
		return collisions, pairs
	}

	naiveCollisions, examples := count(naive)
	// The positive control. If the delimiter-joined form has no collisions in
	// this grid then the grid cannot detect one, and the zero below is an
	// artefact of the sweep rather than a property of the encoding.
	if naiveCollisions == 0 {
		t.Fatal("the delimiter-joined control found no collision, so this grid cannot detect one at all")
	}
	t.Logf("%d tuples: the delimiter-joined form collides %d times, e.g. %+v", len(tuples), naiveCollisions, examples)

	v1Collisions, found := count(func(b Bundle, key ed25519.PublicKey) string {
		message, err := Message(b, key)
		if err != nil {
			t.Fatalf("building a message for %+v: %v", b, err)
		}
		return message
	})
	if v1Collisions != 0 {
		t.Fatalf("%d of %d tuples share a signed message under %s, e.g. %+v", v1Collisions, len(tuples), Label, found)
	}
	t.Logf("%d tuples: %s collides %d times", len(tuples), Label, v1Collisions)

	// The bytes are one half. The signatures are the half that matters: a
	// signature minted for one of a colliding pair must not verify as the
	// other.
	for _, pair := range examples {
		one, two := build(pair[0]), build(pair[1])
		if err := Seal(&one, private); err != nil {
			t.Fatal(err)
		}
		if _, err := Check(one); err != nil {
			t.Fatalf("the first bundle does not verify as itself (%v) — the refusal below measures nothing", err)
		}
		two.Format = Label
		two.Signature = one.Signature
		if _, err := Check(two); !errors.Is(err, ErrBadSignature) {
			t.Errorf("a signature over %+v verified as %+v: %v", pair[0], pair[1], err)
		}
	}
}

// TestTheMemberCountIsWhatKeepsTwoListsApart measures the hazard a grant did
// not have, at the level where it lives.
//
// A bundle holds two lists, not five scalars. Putting a byte count in front of
// every member stops one member running into the next; it does nothing at all
// about one LIST running into the next, and the two are different bugs. Two
// collections and no operations must not write what one collection and one
// operation write.
//
// Today the two lists hold different Go types whose JSON can never be equal,
// so a bundle cannot reach this collision by accident. That is a proof about
// two structs and not a property of the format — exactly the sort of argument
// internal/signing had to redo from scratch each time a grant grew a field —
// and it stops holding the moment a third list of declarations is added. This
// test is against the encoder, where the property actually is.
func TestTheMemberCountIsWhatKeepsTwoListsApart(t *testing.T) {
	uncounted := func(values []string) string {
		message := ""
		for _, value := range values {
			message += counted(value)
		}
		return message
	}

	for _, pair := range []struct {
		name     string
		one, two [2][]string
	}{
		{"a member moved from the first list to the second",
			[2][]string{{"A", "B"}, nil},
			[2][]string{{"A"}, {"B"}}},
		{"an empty first list against a first list holding everything",
			[2][]string{nil, {"A", "B"}},
			[2][]string{{"A", "B"}, nil}},
		{"one member against none, with the tail unchanged",
			[2][]string{{"A"}, {"B"}},
			[2][]string{nil, {"A", "B"}}},
	} {
		t.Run(pair.name, func(t *testing.T) {
			if reflect.DeepEqual(pair.one, pair.two) {
				t.Fatal("this row is one pair of lists written twice and cannot collide")
			}
			// The hazard, measured rather than asserted: without the count,
			// these two really do write the same bytes.
			bad := uncounted(pair.one[0]) + uncounted(pair.one[1])
			alsoBad := uncounted(pair.two[0]) + uncounted(pair.two[1])
			if bad != alsoBad {
				t.Fatalf("this row does not collide without the count either (%q vs %q), so the count below is not what keeps it apart", bad, alsoBad)
			}

			good := section(pair.one[0]) + section(pair.one[1])
			alsoGood := section(pair.two[0]) + section(pair.two[1])
			if good == alsoGood {
				t.Errorf("two different pairs of lists write the same bytes %q", good)
			}
		})
	}
}

// TestReformattingTheFileDoesNotBreakTheSignature is SAPE-10's sixth
// criterion, and it is the other half of the byte-flip test above. The
// signature covers declarations, not the bytes they arrived in, so a bundle
// that has been pretty-printed, re-ordered inside a JSON object, or moved
// through something that rewrote its whitespace still verifies — while any
// byte of any declaration changing does not.
func TestReformattingTheFileDoesNotBreakTheSignature(t *testing.T) {
	public, private := keyFrom(t, 8)
	b := sealed(t, private)
	trust := trusting(t, TrustedSigner{Label: "acme", PublicKey: hex.EncodeToString(public)})

	compact, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(b, "\t", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if string(compact) == string(indented) {
		t.Fatal("these two spellings are the same file, so this test measures nothing")
	}

	for _, spelling := range [][]byte{compact, indented, append(append([]byte("\n  "), indented...), '\n')} {
		read, err := Parse(spelling)
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		if _, err := trust.Verify(read); err != nil {
			t.Errorf("a reformatted bundle must still verify: %v", err)
		}
	}
}

// TestAFieldThisVersionDoesNotKnowIsRefused: nothing may be carried past
// verification by hiding it where this version does not look.
func TestAFieldThisVersionDoesNotKnowIsRefused(t *testing.T) {
	_, private := keyFrom(t, 9)
	b := sealed(t, private)

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	// The control: as written, it parses.
	if _, err := Parse(raw); err != nil {
		t.Fatalf("the file as written must parse: %v", err)
	}

	for _, tampered := range []string{
		`{"unheard_of":1,` + strings.TrimPrefix(string(raw), "{"),
		string(raw) + string(raw),
	} {
		if _, err := Parse([]byte(tampered)); !errors.Is(err, ErrBundle) {
			t.Errorf("must be refused as ErrBundle, got %v", err)
		}
	}
}

// TestABundleCarryingTheStoresOwnNumbersIsRefused. An index id, a collection
// id and an operation version are handed out by the database a bundle is
// installed into, and they are written into that database's keys. An author on
// another machine cannot know them. Signing one would mean a field inside the
// signature that the installer then ignores.
func TestABundleCarryingTheStoresOwnNumbersIsRefused(t *testing.T) {
	public, _ := keyFrom(t, 10)

	// The control first: the same bundle with all of them at zero.
	if _, err := Message(catalogue(), public); err != nil {
		t.Fatalf("a declaration carrying no store numbers must sign: %v", err)
	}

	for _, claimed := range []struct {
		what string
		make func() Bundle
	}{
		{"a collection id", func() Bundle { b := catalogue(); b.Collections[0].ID = 4; return b }},
		{"the index counter", func() Bundle { b := catalogue(); b.Collections[0].NextIndexID = 1; return b }},
		{"the rollup counter", func() Bundle { b := catalogue(); b.Collections[0].NextRollupID = 1; return b }},
		{"an index id", func() Bundle { b := catalogue(); b.Collections[0].Indexes[0].ID = 2; return b }},
		{"a rollup id", func() Bundle { b := catalogue(); b.Collections[0].Rollups[0].ID = 2; return b }},
		{"an operation version", func() Bundle { b := catalogue(); b.Operations[0].Version = 3; return b }},
	} {
		t.Run(claimed.what, func(t *testing.T) {
			if _, err := Message(claimed.make(), public); !errors.Is(err, ErrBundle) {
				t.Errorf("a bundle claiming %s must be ErrBundle, got %v", claimed.what, err)
			}
		})
	}
}

// TestAKeyThatIsNotOneIsRefused covers both sides of the key: an operator's
// configuration, and the key written inside a bundle. They are different
// refusals on purpose — one is a line in a config file to fix, the other is a
// file to throw away.
func TestAKeyThatIsNotOneIsRefused(t *testing.T) {
	good := hex.EncodeToString(func() ed25519.PublicKey { key, _ := keyFrom(t, 11); return key }())

	// The control.
	if _, err := NewTrust([]TrustedSigner{{Label: "acme", PublicKey: good}}); err != nil {
		t.Fatalf("a real key must be accepted: %v", err)
	}

	for _, bad := range []string{"", "zz", good[:62], good + "00", strings.ToUpper(good)} {
		if _, err := NewTrust([]TrustedSigner{{Label: "acme", PublicKey: bad}}); !errors.Is(err, ErrKey) {
			t.Errorf("%q must be refused as ErrKey, got %v", bad, err)
		}
	}
	// The same key twice under two names is a list an operator cannot reason
	// about: one of the two labels would silently never be printed.
	if _, err := NewTrust([]TrustedSigner{{Label: "acme", PublicKey: good}, {Label: "also acme", PublicKey: good}}); !errors.Is(err, ErrKey) {
		t.Errorf("one key under two labels must be refused, got %v", err)
	}

	// And inside a bundle, where a bad key is a bad file rather than a bad
	// configuration.
	_, private := keyFrom(t, 12)
	for _, bad := range []string{"", "zz", strings.ToUpper(good)} {
		b := sealed(t, private)
		b.Signature.PublicKey = bad
		if _, err := Check(b); !errors.Is(err, ErrBundle) {
			t.Errorf("a bundle naming %q as its key must be ErrBundle, got %v", bad, err)
		}
	}
	// A substituted key that IS a key is a signature that does not verify,
	// because the key is part of the message.
	b := sealed(t, private)
	b.Signature.PublicKey = good
	if _, err := Check(b); !errors.Is(err, ErrBadSignature) {
		t.Errorf("swapping the key for another real one must be ErrBadSignature, got %v", err)
	}
}

// TestTheAlgorithmIsCheckedRatherThanAssumed. A field nobody reads is a field
// an attacker may write, and this one would otherwise be the single place in a
// bundle where a byte can change freely.
func TestTheAlgorithmIsCheckedRatherThanAssumed(t *testing.T) {
	_, private := keyFrom(t, 13)
	b := sealed(t, private)
	if _, err := Check(b); err != nil {
		t.Fatalf("the control must verify: %v", err)
	}
	for _, algorithm := range []string{"", "ED25519", "hmac-sha256", "none"} {
		tampered := sealed(t, private)
		tampered.Signature.Algorithm = algorithm
		if _, err := Check(tampered); !errors.Is(err, ErrBundle) {
			t.Errorf("algorithm %q must be refused as ErrBundle, got %v", algorithm, err)
		}
	}
}

// TestTheMessageIsWhatItSaysItIs pins the encoding against its own
// documentation, byte for byte. Everything else in this file would stay green
// if the format changed shape, as long as it changed on both sides at once;
// this is the row that would go red.
func TestTheMessageIsWhatItSaysItIs(t *testing.T) {
	key := make([]byte, ed25519.PublicKeySize)
	for i := range key {
		key[i] = 0xab
	}
	keyHex := hex.EncodeToString(key)

	spec := store.Spec{Name: "c", Key: store.Key{Path: "id", Type: store.TypeString}, Indexes: []store.Index{}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	b := Bundle{Name: "n:1", Version: "1:2", Signer: "s", Collections: []store.Spec{spec}}
	message, err := Message(b, key)
	if err != nil {
		t.Fatal(err)
	}

	want := Label + "\n" +
		"3:n:1\n" +
		"3:1:2\n" +
		"1:s\n" +
		fmt.Sprintf("%d:%s\n", len(keyHex), keyHex) +
		"1:1\n" +
		fmt.Sprintf("%d:%s\n", len(specJSON), specJSON) +
		"1:0\n"

	if message != want {
		t.Errorf("the message is not what the doc comment says:\n got  %q\n want %q", message, want)
	}
}
