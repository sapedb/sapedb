// Package dbkey is the one place that derives the key a database file is
// encrypted with. It exists because internal/cli and internal/server used to
// each carry their own copy of the label and the hkdf.Key call — two source
// locations for one fact that must never disagree, with nothing in the tree
// that noticed if they did. A patrol (internal/naming) could see the label's
// text drift; it could not see someone edit the byte order of the info string
// or swap the label and the info argument, because neither of those changes
// touches the old product name the patrol watches for. Collapsing both
// callers onto this package removes the drift entirely, rather than
// detecting it after the fact.
package dbkey

import (
	"crypto/hkdf"
	"crypto/sha256"

	"github.com/sapedb/sapedb/internal/dbname"
	"github.com/sapedb/sapedb/internal/pager"
)

// Label must not change from here on. It used to carry the product's old
// name, kept on the argument that changing it is a silent, unannounced key
// rotation: the key this package derives stops matching the key an existing
// file was written under, which reads as "wrong secret" for a secret that is
// right. That argument was true and is now spent — the repository carries no
// tags at all, so there has never been a release, and no encrypted database
// written by a build anybody else ran exists to be locked out. This commit
// is the rename; after 1.0.0 moving this value is a migration's job, not a
// refactor's, and the argument above applies again in full.
//
// The :v1 suffix stays :v1 on purpose. This is not a new version of the
// derivation scheme — the algorithm, the info string and the key length are
// untouched — it is the same scheme with the product's current name in it.
const Label = "sapedb/server:database:v1"

// Key derives the key a database file is encrypted with, from the secret
// that is the single root of trust and the account/name that names the
// database. Both internal/cli (writing a file) and internal/server (opening
// it) must call exactly this, or the two processes derive different keys for
// the same file.
//
// It refuses an account or a name dbname.Check would refuse, before
// deriving anything — the same rule internal/cli and internal/server check
// on their own two doors. This is the sole reason task 0053 gives for
// putting the check here too, not just at those two doors: this function's
// info string is account+"/"+name, so ("a/b", "c") and ("a", "b/c") build
// the identical string "a/b/c" and would derive the identical key
// (dbkey_test.go's TestKeyDoesNotDistinguishWhereTheSlashFallsInAccountOrDB
// still measures that arithmetic fact). -encrypt is the only caller that
// ever reaches this function, so a caller that skips the check at its own
// door — a third process embedding this package directly, say — would
// otherwise derive a colliding key with no other check ever having run.
func Key(secret, account, name string) ([]byte, error) {
	if err := dbname.CheckPair(account, name); err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, []byte(secret), []byte(account+"/"+name), Label, pager.KeyBytes)
}
