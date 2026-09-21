package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Namespaces: who is allowed to declare a name (SAPE-9).
//
// An operation name is `namespace:name`. A name with no separator is in the
// unnamed namespace, which is every name that was ever declared before this
// existed and every name anybody types at a shell. Nothing about those names
// changed: they resolve the way they always did, a second declaration of one
// still takes over every unversioned call, and no test that measured that had
// to be edited. The unnamed namespace is the one nobody owns, and saying so
// out loud is most of what this feature is.
//
// A NAMED namespace is claimed by the first declaration into it and is closed
// to everybody else afterwards. That is the whole of the protection: two
// vendors who each pick a namespace can no longer silently redeclare over one
// another, because the second one is refused by name and told who holds it.
// Two vendors who both stay in the unnamed namespace are exactly as exposed as
// they were, which is the honest answer — a namespace nobody claimed cannot
// defend anybody.
//
// WHY THIS COULD NOT WAIT FOR 1.1. Every separator is a legal operation name
// today: `internal/store/collision_test.go`'s
// TestAnOperationNameMayHoldAnySeparatorAName declares `acme:orders.recent`
// and five others and all six are accepted. COMPATIBILITY.md section 3 says a
// 1.x release may not tighten a rule so that a declaration valid in 1.0 is
// refused in 1.1. Refusing `a:b:c`, refusing `:foo`, and refusing a namespace
// with a space in it are all tightenings. After the 1.0.0 tag none of them can
// ever happen, so a namespace whose shape is checked at all had to land before
// it or never.
//
// WHAT THE CLAIMANT IS. The signing key, when the declaration arrived inside a
// signed bundle (`sapedb install`), and the caller's actor otherwise. The KIND
// is stored beside the identity rather than left to be guessed from the
// string, so a key and an account that happen to spell the same are two
// different claimants and not one. Without that, "who holds this namespace" is
// answered by a string comparison between two things that were never the same
// kind of fact — one checked by ed25519 over the declarations themselves, the
// other a label written by whoever was holding the file.

// NamespaceSeparator is the one byte that divides a name into its two halves.
//
// A single byte, and only one of them per name: `acme:orders.recent` is the
// namespace `acme` holding `orders.recent`, and `a:b:c` is refused rather than
// read as some nesting nobody agreed on. Refusing it is free now and
// impossible later.
const NamespaceSeparator = ':'

// What kind of thing holds a claim. Stored on the claim, not inferred.
//
// The same two kinds also say what a Declarer is (declarer.go), which records
// who declared one version of an operation. One vocabulary rather than two,
// because a key and an actor are the same two kinds of identity in both places
// and a second set of words for them is a second set to keep in agreement.
const (
	// ClaimedByKey is an ed25519 public key that signed a bundle, in hex.
	// This is the strong one: it was checked, over exactly the declarations
	// it installed.
	ClaimedByKey = "key"
	// ClaimedByActor is the actor of whoever declared it — the account a
	// connection proved, or the label `sapedb apply` was pointed at. It is a
	// name, not a proof, and it is recorded as a different kind for that
	// reason.
	ClaimedByActor = "actor"
)

// ErrNamespace is every refusal this file makes: a name whose namespace is not
// a shape a namespace may have, and a declaration into a namespace somebody
// else holds.
//
// One sentinel rather than two, and it reaches the wire as its own code
// (`namespace`, see internal/server's codeFor). Both halves are the same
// answer to whoever reads it — this name is not yours to declare — and both
// need a code of their own: a refusal that arrives as `failed` is ISS-21, and
// the cheapest time to not make that mistake again is while adding the
// refusal.
var ErrNamespace = errors.New("sapedb/store: that namespace is not this caller's to declare into")

// spaceClaims is where a namespace's claimant lives: one record per namespace,
// keyed by the namespace, the same shape as the collection catalogue
// (spaceCatalog) and the partition table (spaceParts). Its own record rather
// than a field on a declaration, because it has to survive a reopen and be
// readable before the declaration that would carry it is written.
const spaceClaims byte = 0x09 // 0x09 | namespace -> the Claim that holds it

// Claim is who holds a namespace.
type Claim struct {
	Namespace string `json:"namespace"`
	// Kind is ClaimedByKey or ClaimedByActor.
	Kind string `json:"kind"`
	// Identity is the key or the actor, depending on Kind. The two are never
	// compared across kinds.
	Identity string `json:"identity"`
}

// holder is the claim a caller would make, for comparing and for storing.
func holder(namespace string, caller Caller) Claim {
	if caller.Signer != "" {
		return Claim{Namespace: namespace, Kind: ClaimedByKey, Identity: caller.Signer}
	}
	return Claim{Namespace: namespace, Kind: ClaimedByActor, Identity: caller.Actor}
}

// same reports whether two claims are the same claimant. Kind first: a key and
// an actor spelled identically are not each other.
func (c Claim) same(other Claim) bool {
	return c.Kind == other.Kind && c.Identity == other.Identity
}

// SplitOperationName reads a name as namespace and name.
//
// No separator is the unnamed namespace, and an empty namespace is what that
// is called here rather than a special case spelled some other way. Everything
// else is refused: two separators, an empty half on either side, or a
// namespace holding anything but letters, digits, dot, dash and underscore.
//
// The charset is small on purpose. A namespace is compared by eye and typed by
// hand — it appears in every call a caller writes and in the refusal that names
// who holds it — and permitting arbitrary bytes now is a decision that cannot
// be walked back after the tag. Letters, digits, dot, dash and underscore is
// the same alphabet a partition name already gets (usablePartName, parts.go).
func SplitOperationName(name string) (string, string, error) {
	at := strings.IndexByte(name, NamespaceSeparator)
	if at < 0 {
		return "", name, nil
	}

	namespace, local := name[:at], name[at+1:]
	if strings.IndexByte(local, NamespaceSeparator) >= 0 {
		return "", "", fmt.Errorf("%w: %q holds more than one %q, and a name is one namespace and one name",
			ErrNamespace, name, string(NamespaceSeparator))
	}
	if namespace == "" {
		return "", "", fmt.Errorf("%w: %q starts with %q, so its namespace is empty; a name in no namespace is written without the separator",
			ErrNamespace, name, string(NamespaceSeparator))
	}
	if local == "" {
		return "", "", fmt.Errorf("%w: %q is a namespace with no name after it", ErrNamespace, name)
	}
	for i := 0; i < len(namespace); i++ {
		if !usableNamespaceByte(namespace[i]) {
			return "", "", fmt.Errorf("%w: the namespace of %q holds %q; a namespace may hold letters, digits, dot, dash and underscore",
				ErrNamespace, name, string(namespace[i]))
		}
	}
	return namespace, local, nil
}

// usableNamespaceByte is the charset, one byte at a time. Bytes rather than
// runes because every byte of a multi-byte rune is outside this set, so a rune
// that is not permitted is refused whichever of its bytes is looked at first.
func usableNamespaceByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '-' || b == '_':
		return true
	}
	return false
}

// NamespaceClaim reads back who holds a namespace, if anybody does.
//
// The unnamed namespace is never held: it is asked about here so that the
// answer is a sentence somebody can read rather than a lookup that happens to
// miss.
func (s *Store) NamespaceClaim(namespace string) (Claim, bool, error) {
	if namespace == "" {
		return Claim{}, false, nil
	}
	value, found, err := s.tree.Get(claimKey(namespace))
	if err != nil || !found {
		return Claim{}, false, err
	}
	claim := Claim{}
	if err := json.Unmarshal(value, &claim); err != nil {
		return Claim{}, false, fmt.Errorf("%w: the claim on namespace %q: %v", ErrDamaged, namespace, err)
	}
	return claim, true, nil
}

// claimFor settles whether this caller may declare this name, and takes the
// namespace for them if nobody has it yet.
//
// Called from DeclareOperation and nowhere else. Explore builds an Operation
// and validates it, but it never stores one, and a shell that quietly took
// ownership of a namespace by looking at a collection would be the worst
// possible place for a claim to come from.
func (s *Store) claimFor(caller Caller, name string) error {
	namespace, _, err := SplitOperationName(name)
	if err != nil {
		return err
	}
	// The unnamed namespace is never claimed. Claiming it would lock every
	// existing caller out of every flat name on their next declaration, which
	// is the one outcome this feature must not have.
	if namespace == "" {
		return nil
	}

	want := holder(namespace, caller)
	held, found, err := s.NamespaceClaim(namespace)
	if err != nil {
		return err
	}
	if found {
		if held.same(want) {
			return nil
		}
		return fmt.Errorf("%w: namespace %q is held by the %s %q, and %q was declared by the %s %q",
			ErrNamespace, namespace, held.Kind, held.Identity, name, want.Kind, want.Identity)
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return s.tree.Put(claimKey(namespace), encoded)
}

func claimKey(namespace string) []byte {
	return append([]byte{spaceClaims}, namespace...)
}
