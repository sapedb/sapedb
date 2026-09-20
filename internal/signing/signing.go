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
	"strconv"
	"strings"
	"time"
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
// of it is signed together. So a grant is not a scope, it is a scope HERE and
// UNTIL: a grant minted for one database cannot be presented at another, one
// minted for one account cannot be presented by another, and one whose expiry
// has passed cannot be presented at all.
//
// What it deliberately is not is a challenge-response like Operating above. A
// nonce would stop a grant being replayed, and it would also mean whoever
// issues grants has to be online at the moment of every connection — which is
// the opposite of how this product's credentials work, and would turn an
// offline control plane into a request path. An expiry does not: it is a
// number the issuer writes into the message it is already signing, offline,
// and the verifier reads it off the credential rather than asking anybody.
// A grant is a bearer credential of exactly the same weight as the connection
// string it travels beside — no stronger and no weaker — for as long as it
// says it is good for.
//
// GrantLabel is both the key derivation label and the first line of the signed
// message, deliberately one string rather than two: a message that names the
// key it must be signed with cannot be replayed into a verifier expecting a
// different one, and there is only one version spelling to get wrong when this
// changes again. It was "sapedb/scopes:v1" over a three-field message with no
// expiry and no serial; every grant minted under that is refused here, which
// costs nothing because nothing has been tagged.
const GrantLabel = "sapedb/scopes:v2"

var (
	// ErrScope is a scope this protocol cannot carry: empty, or holding the
	// comma the list is joined with.
	//
	// A scope MAY hold a ":", and the ones this product uses do —
	// "articles:read", "catalog:read". The comma is the one character that
	// would make two different sets the same list: one scope spelled "a,b"
	// and two scopes "a" and "b" would join to the same string. Nothing else
	// about a scope is constrained, because the field the list lands in is
	// length-prefixed and so cannot run into its neighbours whatever it
	// holds.
	ErrScope = errors.New(`sapedb: a scope must not be empty or contain ","`)

	// ErrExpiry is a grant with no usable expiry. There is no such thing as a
	// grant without one: the zero time, a time before the epoch, and anything
	// else that does not land on a positive count of seconds are all refused
	// at minting rather than signed and discovered later.
	ErrExpiry = errors.New("sapedb: a grant must carry an expiry, as a time after 1970")

	// ErrSerial is a grant serial this protocol cannot carry: empty, longer
	// than SerialBytes, or holding a control character.
	//
	// Note what is NOT banned: ":" and "," are both allowed, and that is the
	// point rather than an oversight. The v2 message is length-prefixed, so
	// no character in any field can be mistaken for a delimiter, and a rule
	// banning one would be this encoding leaning on a character table again —
	// which is the exact thing that had to be re-proved every time a field
	// was added to the v1 message. Control characters are refused only so a
	// serial can be printed into an error or a log without mangling it.
	ErrSerial = errors.New("sapedb: a grant serial must be 1-64 bytes and hold no control characters")

	// ErrExpired is a grant whose expiry has passed, checked against the
	// clock the verifier was handed. It is a different answer from
	// ErrBadSignature and must stay one: a caller whose grant expired should
	// go and get another, and a caller whose grant never authorised this
	// should stop.
	ErrExpired = errors.New("sapedb: that grant has expired")
)

// SerialBytes is the longest a grant serial may be.
const SerialBytes = 64

// Held is what a grant says: these scopes, for this account, on this database,
// until this moment, under this serial.
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
	// Expires is when this grant stops being one, and it is mandatory. It is
	// signed to whole seconds — GrantMessage writes Expires.Unix() and
	// nothing finer — so two times in the same second are one grant.
	//
	// Mandatory rather than "checked when present" on purpose. A verifier
	// that accepts a message with no expiry is a verifier an attacker can
	// choose: strip the field, and the credential is good forever again.
	// There is one message shape here and one branch that reads it.
	Expires time.Time
	// Serial names this particular grant, so that a revocation list has
	// something to name. Nothing in this repository keeps such a list yet and
	// this field is not consulted by verification — it is in the signed
	// message now because adding a field to a signed message after a tag is a
	// break, and adding one before the tag is an edit.
	//
	// Uniqueness is the issuer's business: two grants may carry the same
	// serial and nothing here will notice, which matters the day somebody
	// tries to revoke one of them and withdraws both.
	Serial string
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

// validSerial reports whether a serial is one this protocol can carry.
func validSerial(serial string) bool {
	if serial == "" || len(serial) > SerialBytes {
		return false
	}
	for _, r := range serial {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// counted writes one field of the signed message: its length in bytes, a ":",
// the field, and a newline.
//
// The length is what makes the message unambiguous, and it is the only thing
// that does. The ":" and the "\n" are there to be read by a person looking at
// a fixture; a parser driven by the count never consults either, so a field
// holding a ":" or a "\n" of its own changes nothing.
func counted(value string) string {
	return strconv.Itoa(len(value)) + ":" + value + "\n"
}

// GrantMessage is the exact bytes a grant signs:
//
//	"sapedb/scopes:v2" "\n"
//	len(account_id) ":" account_id "\n"
//	len(dbname)     ":" dbname     "\n"
//	len(scope_list) ":" scope_list "\n"
//	len(exp)        ":" exp        "\n"
//	len(serial)     ":" serial     "\n"
//
// where every length is the field's length in BYTES, written in decimal with
// no leading zeros and no sign; scope_list is the scopes sorted,
// de-duplicated and joined with ","; and exp is Expires.Unix() in decimal.
//
// Length-prefixed rather than delimiter-joined, and that is the whole of this
// ticket's second decision. The v1 message was account_id ":" dbname ":"
// scope_list, and it was unambiguous — but only by an argument: account_id and
// dbname may not hold a ":", so the first two colons are fixed and the scope
// list is the whole of the tail. That argument is not a property of the
// format, it is a proof about three specific fields, and it has to be redone
// from scratch every time a field is added. Adding two at once is exactly
// where such a proof goes wrong: "a:b:" + scopes + ":" + exp + ":" + serial
// is genuinely ambiguous the moment a serial may hold a ":" — the tuples
// (scopes ["x"], exp 100, serial "200:z") and (scopes ["x:100"], exp 200,
// serial "z") both write "a:b:x:100:200:z", which is two different grants with
// one signature and therefore a forgery primitive rather than a formatting
// nit. TestTwoGrantsCannotShareOneMessage holds exactly that pair.
//
// With a count in front of every field there is nothing left to prove: the
// count says where the field ends, so no character in any field can be read as
// a delimiter, and a sixth field could be added tomorrow without re-opening
// the question. The version line pins which set of fields is being read, so a
// message of five fields can never be mistaken for a message of six.
//
// A grant of no scopes is legal and means exactly what it says — the scope
// list is the empty string, written "0:". It is worth minting: it is how a
// caller is told, in one place, that it holds nothing, rather than by every
// scoped operation refusing it one at a time.
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
	if held.Expires.IsZero() || held.Expires.Unix() <= 0 {
		return "", fmt.Errorf("%w: %v", ErrExpiry, held.Expires)
	}
	if !validSerial(held.Serial) {
		return "", fmt.Errorf("%w: %q", ErrSerial, held.Serial)
	}
	return GrantLabel + "\n" +
		counted(held.AccountID) +
		counted(held.DBName) +
		counted(list) +
		counted(strconv.FormatInt(held.Expires.Unix(), 10)) +
		counted(held.Serial), nil
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
// account and this database, under this secret — and whether the grant is
// still good at the moment named by now.
//
// It returns an error rather than a bool, and takes a clock rather than
// reading one, because both of those are the shape of the decision. There are
// two ways to refuse and a caller has to tell them apart: a grant that expired
// is a reason to go and get another, and a grant that was never for this
// account is a reason to stop. A bool cannot say which, and a verifier that
// reads time.Now() for itself is one a test cannot put a clock in front of.
// Handing the clock in also means no caller can verify a grant without having
// decided what time it is, which is the whole point of the field.
//
// The signature is checked first and always. Anything malformed — a grant
// whose fields cannot even make a message — is ErrBadSignature and not an
// error path of its own, so a forged grant stays input rather than becoming an
// exception; and a forger learns nothing about a made-up grant's expiry,
// because expiry is not consulted until the signature has already agreed.
//
// Comparison is constant time. As with Operates, no test here measures that:
// a mutant replacing hmac.Equal with == passes everything in this package,
// because the difference is a timing signal and not an answer.
//
// There is no clock skew allowance, and that is a decision rather than an
// omission. Any leeway of L seconds is a grant that keeps working for L
// seconds after the credential itself says it stopped — the exact property
// this function exists to enforce, weakened by an amount that is invisible in
// the grant, invisible in the log, and identical for every grant on the
// server. The margin belongs in the number the issuer signs, where it is
// visible and per-grant, and the issuer is the one party that can size it: it
// picks the lifetime. Skew is real but it is not fatal here the way it is for
// a thirty-second token — a grant is issued offline for hours or days, so an
// issuing clock a minute out shifts the effective expiry by a minute and
// nothing notices. For the issuing clock to mint something already dead it
// would have to be wrong by more than the entire lifetime of the grant, which
// is a broken clock and not skew, and a leeway sized for skew would not save
// it anyway.
//
// What an operator with a badly wrong clock sees is therefore the honest
// thing: a server whose clock is hours fast refuses every grant as expired,
// including ones minted seconds earlier, and a server whose clock is hours
// slow honours grants well past their expiry without saying so. Neither is
// silent on the first count — the refusal names both the expiry and this
// verifier's own reading of the clock, so the disagreement is in the error
// message rather than left to be guessed at. The second is silent, and cannot
// be otherwise: a verifier that does not know it is slow has nothing to
// compare itself against.
func Grants(signature string, held Held, secret string, now time.Time) error {
	want, err := Granting(held, secret)
	if err != nil || !hmac.Equal([]byte(strings.ToLower(signature)), []byte(want)) {
		return ErrBadSignature
	}
	// Expires is the first instant at which the grant is no longer one, not
	// the last at which it is: a grant marked for 12:00:00 is refused at
	// 12:00:00. Same rule as every other exp field a client author has met,
	// which is worth more here than a second of extra life.
	if !now.Before(held.Expires.Truncate(time.Second)) {
		return fmt.Errorf("%w: it was good until %s, and this verifier's clock reads %s",
			ErrExpired,
			held.Expires.UTC().Truncate(time.Second).Format(time.RFC3339),
			now.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	return nil
}
