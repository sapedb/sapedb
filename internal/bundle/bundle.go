// Package bundle is a set of declarations that arrived from somebody else,
// and the two questions a server has to answer before it stores one: who
// signed this, and is it still the thing they signed.
//
// # What a bundle is
//
// A bundle is an apply file with a name, an author and a signature on it.
// That is the whole of the decision, and it was made by reading what already
// exists rather than by inventing a container: internal/cli's `schema` — the
// thing `sapedb apply FILE` reads — is
//
//	{"collections": [store.Spec...], "operations": [store.Operation...]}
//
// and those two lists are exactly what an external operation module needs to
// be. A Spec says what a collection is, what its primary key is, which indexes
// and rollups it carries and how it is partitioned. An Operation says which
// collection it touches, which index it walks, how far, what it returns and
// which scopes a caller must hold. Declared operations are already the only
// way into a database, so a bundle that carries declarations carries
// everything a caller can ever do.
//
// The project's R&D note called the v1 shape "collections, operations,
// shapes". There is no shape in this repository at this commit — the only
// `Shape` in the non-test tree is store.sameShape, a comparison between two
// indexes — so v1 carries the two lists that exist. A third list can be added
// later without re-opening the encoding, which is one of the reasons the
// encoding below counts its members.
//
// # What a bundle deliberately cannot express
//
//   - Code. There is no custom action, no expression language, no trigger, no
//     validator, no hook, no plugin, no WASM. The ten actions in
//     store.ops.go are the whole vocabulary, and a bundle that wants an
//     eleventh cannot have one. This is why v1 can get away with signing rather than
//     sandboxing: a verified bundle can widen what a database holds, but not
//     what the server is able to do. The worst a trusted-but-hostile bundle
//     achieves is declarations that cost more to serve than the operator
//     expected — a capacity problem, not a code-execution one.
//   - Data. No seed documents, no fixtures. A bundle declares shapes and
//     leaves them empty.
//   - Removal. A bundle adds declarations. It cannot say "drop this
//     collection" or "retire that operation", so there is no uninstall and
//     nothing here is reversible by presenting a different bundle.
//   - Namespacing. The bundle's name is not a prefix on anything it declares.
//     Two bundles from two authors that both declare "orders" are in a fight,
//     and the fight is settled at install time by store.ErrIncompatible, not
//     here. Verification says nothing about whether a bundle will install.
//   - Dependencies. No bundle requires another, no bundle pins a version of
//     another, and nothing orders two bundles against each other.
//   - Freshness. A bundle carries a version string but nothing compares it
//     against anything. A correctly signed old bundle verifies forever, so
//     handing somebody last month's copy is not detected here. A grant solved
//     the same problem with a mandatory expiry (see internal/signing); an
//     install artifact cannot borrow that, because the moment it expires the
//     thing already installed from it does not.
//   - Where it may be installed. A grant names an account and a database, so
//     it is a permission HERE. A bundle names neither, on purpose: the same
//     bundle is meant to install on every server whose operator trusts its
//     author.
//
// # Whose key, and what that proves
//
// Signatures are ed25519, not the HMAC the rest of this repository uses, and
// the reason is the whole point of the ticket. Every existing signature here —
// a connection string, an operator proof, a scope grant — is made with the
// server's own secret, and so it proves the signer held that secret. A bundle
// comes from outside. Signing it with the server's secret would prove that the
// server signed a file it received, which is not a fact about its author and
// not worth a signature. So the author keeps a private key nobody else has,
// and the server keeps a list of public keys an operator put there.
//
// What a verified bundle therefore proves is narrow and worth stating exactly:
//
//	the declarations in this file are byte-for-byte the ones that the holder
//	of this 32-byte public key signed, and an operator of this server wrote
//	that key into its configuration.
//
// What it does not prove:
//
//   - Who that is in the world. There is no PKI, no certificate, no name
//     binding, no directory. "Trusted" means an operator pasted 64 hex
//     characters into a config file. The Signer field in the bundle is signed,
//     so nobody can change it in transit, but it is written by whoever holds
//     the key and can say anything at all. Trust flows from the key and only
//     from the key; the name is for reading. Verify returns the label THIS
//     operator wrote beside the key, not the name the bundle claims, so the
//     two can be compared by a person.
//   - That the key is still the author's. There is no revocation and no
//     rotation in v1 — both are out of scope for SAPE-10 and neither is
//     simulated by anything here. A key that leaks stays trusted until an
//     operator deletes the line.
//   - That the declarations are good, or safe, or installable. Verification
//     is about origin and integrity. store.Declare is what decides whether a
//     declaration can be stored, and it runs later.
//
// So v1 is narrower than "anyone can publish a bundle". It is: an operator
// decides, by hand, whose bundles this server will look at. Nothing here
// implies more trust than it checks, and there is deliberately no fallback —
// an empty list of trusted signers refuses every bundle and says so in those
// words rather than accepting anything.
//
// # What the signature covers
//
// Not the bytes of the file. The signed message is built from the
// declarations as this version of sapedb understands them: the file is parsed
// into store.Spec and store.Operation and re-marshalled, so JSON whitespace,
// key order inside a map, file permissions, timestamps and whatever an
// archive did to any of that are outside the signature. That is what makes a
// signature survive being moved between machines. What it costs is that "a
// byte changed" and "a declaration changed" are not quite the same sentence:
// re-indenting a bundle keeps it valid, while changing any byte that any
// declaration is made of does not.
//
// A field this version does not know is refused rather than dropped — Parse
// uses DisallowUnknownFields, the same as `sapedb apply` already does — so
// there is no way to carry something past verification by putting it
// somewhere this version ignores.
//
// Hex is required to be lower case, and a bundle whose key or signature is
// written in upper case is refused. internal/signing goes the other way for
// connection strings, where hex case is explicitly not part of the contract,
// and the difference is deliberate: a connection string is typed by a person,
// while a bundle is a file that must not have a second spelling — hex.Decode
// reads "AB" and "ab" as the same byte, so accepting both would mean a byte of
// this file could be changed without the signature noticing.
package bundle

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sapedb/sapedb/internal/store"
)

// Label is both the format string a bundle must declare and the first line of
// the signed message — one string rather than two, the same choice
// internal/signing made for GrantLabel. A message that names the format it
// belongs to cannot be replayed into a verifier reading a different one, and
// there is only one version spelling to get wrong when this changes.
const Label = "sapedb/bundle:v1"

// Algorithm is the only signature algorithm v1 knows. It is checked rather
// than assumed: a field nobody reads is a field an attacker may write.
const Algorithm = "ed25519"

// TextBytes is the longest a bundle's name, version or signer may be. The
// limit exists so an error message can print one, not because the encoding
// needs it — the encoding counts bytes and does not care how many there are.
const TextBytes = 128

var (
	// ErrBundle is a file that is not a bundle this version can read: the
	// wrong format line, a field v1 does not know, a key or signature that is
	// not the right number of lower-case hex characters, a name that cannot
	// be printed, or a declaration carrying a number only a store may assign.
	ErrBundle = errors.New("sapedb/bundle: that is not a bundle this version can read")

	// ErrUnsigned is a bundle with no signature on it at all. Separate from
	// ErrBadSignature on purpose, and SAPE-10 asks for the separation by
	// name: an unsigned bundle is one whose author never signed it, and the
	// answer is to go and get a signed copy. A bad signature is a file that
	// was signed and then changed.
	ErrUnsigned = errors.New("sapedb/bundle: the bundle carries no signature")

	// ErrBadSignature is a bundle whose declarations are not the ones the key
	// it names signed. Either the bytes were altered after signing or the
	// signature belongs to a different bundle; nothing here can tell those
	// apart, and it does not claim to.
	ErrBadSignature = errors.New("sapedb/bundle: the signature does not match these declarations")

	// ErrUntrusted is an intact bundle signed by a key no operator of this
	// server put on the list. The file is fine; the question is who made it.
	// The operator's answer is to add the key or to find out why they were
	// sent a bundle from a stranger — which is a different action from every
	// other refusal here, which is why it has a name of its own.
	ErrUntrusted = errors.New("sapedb/bundle: the bundle is signed by a key no operator of this server trusts")

	// ErrNoTrust is a server with an empty list of trusted signers. It is a
	// fact about the server rather than about the bundle, so it is answered
	// before the bundle is read at all: no key is trusted here, therefore no
	// bundle can be, and no amount of re-downloading will change that.
	//
	// Fail-closed with a name is the whole of this decision. An empty list
	// that meant "trust everyone" would be a configuration file that grants
	// the most by saying the least, and an empty list that produced
	// ErrUntrusted would send an operator looking for the key they forgot to
	// add rather than for the list they forgot to write.
	ErrNoTrust = errors.New("sapedb/bundle: no signer is trusted on this server, so no bundle can be")

	// ErrKey is a key that is not one: the wrong length, or not lower-case
	// hex. It names an operator's own configuration or an author's own key
	// file, never the contents of a bundle — a bad key inside a bundle is
	// ErrBundle, because there the key is part of the file being judged.
	ErrKey = errors.New("sapedb/bundle: that is not an ed25519 key this version can read")
)

// Bundle is a signed set of declarations.
type Bundle struct {
	// Format must be Label. It is not in the signed message, because Label is
	// already the message's first line; this field is here so that a file can
	// be told apart from some other JSON before any crypto is attempted.
	Format string `json:"format"`

	// Name and Version are what this bundle calls itself. Neither is
	// interpreted: Version is not compared against anything and is not
	// ordered, so it is a label for a person reading a log, not a mechanism.
	// Both are signed, so neither can be changed in transit.
	Name    string `json:"name"`
	Version string `json:"version"`

	// Signer is what the author calls themselves. It is signed and therefore
	// tamper-evident, and it is self-asserted and therefore proves nothing:
	// whoever holds the key writes whatever they like here. Compare it
	// against the label Verify returns, which is what this operator wrote
	// beside that key.
	Signer string `json:"signer"`

	// Collections and Operations are the declarations, in the same shapes
	// `sapedb apply` already reads. Order is part of the signature: the same
	// declarations listed in a different order are a different bundle. That
	// is the opposite of what internal/signing does with a grant's scopes,
	// which are sorted before signing, and it is deliberate — a scope list is
	// a set, while these are applied in the order they are written and two
	// orders can install differently.
	Collections []store.Spec      `json:"collections,omitempty"`
	Operations  []store.Operation `json:"operations,omitempty"`

	// Signature is absent until Seal puts one there.
	Signature *Signature `json:"signature,omitempty"`
}

// Signature is who signed a bundle and with what.
type Signature struct {
	Algorithm string `json:"algorithm"`

	// PublicKey is the ed25519 public key, 64 lower-case hex characters. It
	// travels inside the bundle because a verifier has to know which key to
	// check against before it knows whether it trusts that key, and it is
	// part of the signed message so that swapping it is a broken signature
	// rather than a bundle that suddenly claims a different author.
	PublicKey string `json:"public_key"`

	// Signature is the ed25519 signature over Message, 128 lower-case hex
	// characters.
	Signature string `json:"signature"`
}

// TrustedSigner is one line of an operator's configuration: a key, and what
// this operator calls whoever holds it.
type TrustedSigner struct {
	// Label is local. It travels with nothing, it is never compared against
	// the Signer inside a bundle, and it exists so that a refusal or an audit
	// line can name a person instead of 64 hex characters.
	Label string `json:"label"`

	// PublicKey is 64 lower-case hex characters.
	PublicKey string `json:"public_key"`
}

// Trust is the set of signers an operator decided this server will look at.
type Trust struct {
	labels map[string]string
}

// NewTrust reads an operator's list. An empty list is not an error — it is a
// server that installs nothing, which is the correct state for a server whose
// operator has not decided yet, and Verify says so in those words.
func NewTrust(signers []TrustedSigner) (*Trust, error) {
	trust := &Trust{labels: make(map[string]string, len(signers))}
	for _, signer := range signers {
		if _, err := publicKey(signer.PublicKey); err != nil {
			return nil, fmt.Errorf("trusted signer %q: %w", signer.Label, err)
		}
		if previous, already := trust.labels[signer.PublicKey]; already {
			return nil, fmt.Errorf("%w: the key for %q is already on the list as %q",
				ErrKey, signer.Label, previous)
		}
		trust.labels[signer.PublicKey] = signer.Label
	}
	return trust, nil
}

// Signers is how many keys this server trusts.
func (t *Trust) Signers() int { return len(t.labels) }

// lowerHex decodes exactly want bytes written as lower-case hex, and refuses
// anything else. Upper case is refused rather than accepted, so that no byte
// of a bundle has a second spelling; see the package comment.
func lowerHex(value string, want int) ([]byte, error) {
	if len(value) != want*2 {
		return nil, fmt.Errorf("expected %d hex characters, got %d", want*2, len(value))
	}
	if value != strings.ToLower(value) {
		return nil, errors.New("hex must be lower case")
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func publicKey(value string) (ed25519.PublicKey, error) {
	raw, err := lowerHex(value, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKey, err)
	}
	return ed25519.PublicKey(raw), nil
}

// PublicKeyText is a public key written the way SAPEDB_TRUST wants it: 64
// lower-case hex characters, and nothing else.
//
// It exists so that the one command that produces a key and the one parser
// that reads a trust list cannot drift apart into two spellings. There is no
// second form — no base64, no PEM, no prefix — for the reason lowerHex refuses
// upper case: a key with two spellings is a key an operator can put on a trust
// list in a form that never matches the one inside a bundle.
func PublicKeyText(key ed25519.PublicKey) (string, error) {
	if len(key) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: a public key is %d bytes, not %d", ErrKey, ed25519.PublicKeySize, len(key))
	}
	return hex.EncodeToString(key), nil
}

// PrivateKeyText is a private key written in the same alphabet as the public
// one: 128 lower-case hex characters, being the 64 bytes ed25519 calls a
// private key (the 32-byte seed with the public half appended).
//
// Hex rather than anything richer, and no armour, header or comment around it,
// because the file this lands in is read back by ParsePrivateKey and by
// nothing else. A format with a header is a format with a version, a parser
// and a migration; this product's whole key story is "64 bytes, and whoever
// holds them is the author", and a file that says more than that would be
// claiming more than that.
func PrivateKeyText(key ed25519.PrivateKey) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: a private key is %d bytes, not %d", ErrKey, ed25519.PrivateKeySize, len(key))
	}
	return hex.EncodeToString(key), nil
}

// ParsePrivateKey reads back what PrivateKeyText wrote.
//
// Surrounding whitespace is trimmed and nothing else is: a key that arrives
// from a file written by an editor, from `echo`, or down a pipe carries a
// trailing newline that nobody typed, and refusing it would be refusing the
// key for something that is not part of it. Whitespace INSIDE the key is not
// trimmed, so a key with a line break through the middle is refused rather
// than silently re-joined into a different key.
//
// The refusal deliberately quotes nothing back. Every other parser here puts
// the offending value in its error, which is right for a public key, a label
// or a bundle field and wrong for exactly this one: a private key that failed
// to parse is still a private key, and the usual courtesy of showing what was
// read would put a live secret in a terminal and a log. So the error says how
// long the input was and what shape was wanted, which is everything an author
// needs to fix it, and never a character of it. That is also why lowerHex's
// own error is summarised rather than wrapped — hex.DecodeString names the
// byte it choked on, and that byte is part of the key.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	trimmed := strings.TrimSpace(text)
	raw, err := lowerHex(trimmed, ed25519.PrivateKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: a private key is %d lower-case hex characters, and this is %d characters that do not read as that",
			ErrKey, ed25519.PrivateKeySize*2, len(trimmed))
	}
	return ed25519.PrivateKey(raw), nil
}

// printable reports whether a field can be put in an error or a log without
// mangling it. It is the only rule on a name, a version or a signer: no
// character is banned for the encoding's sake, because the encoding counts
// bytes and so has no delimiter for a character to be mistaken for.
func printable(value string) bool {
	if value == "" || len(value) > TextBytes {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// counted writes one field of the signed message: its length in bytes, a ":",
// the field, and a newline. Identical in shape to internal/signing's counted,
// and identical in purpose: the length is what makes the message unambiguous,
// and the ":" and "\n" are there for a person reading a fixture.
func counted(value string) string {
	return strconv.Itoa(len(value)) + ":" + value + "\n"
}

// section writes a list: how many members, then each member.
//
// The member count is the part a grant did not need and a bundle does, and it
// is the second half of this ticket's encoding decision. Counting each member
// stops one member running into the next. It does not stop one LIST running
// into the next: with per-member counts alone, a bundle of two collections and
// no operations writes exactly the bytes of a bundle of one collection and one
// operation whose member happens to be the same string. Today the two lists
// hold different Go types, whose JSON can never coincide — but that is a proof
// about two structs, which is precisely the kind of argument internal/signing
// had to redo from scratch every time a field was added, and it stops holding
// the day a third list of declarations arrives. The count in front removes the
// question instead of answering it.
func section(values []string) string {
	message := counted(strconv.Itoa(len(values)))
	for _, value := range values {
		message += counted(value)
	}
	return message
}

// declarable refuses a declaration carrying a number that only a store may
// hand out.
//
// A Spec's ID and its NextIndexID/NextRollupID counters, an Index's ID, a
// Rollup's ID and an Operation's Version are all assigned by the receiving
// store, and they are written into the keys of that particular database. An
// author on another machine cannot know them and has no business claiming
// them. Leaving them unchecked would mean signing a number that the installer
// then ignores — a field inside the signature that means nothing, which is
// exactly how a signed message grows a place to hide something.
func declarable(b Bundle) error {
	for _, spec := range b.Collections {
		switch {
		case spec.ID != 0:
			return fmt.Errorf("%w: collection %q carries id %d, which only the store it is installed into may assign",
				ErrBundle, spec.Name, spec.ID)
		case spec.NextIndexID != 0 || spec.NextRollupID != 0:
			return fmt.Errorf("%w: collection %q carries the store's own counters", ErrBundle, spec.Name)
		}
		for _, index := range spec.Indexes {
			if index.ID != 0 {
				return fmt.Errorf("%w: index %q of collection %q carries id %d",
					ErrBundle, index.Name, spec.Name, index.ID)
			}
		}
		for _, rollup := range spec.Rollups {
			if rollup.ID != 0 {
				return fmt.Errorf("%w: rollup %q of collection %q carries id %d",
					ErrBundle, rollup.Name, spec.Name, rollup.ID)
			}
		}
	}
	for _, operation := range b.Operations {
		if operation.Version != 0 {
			return fmt.Errorf("%w: operation %q carries version %d, which only the store it is installed into may assign",
				ErrBundle, operation.Name, operation.Version)
		}
	}
	return nil
}

// Message is the exact bytes a bundle signs:
//
//	"sapedb/bundle:v1" "\n"
//	len(name)       ":" name       "\n"
//	len(version)    ":" version    "\n"
//	len(signer)     ":" signer     "\n"
//	len(public_key) ":" public_key "\n"
//	len(itoa(C))    ":" itoa(C)    "\n"      C = number of collections
//	  len(json_i)   ":" json_i     "\n"      for each collection, in order
//	len(itoa(O))    ":" itoa(O)    "\n"      O = number of operations
//	  len(json_j)   ":" json_j     "\n"      for each operation, in order
//
// where every length is the field's length in BYTES, written in decimal with
// no leading zeros and no sign; public_key is the 64 lower-case hex characters
// of the signing key; and json_i is the collection as encoding/json marshals
// a store.Spec — which is canonical for these types, because Go writes struct
// fields in declaration order and map keys sorted.
//
// Length-prefixed rather than delimiter-joined, because a bundle is where that
// mistake would be worse than it already was. internal/signing changed a
// grant's message to this form tonight after measuring 16 collisions over
// 7,744 tuples in the delimiter-joined form; a bundle has more variable-length
// fields than a grant and two unbounded lists on top, and none of the
// characters an author might reach for — ":" in a name, a newline inside a
// JSON string, a version written "1:2" — can be banned without this encoding
// leaning on a character table again. With a count in front of every field and
// in front of every list there is nothing left for any character to be
// mistaken for, and TestNoTwoBundlesShareOneMessage measures that over 8,192
// tuples with the delimiter-joined form beside it as the control.
//
// What is NOT in the message: the Format field, because Label is already the
// first line and a second copy would only be a second thing to keep in step;
// the Signature block, for the obvious reason; and the whitespace of the file
// the bundle was read from, because the message is built from the parsed
// declarations and not from the bytes they arrived in.
func Message(b Bundle, key ed25519.PublicKey) (string, error) {
	if len(key) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: a public key is %d bytes, not %d", ErrKey, ed25519.PublicKeySize, len(key))
	}
	// A slice rather than a map, so that a bundle with two bad fields always
	// names the same one: a refusal whose wording depends on Go's map
	// iteration order is a refusal nobody can write a test against.
	for _, named := range []struct{ field, value string }{
		{"name", b.Name}, {"version", b.Version}, {"signer", b.Signer},
	} {
		if !printable(named.value) {
			return "", fmt.Errorf("%w: %s must be 1-%d bytes and hold no control characters, not %q",
				ErrBundle, named.field, TextBytes, named.value)
		}
	}
	if err := declarable(b); err != nil {
		return "", err
	}

	collections := make([]string, 0, len(b.Collections))
	for _, spec := range b.Collections {
		encoded, err := json.Marshal(spec)
		if err != nil {
			return "", fmt.Errorf("%w: collection %q: %v", ErrBundle, spec.Name, err)
		}
		collections = append(collections, string(encoded))
	}
	operations := make([]string, 0, len(b.Operations))
	for _, operation := range b.Operations {
		encoded, err := json.Marshal(operation)
		if err != nil {
			return "", fmt.Errorf("%w: operation %q: %v", ErrBundle, operation.Name, err)
		}
		operations = append(operations, string(encoded))
	}

	return Label + "\n" +
		counted(b.Name) +
		counted(b.Version) +
		counted(b.Signer) +
		counted(hex.EncodeToString(key)) +
		section(collections) +
		section(operations), nil
}

// Seal signs a bundle, for whoever holds the author's private key. It fills in
// Format and Signature and leaves everything else alone.
func Seal(b *Bundle, key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: a private key is %d bytes, not %d", ErrKey, ed25519.PrivateKeySize, len(key))
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("%w: the key does not carry a public half", ErrKey)
	}

	b.Format = Label
	b.Signature = nil
	message, err := Message(*b, public)
	if err != nil {
		return err
	}
	b.Signature = &Signature{
		Algorithm: Algorithm,
		PublicKey: hex.EncodeToString(public),
		Signature: hex.EncodeToString(ed25519.Sign(key, []byte(message))),
	}
	return nil
}

// Parse reads a bundle from the bytes of a file.
//
// Unknown fields are refused rather than dropped — the same rule `sapedb
// apply` already applies to a schema file — so nothing can be carried past
// verification by hiding it in a key this version does not read. Trailing
// content after the object is refused for the same reason: a file holding two
// JSON objects must not verify as its first one.
func Parse(raw []byte) (Bundle, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var b Bundle
	if err := decoder.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("%w: %v", ErrBundle, err)
	}
	if decoder.More() {
		return Bundle{}, fmt.Errorf("%w: there is more than one document in this file", ErrBundle)
	}
	return b, nil
}

// Check reports whether a bundle is intact and signed by the key it names, and
// returns that key. It makes no trust decision at all: a bundle signed by a
// stranger passes Check. Use Trust.Verify to decide whether the key is one
// this server will take a bundle from.
//
// The order of the refusals is the order of the questions. Is this a bundle at
// all; is there a signature on it; is the signature over these declarations.
// Nothing about the declarations themselves is judged here — a bundle
// declaring a collection this store would refuse is still a bundle its author
// signed, and confusing "not from you" with "will not install" is how a
// refusal ends up meaning two things.
func Check(b Bundle) (ed25519.PublicKey, error) {
	if b.Format != Label {
		return nil, fmt.Errorf("%w: format is %q, not %q", ErrBundle, b.Format, Label)
	}
	if b.Signature == nil || b.Signature.Signature == "" {
		return nil, ErrUnsigned
	}
	if b.Signature.Algorithm != Algorithm {
		return nil, fmt.Errorf("%w: signature algorithm is %q, and v1 only knows %q",
			ErrBundle, b.Signature.Algorithm, Algorithm)
	}

	raw, err := lowerHex(b.Signature.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: public_key: %v", ErrBundle, err)
	}
	key := ed25519.PublicKey(raw)

	signature, err := lowerHex(b.Signature.Signature, ed25519.SignatureSize)
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrBundle, err)
	}

	message, err := Message(b, key)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key, []byte(message), signature) {
		return nil, fmt.Errorf("%w: it was signed by %s", ErrBadSignature, b.Signature.PublicKey)
	}
	return key, nil
}

// Verify is the whole decision: this bundle is intact, and the key that signed
// it is one an operator of this server put on the list. It returns the label
// THIS operator wrote beside that key — deliberately not the Signer field
// inside the bundle, which the author wrote about themselves.
//
// An empty list is answered before anything else, because it is a fact about
// the server and true before the file arrives: no key is trusted here, so no
// bundle can be, and an operator reading that should go and configure a
// signer rather than go looking for a better copy of the bundle.
//
// After that the signature is checked before the trust list is consulted. Two
// reasons: a bundle whose bytes were altered is altered whoever signed it, so
// that is the more accurate sentence; and a verifier that answered "I do not
// know that key" before checking anything would let anybody who can hand this
// server a file probe which keys it trusts, one guess at a time, without
// holding any key at all.
func (t *Trust) Verify(b Bundle) (string, error) {
	if len(t.labels) == 0 {
		return "", ErrNoTrust
	}
	key, err := Check(b)
	if err != nil {
		return "", err
	}
	label, trusted := t.labels[hex.EncodeToString(key)]
	if !trusted {
		return "", fmt.Errorf("%w: it was signed by %s, and this server's list holds %d key(s), none of them that one",
			ErrUntrusted, hex.EncodeToString(key), len(t.labels))
	}
	return label, nil
}
