package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/bundle"
	"github.com/sapedb/sapedb/internal/store"
)

// author is a deterministic key pair, so a failing test prints the same key
// every run and the assertions below can name it.
func author(t *testing.T, seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
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

// pack is the bundle these tests install. Its name holds neither the
// account ("acme") nor the operator's label for the key, so an assertion
// that the change log names the bundle cannot be satisfied by the log
// happening to name something else.
func pack() bundle.Bundle {
	return bundle.Bundle{
		Name:    "ledger-pack",
		Version: "2.1.0",
		Signer:  "Ledger Authors",
		Collections: []store.Spec{{
			Name: "postings",
			Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
			Indexes: []store.Index{{
				Name:   "by_account",
				Fields: []store.Field{{Path: "account", Type: store.TypeString, Missing: store.MissingSkip}},
			}},
		}},
		Operations: []store.Operation{
			{
				Name: "postings.get", Collection: "postings", Action: store.ActionGet,
				Input: []store.Parameter{{Name: "id", Type: store.TypeString, Required: true}},
				Key:   &store.Term{Arg: "id"},
			},
			{
				Name: "postings.by_account", Collection: "postings", Action: store.ActionScan,
				Index: "by_account", Limit: 25, Scopes: []string{"postings:read"},
				Input: []store.Parameter{{Name: "account", Type: store.TypeString, Required: true}},
				From:  &store.Endpoint{Terms: []store.Term{{Arg: "account"}}},
				To:    &store.Endpoint{Terms: []store.Term{{Arg: "account"}}},
			},
		},
	}
}

// sealedAt seals a bundle and writes it where a real one would be: outside
// SAPEDB_DIR. A bundle written inside the data directory would show up in
// walkTree and make "a refused command left the tree alone" unmeasurable.
func sealedAt(t *testing.T, b bundle.Bundle, private ed25519.PrivateKey) string {
	t.Helper()
	if err := bundle.Seal(&b, private); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	return writtenAt(t, b)
}

// writtenAt writes a bundle exactly as it is, sealed or not.
func writtenAt(t *testing.T, b bundle.Bundle) string {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("writing the bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ledger-pack.bundle")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// trusting is the environment of an operator who put this key on the list
// under this label.
func trusting(label string, key ed25519.PublicKey) map[string]string {
	return map[string]string{bundle.TrustEnv: label + "=" + hex.EncodeToString(key)}
}

// TestInstallingAVerifiedBundleDeclaresWhatItCarries is the whole point of
// the command: a signed file from somebody else becomes declarations in a
// real database, and `ls` — which knows nothing about bundles and reads only
// what the store actually holds — can see them afterwards.
//
// The read-back goes through a second command rather than through the
// store handle install used, on purpose. Asking the same open store would
// be satisfied by declarations sitting in an uncommitted transaction; `ls`
// runs after install's process has returned, takes the directory lock
// itself and opens the file from scratch, so what it prints is what was
// committed and nothing else.
func TestInstallingAVerifiedBundleDeclaresWhatItCarries(t *testing.T) {
	setup := start(t)
	public, private := author(t, 11)
	path := sealedAt(t, pack(), private)

	out, errs, status := setup.runWith(trusting("north-star", public), "", "install", path)
	if status != 0 {
		t.Fatalf("installing a verified bundle was refused: %s", errs)
	}

	for _, want := range []string{
		`installing bundle "ledger-pack" version "2.1.0"`,
		hex.EncodeToString(public),
		`trusted here as "north-star"`,
		"collection postings",
		"operation postings.get version 1",
		"operation postings.by_account version 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install did not report %q:\n%s", want, out)
		}
	}

	listed, errs, status := setup.run("ls")
	if status != 0 {
		t.Fatalf("ls after install: %s", errs)
	}
	for _, want := range []string{
		"collection postings (key id string, ulid)",
		"index by_account [account string missing:skip]",
		"operation postings.get v1 get postings",
		"operation postings.by_account v1 scan postings via by_account limit 25 scopes postings:read",
	} {
		if !strings.Contains(listed, want) {
			t.Errorf("the database does not hold %q after installing the bundle:\n%s", want, listed)
		}
	}

	// Installed twice is installed once. A bundle is a release artifact and
	// the obvious thing to do with one after a restore is install it again;
	// handing every caller a new operation version for that would be a
	// migration nobody asked for.
	again, errs, status := setup.runWith(trusting("north-star", public), "", "install", path)
	if status != 0 {
		t.Fatalf("installing the same bundle twice was refused: %s", errs)
	}
	if !strings.Contains(again, "operation postings.get unchanged at version 1") {
		t.Errorf("re-installing the same bundle did not leave the operations alone:\n%s", again)
	}
}

// TestARefusedBundleInstallsNothing is the all-or-nothing guarantee, and it
// is the reason this command is not a loop over apply().
//
// The bundle below carries two collections. The first is new and would
// install fine; the second collides with one this database already declared
// with a different primary key, which is store.ErrIncompatible — the exact
// refusal internal/bundle's own doc says settles a name fight between two
// authors, at install time and not at verification. There is no uninstall,
// so a run that declared the first and then stopped would leave a database
// holding half of somebody's release with no way back.
//
// Read back with `ls`, after install's process returned: the first
// collection must not be there.
func TestARefusedBundleInstallsNothing(t *testing.T) {
	setup := start(t)
	public, private := author(t, 12)

	// Already in the database, declared with a number for a primary key.
	existing := writeOutside(t, `{"collections":[{"name":"postings","key":{"path":"seq","type":"number"}}]}`)
	if _, errs, status := setup.run("apply", existing); status != 0 {
		t.Fatalf("setting the database up: %s", errs)
	}

	b := pack()
	// audit_trail comes FIRST and is perfectly installable on its own. It is
	// what proves the refusal undid something rather than merely stopping
	// before it started: a bundle whose only collection was the colliding
	// one would leave the catalogue unchanged whether or not anything rolled
	// back, and the test would measure nothing.
	b.Collections = append([]store.Spec{{
		Name: "audit_trail",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}}, b.Collections...)
	path := sealedAt(t, b, private)

	_, errs, status := setup.runWith(trusting("north-star", public), "", "install", path)
	if status == 0 {
		t.Fatal("a bundle whose second collection collides with this database was installed anyway")
	}
	// The sentinel has to arrive intact, not flattened into something
	// vaguer: internal/server's codeFor turns store.ErrIncompatible into the
	// "incompatible" code a client switches on, and a fmt.Errorf without %w
	// anywhere on this path would make it arrive as "failed".
	if !strings.Contains(errs, "sapedb/store: this does not match what was declared before") {
		t.Errorf("the refusal does not carry store.ErrIncompatible's own sentence: %q", errs)
	}
	for _, want := range []string{`bundle "ledger-pack"`, `collection "postings"`} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not say %q: %q", want, errs)
		}
	}

	listed, errs, status := setup.run("ls")
	if status != 0 {
		t.Fatalf("ls after a refused install: %s", errs)
	}
	if strings.Contains(listed, "audit_trail") {
		t.Errorf("a refused bundle left its first collection behind:\n%s", listed)
	}
	for _, name := range []string{"postings.get", "postings.by_account"} {
		if strings.Contains(listed, name) {
			t.Errorf("a refused bundle left %s behind:\n%s", name, listed)
		}
	}
	// And what was there before is still exactly as it was.
	if !strings.Contains(listed, "collection postings (key seq number)") {
		t.Errorf("the collection that was already there did not survive the refused install:\n%s", listed)
	}
}

// TestARefusedInstallLeavesNothingInTheOPENStoreEither is the half the test
// above cannot measure, and the reason install() calls Rollback explicitly
// rather than leaning on the process ending.
//
// `sapedb install` is a process: it returns, run() closes the pager, and an
// uncommitted transaction dies with it — so the test above stays green even
// with the Rollback taken out, which was measured before this test was
// written rather than assumed. What the Rollback is actually for is the
// caller that does not exit: anything holding this same *store.Store after a
// refused install would otherwise be handed collections that are in the tree
// and will never be committed, which is a worse answer than an error.
//
// So this one calls install() directly, and asks the store it was given.
func TestARefusedInstallLeavesNothingInTheOPENStoreEither(t *testing.T) {
	setup := start(t)
	public, private := author(t, 20)

	opts := options{
		dir: setup.dir, account: "acme", db: "main", secret: secret,
		lookup: setup.env(trusting("north-star", public)),
	}
	db, closeDB, err := open(opts)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer closeDB()

	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "postings", Key: store.Key{Path: "seq", Type: store.TypeNumber},
	}); err != nil {
		t.Fatalf("setting the database up: %v", err)
	}
	if err := db.Commit(); err != nil {
		t.Fatalf("committing the setup: %v", err)
	}

	b := pack()
	b.Collections = append([]store.Spec{{
		Name: "audit_trail",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}}, b.Collections...)

	out := &strings.Builder{}
	if err := install(db, opts, sealedAt(t, b, private), out); err == nil {
		t.Fatal("a colliding bundle installed")
	}

	for _, name := range db.Collections() {
		if name == "audit_trail" {
			t.Fatalf("the store still holds audit_trail after the install was refused: %v", db.Collections())
		}
	}
	if out.Len() != 0 {
		t.Errorf("a refused install printed progress for something that never happened: %q", out.String())
	}
}

// TestTheChangeLogNamesTheBundleAndTheKey is the only record anywhere that a
// declaration came from outside. Nothing else says so: what a bundle
// installs is, afterwards, indistinguishable from a declaration somebody
// wrote by hand.
//
// So the actor has to be the bundle and the key, not the account the command
// was pointed at. The account answers "who typed this", which the shell
// history already answers; the key answers "and where did these declarations
// come from", and it is the one fact on this path that was actually checked
// by anything.
func TestTheChangeLogNamesTheBundleAndTheKey(t *testing.T) {
	setup := start(t)
	public, private := author(t, 13)
	path := sealedAt(t, pack(), private)

	if _, errs, status := setup.runWith(trusting("north-star", public), "", "install", path); status != 0 {
		t.Fatalf("installing: %s", errs)
	}

	logged, errs, status := setup.run("log")
	if status != 0 {
		t.Fatalf("log: %s", errs)
	}

	declared := 0
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var change store.Change
		if err := json.Unmarshal([]byte(line), &change); err != nil {
			t.Fatalf("reading the log back: %v (%q)", err, line)
		}
		if change.Kind != store.ChangeDeclare && change.Kind != store.ChangeOperation {
			continue
		}
		declared++

		for _, want := range []string{
			"ledger-pack",              // which bundle
			"2.1.0",                    // which release of it
			hex.EncodeToString(public), // whose key, in full
			"north-star",               // what this operator calls that key
		} {
			if !strings.Contains(change.By.Actor, want) {
				t.Errorf("entry %d (%s) does not name %q: %q", change.LSN, change.Kind, want, change.By.Actor)
			}
		}
		// The key in full, not shortened: a truncated key cannot be compared
		// against a trust list, which is the only thing anybody would read it
		// for.
		if !strings.Contains(change.By.Actor, hex.EncodeToString(public)) {
			t.Errorf("entry %d does not carry the whole key: %q", change.LSN, change.By.Actor)
		}
		// And not the account. "acme" is what SAPEDB_ACCOUNT is set to in
		// every test in this package and what `sapedb apply` would have
		// recorded here.
		if strings.Contains(change.By.Actor, "acme") {
			t.Errorf("entry %d records the account that ran the command: %q", change.LSN, change.By.Actor)
		}
	}

	// One collection and two operations. A loop that found nothing to check
	// would otherwise pass every assertion above.
	if declared != 3 {
		t.Fatalf("the log holds %d declaration entries, want 3:\n%s", declared, logged)
	}
}

// TestAnUnverifiedBundleIsNotInstalledAndOpensNothing is the fail-closed
// half, asked of the command rather than of internal/bundle.
//
// Each case is refused before the database is opened, which is what the
// tree assertion measures: a bundle nobody trusts must not leave a lock, an
// account folder or an empty database behind on its way to being refused.
func TestAnUnverifiedBundleIsNotInstalledAndOpensNothing(t *testing.T) {
	public, private := author(t, 14)
	stranger, strangerKey := author(t, 15)

	for _, tc := range []struct {
		name  string
		env   map[string]string
		file  func(t *testing.T) string
		wants string
	}{
		{
			name: "no trust list at all",
			env:  map[string]string{},
			file: func(t *testing.T) string { return sealedAt(t, pack(), private) },
			// The empty-list refusal, which is a fact about the server and
			// not about the file: re-downloading the bundle will not help.
			wants: "no signer is trusted on this server",
		},
		{
			name:  "signed by somebody this server does not know",
			env:   trusting("north-star", public),
			file:  func(t *testing.T) string { return sealedAt(t, pack(), strangerKey) },
			wants: "signed by " + hex.EncodeToString(stranger),
		},
		{
			name: "never signed",
			env:  trusting("north-star", public),
			file: func(t *testing.T) string {
				b := pack()
				b.Format = bundle.Label
				return writtenAt(t, b)
			},
			wants: "carries no signature",
		},
		{
			name: "signed, then changed",
			env:  trusting("north-star", public),
			file: func(t *testing.T) string {
				b := pack()
				if err := bundle.Seal(&b, private); err != nil {
					t.Fatal(err)
				}
				// A declaration changed after signing — the scan that was
				// declared to return 25 rows now says 5000.
				b.Operations[1].Limit = 5000
				return writtenAt(t, b)
			},
			wants: "does not match these declarations",
		},
		{
			name: "a trust list that is not one",
			env:  map[string]string{bundle.TrustEnv: "north-star=not-a-key"},
			file: func(t *testing.T) string { return sealedAt(t, pack(), private) },
			// Refused by name rather than treated as an empty list: an
			// operator who mistyped a key must not be told nobody is
			// trusted, which would send them to write a list they already
			// wrote.
			wants: "expected 64 hex characters",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := start(t)
			before := walkTree(t, setup.dir)

			_, errs, status := setup.runWith(tc.env, "", "install", tc.file(t))
			if status == 0 {
				t.Fatal("it installed")
			}
			if !strings.Contains(errs, tc.wants) {
				t.Errorf("the refusal does not say %q: %q", tc.wants, errs)
			}
			assertTreeUnchanged(t, setup.dir, before)
		})
	}
}

// TestVerifyPrintsWhatAnOperatorHasToDecideFrom is the command's whole
// output being the command's whole point.
//
// The two names are the assertion that matters. A bundle's `signer` is
// signed — so it cannot have been changed in transit — and self-asserted —
// so it says nothing about who the author is; the label is what an operator
// of THIS server wrote beside that key by hand. A person comparing the two
// is the entirety of this product's name binding, and a report that printed
// only one of them would be asking them to compare a string against nothing.
func TestVerifyPrintsWhatAnOperatorHasToDecideFrom(t *testing.T) {
	setup := start(t)
	public, private := author(t, 16)
	path := sealedAt(t, pack(), private)

	out, errs, status := setup.runWith(trusting("north-star", public), "", "verify", path)
	if status != 0 {
		t.Fatalf("verifying a good bundle was refused: %s\n%s", errs, out)
	}

	for _, want := range []string{
		`bundle "ledger-pack" version "2.1.0"`,

		// The three checks, each said out loud.
		"read      yes  sapedb/bundle:v1",
		"signed    yes  ed25519, key " + hex.EncodeToString(public),
		"intact    yes  the signature is over exactly these declarations",
		"trusted   yes  an operator of this server put that key on the list",

		// The two names, side by side.
		`says it is "Ledger Authors"`,
		`known as  "north-star"`,

		// And what it would declare, so the decision is about something.
		"collection postings (key id string, ulid)",
		"index by_account [account string missing:skip]",
		"operation postings.get get postings",
		"operation postings.by_account scan postings via by_account limit 25 scopes postings:read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verify did not print %q:\n%s", want, out)
		}
	}

	// A bundle may not carry an operation version — internal/bundle refuses
	// one that does, because a version is a number the receiving store hands
	// out — so printing one here would be printing a field that has no value.
	if strings.Contains(out, "postings.get v0") || strings.Contains(out, "version 0") {
		t.Errorf("verify printed a version for a declaration that cannot carry one:\n%s", out)
	}
}

// TestVerifyNamesEachRefusalSeparately is why verify is a command and not an
// exit status. Four refusals, four different things for the reader to do:
// go and get a signed copy, fetch the file again, ask an operator to add a
// key, or write a trust list at all. A report that collapsed any two of them
// would send somebody to do the wrong one.
//
// Every case asserts the report is printed as well as the refusal, because
// the report is what the command exists for and a refusal that skipped it
// would still exit non-zero and still pass a status-only test.
func TestVerifyNamesEachRefusalSeparately(t *testing.T) {
	public, private := author(t, 17)
	stranger, strangerKey := author(t, 18)

	for _, tc := range []struct {
		name  string
		env   map[string]string
		file  func(t *testing.T) string
		rows  []string
		wants string
	}{
		{
			name: "never signed",
			env:  trusting("north-star", public),
			file: func(t *testing.T) string {
				b := pack()
				b.Format = bundle.Label
				return writtenAt(t, b)
			},
			rows: []string{
				"read      yes",
				"signed    no   the bundle carries no signature",
				"intact    -    not reached",
				"trusted   -    not reached; the signature did not check out",
			},
			wants: "carries no signature",
		},
		{
			name: "signed, then changed",
			env:  trusting("north-star", public),
			file: func(t *testing.T) string {
				b := pack()
				if err := bundle.Seal(&b, private); err != nil {
					t.Fatal(err)
				}
				b.Operations[1].Limit = 5000
				return writtenAt(t, b)
			},
			rows: []string{
				"read      yes",
				"signed    yes  ed25519, key " + hex.EncodeToString(public),
				"intact    no   these are not the declarations that key signed",
				"trusted   -    not reached; the signature did not check out",
			},
			wants: "does not match these declarations",
		},
		{
			name: "a stranger's key",
			env:  trusting("north-star", public),
			file: func(t *testing.T) string { return sealedAt(t, pack(), strangerKey) },
			rows: []string{
				"signed    yes  ed25519, key " + hex.EncodeToString(stranger),
				"intact    yes",
				"trusted   no   no operator of this server put that key on the list",
			},
			wants: "signed by " + hex.EncodeToString(stranger),
		},
		{
			name: "no trust list at all",
			env:  map[string]string{},
			file: func(t *testing.T) string { return sealedAt(t, pack(), private) },
			rows: []string{
				// The signature checks still run and are still reported: an
				// operator about to write their first trust list wants to
				// know whether this file is worth putting a key in it for.
				"signed    yes  ed25519, key " + hex.EncodeToString(public),
				"intact    yes  the signature is over exactly these declarations",
				"trusted   no   this server's trust list is empty; set SAPEDB_TRUST",
			},
			wants: "no signer is trusted on this server",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := start(t)
			before := walkTree(t, setup.dir)

			out, errs, status := setup.runWith(tc.env, "", "verify", tc.file(t))
			if status == 0 {
				t.Fatalf("a refused bundle verified:\n%s", out)
			}
			if !strings.Contains(errs, tc.wants) {
				t.Errorf("the refusal does not say %q: %q", tc.wants, errs)
			}
			for _, row := range tc.rows {
				if !strings.Contains(out, row) {
					t.Errorf("the report does not carry %q:\n%s", row, out)
				}
			}
			// Refused, so nothing below the checks is vouched for, and the
			// heading has to say so rather than presenting a stranger's
			// declarations as though somebody had stood behind them.
			if !strings.Contains(out, "declares (nothing below is vouched for") {
				t.Errorf("a refused bundle's declarations are presented as verified:\n%s", out)
			}
			if !strings.Contains(out, "collection postings") {
				t.Errorf("the report does not say what the refused file would declare:\n%s", out)
			}
			assertTreeUnchanged(t, setup.dir, before)
		})
	}
}

// TestVerifyNeedsNoDatabaseAndNoSecret is what makes verify usable by the
// person it is for: somebody handed a file, deciding whether to trust it,
// who may not hold this server's secret and may not have picked a database
// for it yet.
//
// It also pins that verify takes no lock and opens nothing — the directory
// is untouched afterwards — which is what lets it be run against a host
// whose server is up.
func TestVerifyNeedsNoDatabaseAndNoSecret(t *testing.T) {
	setup := start(t)
	public, private := author(t, 19)
	path := sealedAt(t, pack(), private)

	before := walkTree(t, setup.dir)

	out, errs := &strings.Builder{}, &strings.Builder{}
	// Neither SAPEDB_SECRET nor SAPEDB_ACCOUNT nor SAPEDB_DB: the three
	// things every other command that reads a file insists on.
	env := setup.envOnly(map[string]string{
		"SAPEDB_DIR":    setup.dir,
		bundle.TrustEnv: "north-star=" + hex.EncodeToString(public),
	})
	if status := Run([]string{"verify", path}, env, strings.NewReader(""), out, errs); status != 0 {
		t.Fatalf("verify insisted on something it does not need: %s", errs.String())
	}
	if !strings.Contains(out.String(), `known as  "north-star"`) {
		t.Errorf("verify did not print the operator's label:\n%s", out.String())
	}
	assertTreeUnchanged(t, setup.dir, before)
}

// namespacedPack is pack() with both operations moved into a namespace of the
// author's own (SAPE-9). The namespace is "ledger", which is neither the
// account ("acme") nor the operator's label for the key, so a claim recorded
// against it cannot be satisfied by something else in the tree spelling the
// same.
func namespacedPack() bundle.Bundle {
	b := pack()
	b.Operations[0].Name = "ledger:postings.get"
	b.Operations[1].Name = "ledger:postings.by_account"
	return b
}

// TestABundleClaimsItsNamespaceForItsKeyNotForTheAccount is the guard on the
// one line of this package SAPE-9 changed: install passes the signing key as
// store.Caller.Signer, so a namespace installed from a bundle belongs to the
// key that signed it and not to whoever ran the command.
//
// It is measured through the commands rather than by reading the Caller,
// because "the field is set" and "the namespace is protected" are different
// claims and only the second one is worth anything. `apply` runs as the
// account and opens the file from scratch, so what refuses it is a claim that
// was committed.
func TestABundleClaimsItsNamespaceForItsKeyNotForTheAccount(t *testing.T) {
	setup := start(t)
	public, private := author(t, 16)
	path := sealedAt(t, namespacedPack(), private)

	if _, errs, status := setup.runWith(trusting("north-star", public), "", "install", path); status != 0 {
		t.Fatalf("installing: %s", errs)
	}

	// The known-positive: the account may still declare, both flat and into a
	// namespace nobody has taken. Without this, the refusal below could be
	// "apply is broken" rather than "the namespace is held".
	allowed := setup.write("allowed.json", `{"operations":[
      {"name": "postings.count_all", "collection": "postings", "action": "count",
       "index": "by_account", "limit": 100},
      {"name": "acme:postings.count_ours", "collection": "postings", "action": "count",
       "index": "by_account", "limit": 100}
    ]}`)
	if _, errs, status := setup.run("apply", allowed); status != 0 {
		t.Fatalf("declaring outside the bundle's namespace was refused: %s", errs)
	}

	// And the claim: the same operator, into the key's namespace.
	intruder := setup.write("intruder.json", `{"operations":[
      {"name": "ledger:postings.get", "collection": "postings", "action": "count",
       "index": "by_account", "limit": 100}
    ]}`)
	out, errs, status := setup.run("apply", intruder)
	if status == 0 {
		t.Fatalf("the account declared into the bundle key's namespace and was not refused: %s", out)
	}
	for _, want := range []string{"ledger", hex.EncodeToString(public)} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not name %q: %s", want, errs)
		}
	}
	// The key, not the operator's label for it: a claim recorded against a
	// label would stop matching the moment the operator renamed the key in
	// their trust list.
	if strings.Contains(errs, "north-star") {
		t.Errorf("the claim was recorded against the operator's label rather than the key: %s", errs)
	}

	// Installing the same bundle again is not a collision with itself.
	if _, errs, status := setup.runWith(trusting("north-star", public), "", "install", path); status != 0 {
		t.Fatalf("reinstalling the bundle that holds the namespace was refused: %s", errs)
	}
}
