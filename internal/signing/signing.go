// Package signing carries the one contract this store shares with the app:
// the signature in a sapedb:// connection string.
//
// The app side is @ecosy/sapedb/signer. Both read fixtures/signing.json, so a
// change on either side turns both test suites red at once instead of arriving
// as a user who cannot connect.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DefaultLabel derives the signing key from the secret. Both sides must agree
// on the mode: this label, or Direct.
const DefaultLabel = "ecosy/sapedb:connection:v1"

// Direct signs with the secret itself, the plainer form — the app passes
// label: null for it.
//
// Its value is deliberately not the empty string. A blank configuration field
// must never select this: the two forms produce different signatures, so a
// server that fell back to it would refuse every string a client signed the
// ordinary way, and every test on one side would still pass because both sides
// of that side agree. That is exactly what happened here before this was a
// sentinel.
const Direct = "\x00sapedb:direct"

// PasswordPattern is what a password may be.
//
// Narrow on purpose. Unicode has more than one way to write the same password —
// macOS composes, Windows decomposes, copy-paste converts between them — so
// without this the same password would be two byte strings and two signatures,
// with nothing in a log to explain it. This charset has one encoding per
// password, so neither side normalises and neither side can normalise
// differently. It also excludes ":", which is what makes
// user_id:password:project_id unambiguous without length prefixing.
var PasswordPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,128}$`)

// Parts are the three fields of a connection string that a signature covers.
type Parts struct {
	// AccountID is the registered account — the tenant boundary. A database
	// name only has to be unique within one.
	AccountID string
	Password  string
	DBName    string
}

var (
	ErrEmptySecret  = errors.New("sapedb: secret must not be empty")
	ErrPassword     = errors.New("sapedb: password must be 16-128 characters of A-Z a-z 0-9 . _ ~ -")
	ErrFieldDelim   = errors.New(`sapedb: account_id and dbname must not be empty or contain ":"`)
	ErrBadSignature = errors.New("sapedb: signature does not verify")
)

// ValidPassword reports whether the protocol can carry this password.
func ValidPassword(password string) bool {
	return PasswordPattern.MatchString(password)
}

func field(value string) error {
	if value == "" || strings.Contains(value, ":") {
		return ErrFieldDelim
	}
	return nil
}

// Message is the exact bytes signed: account_id ":" password ":" dbname,
// UTF-8, unnormalised.
func Message(parts Parts) (string, error) {
	if err := field(parts.AccountID); err != nil {
		return "", fmt.Errorf("account_id: %w", err)
	}
	if err := field(parts.DBName); err != nil {
		return "", fmt.Errorf("dbname: %w", err)
	}
	if !ValidPassword(parts.Password) {
		return "", ErrPassword
	}
	return parts.AccountID + ":" + parts.Password + ":" + parts.DBName, nil
}

// key is what the signature is made with: derived under a label, or the secret
// itself when label is Direct.
func key(secret, label string) []byte {
	switch label {
	case Direct:
		return []byte(secret)
	case "":
		// Nothing said means the ordinary form, which is what every client
		// does by default. The dangerous answer is never the silent one.
		label = DefaultLabel
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// Sign returns the sig of a connection string: lower-case hex, 64 characters.
func Sign(parts Parts, secret, label string) (string, error) {
	if secret == "" {
		return "", ErrEmptySecret
	}
	message, err := Message(parts)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key(secret, label))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify reports whether signature is the one these parts have under this
// secret. Comparison is constant time; anything malformed is simply false, so
// a forged string is input rather than an error path of its own.
func Verify(signature string, parts Parts, secret, label string) bool {
	want, err := Sign(parts, secret, label)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}

// Operating a database is a different permission from using one, and nothing
// in a connection string says which you hold.
//
// The string signs account, password and database name — what to reach, not
// what you may do with it. Adding a field for it would mean every existing
// string is reissued and both sides changed, and it would still be a claim
// the client makes about itself.
//
// So an operator proves something instead: possession of the server's own
// secret, over a nonce the server just chose. That is not a new power being
// handed out. Whoever can read the secret can already read the database files
// and mint any connection string they like; the shell only makes it
// comfortable, and — because every access it runs goes into the change log —
// visible afterwards, which reading the files is not.
//
// The nonce is what stops the proof being a password. One is good for one
// connection, so it cannot be copied out of a log or a process listing and
// used again.
const OperatorLabel = "sapedb/operator:v1"

// ErrNonce is a challenge that is not one: empty, or not the length the server
// issues. A proof over a nonce the client chose proves nothing.
var ErrNonce = errors.New("sapedb: the challenge is not one the server issued")

// NonceBytes is how long a challenge is.
const NonceBytes = 32

// Operating answers a challenge with proof that the secret is held.
func Operating(secret string, nonce []byte) (string, error) {
	if secret == "" {
		return "", ErrEmptySecret
	}
	if len(nonce) != NonceBytes {
		return "", ErrNonce
	}
	mac := hmac.New(sha256.New, key(secret, OperatorLabel))
	mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Operates reports whether this is the answer to that challenge under this
// secret. Constant time, and anything malformed is false rather than an error
// path of its own.
//
// No test covers the constant time, and one that claimed to would be worse
// than none: a mutation replacing hmac.Equal with == passes everything here,
// because the difference is a timing signal and not an answer. Measuring it in
// a unit test would be measuring the machine the test runs on. It is written
// down here instead, which is the honest form of a property you can review but
// not assert.
func Operates(proof string, secret string, nonce []byte) bool {
	want, err := Operating(secret, nonce)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(proof)), []byte(want))
}

// Holding a scope is a different thing again from operating the server, and a
// connection string says nothing about it either.
//
// An operation may declare `scopes`, and running it means presenting them.
// Until this existed, nothing on the wire could: internal/server built its
// store.Caller with no Scopes at all and said so in a comment — "a caller that
// names its own permissions has none". The comment was right about the danger
// and wrong about the conclusion. An operation that declares a scope was an
// operation nobody could call, so `scopes` was a field that refused everything
// and permitted nothing, which is not a permission system; it is a way of
// disabling an operation by mentioning a word.
//
// What a client sends is therefore not a list of scopes. It is a list of
// scopes AND a signature over them, made with a key derived from the server's
// own secret — the same secret that signs connection strings, under a label of
// its own so the two can never be swapped. A caller cannot mint one, for
// exactly the reason a caller cannot mint a connection string: it does not
// have the secret. Whoever issues connection strings issues these, at the same
// moment, by the same means, offline.
//
// The grant names the account and the database as well as the scopes, and all
// three are signed together. So a grant is not a scope, it is a scope HERE: a
// grant minted for one database cannot be presented at another, and one minted
// for one account cannot be presented by another, even against the same server
// under the same secret.
//
// What it deliberately is not is a challenge-response like Operating below. A
// nonce would stop a grant being replayed, and it would also mean whoever
// issues grants has to be online at the moment of every connection — which is
// the opposite of how this product's credentials work, and would turn an
// offline control plane into a request path. A grant is a bearer credential of
// exactly the same weight as the connection string it travels beside: anybody
// holding that string can already reach the database, and the grant says what
// they may do once there. It is no stronger than the string and no weaker,
// which is the honest place for it to sit.
//
// What it does not have yet, said out loud rather than discovered: an expiry,
// and any way to withdraw one short of changing the secret. See the note on
// Held.
const GrantLabel = "sapedb/scopes:v1"

// ErrScope is a scope this protocol cannot carry: empty, or holding the comma
// the list is joined with.
//
// A scope MAY hold a ":", and the ones this product uses do — "articles:read",
// "catalog:read". That is safe even though ":" separates the first two fields
// of the signed message, because those two fields cannot contain one: whatever
// follows the second ":" is the scope list entire, so there is exactly one way
// to read a message back into an account, a database and a list. The comma is
// the one character that would be ambiguous, and it is the one that is banned.
var ErrScope = errors.New(`sapedb: a scope must not be empty or contain ","`)

// Held is what a grant says: these scopes, for this account, on this database.
//
// There is no expiry field and no serial number, which means a grant is good
// until the secret changes. That is a real limitation and it is written here
// rather than left to be found: withdrawing one scope from one account today
// means reissuing every connection string on the server. Adding an expiry is
// a change to the signed message, so it is a change both this side and the
// TypeScript signer make together — which is why it is not being done halfway
// now.
type Held struct {
	// AccountID and DBName are the same two fields a connection string signs,
	// and they are here for the same reason: a credential that does not say
	// where it is good is good everywhere.
	AccountID string
	DBName    string
	// Scopes is the set granted. Order and repetition do not matter: Granting
	// sorts and de-duplicates before signing, so the same set is always the
	// same signature however a caller happens to have written it down.
	Scopes []string
}

// scopeList is the canonical form of a set of scopes: sorted, de-duplicated,
// comma-joined.
//
// Canonical rather than as-written because the alternative is a signature that
// depends on the order of a JSON array, which would make ["read","write"] and
// ["write","read"] two different grants over one set of permissions — two
// things to issue, two to withdraw, and a support question nobody can answer
// from a log.
func scopeList(scopes []string) (string, error) {
	seen := make(map[string]bool, len(scopes))
	unique := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" || strings.Contains(scope, ",") {
			return "", fmt.Errorf("%w: %q", ErrScope, scope)
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		unique = append(unique, scope)
	}
	sort.Strings(unique)
	return strings.Join(unique, ","), nil
}

// GrantMessage is the exact bytes a grant signs:
//
//	account_id ":" dbname ":" scope["," scope...]
//
// Unambiguous without length prefixing: account_id and dbname cannot contain a
// ":", so the first two are fixed by the first two colons and everything after
// the second one is the scope list entire — which is why a scope is allowed to
// hold a ":" of its own, as every scope this product uses does. The comma is
// the only character a scope may not hold.
//
// A grant of no scopes is legal and means exactly what it says — the third
// field is empty. It is worth minting: it is how a caller is told, in one
// place, that it holds nothing, rather than by every scoped operation refusing
// it one at a time.
func GrantMessage(held Held) (string, error) {
	if err := field(held.AccountID); err != nil {
		return "", fmt.Errorf("account_id: %w", err)
	}
	if err := field(held.DBName); err != nil {
		return "", fmt.Errorf("dbname: %w", err)
	}
	list, err := scopeList(held.Scopes)
	if err != nil {
		return "", err
	}
	return held.AccountID + ":" + held.DBName + ":" + list, nil
}

// Granting mints a grant: lower-case hex, 64 characters. For whoever holds the
// secret, which is whoever issues connection strings.
//
// The key is derived under GrantLabel and nothing else — not the server's
// configured connection-string label. Two labels, two keys: a connection
// string's signature can never be presented as a grant, and a grant can never
// be presented as a connection string's signature, whatever either one happens
// to be signed over.
func Granting(held Held, secret string) (string, error) {
	if secret == "" {
		return "", ErrEmptySecret
	}
	message, err := GrantMessage(held)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key(secret, GrantLabel))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Grants reports whether this signature is the one these scopes have, for this
// account and this database, under this secret.
//
// Constant time, and anything malformed is false rather than an error path of
// its own — a forged grant is input, not an exception.
func Grants(signature string, held Held, secret string) bool {
	want, err := Granting(held, secret)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}
