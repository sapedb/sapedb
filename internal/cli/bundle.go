package cli

// Installing a bundle: the two commands that make internal/bundle reachable.
//
// SAPE-10 stopped at "sealed and verified" and said so: nothing anywhere
// read a trusted key out of a configuration, and none of the six refusal
// codes in internal/server had a call site. This file is what makes them
// reachable, and it is two commands rather than one on purpose.
//
// `sapedb verify FILE` answers a question somebody asks BEFORE installing:
// is this file from who it says, and what would it do to my database. It
// opens no database, takes no lock, needs no secret and touches no network —
// a person deciding whether to trust a file must be able to ask without
// committing to anything, and without the answer depending on a server being
// up. So it prints rather than exits quietly: what it prints is the point of
// the command, and the exit status is only the summary.
//
// `sapedb install FILE` is `sapedb apply` with an author attached. It runs
// the same db.Declare / db.DeclareOperation loop over the same two lists —
// which is the whole reason a bundle carries those two lists and not a
// container of its own — and differs from apply in exactly two ways, both of
// which are the reason this is not just `apply` with a flag:
//
//   - It refuses the file unless Trust.Verify accepts it, before the
//     database is opened at all.
//   - It records the bundle and the key in the change log, not the account
//     that ran the command. See installedBy.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sapedb/sapedb/internal/bundle"
	"github.com/sapedb/sapedb/internal/store"
)

// trusted is the operator's trust list, read out of the environment this
// command was given. A nil lookup — an options built by hand in a test — is
// an empty environment, which is an empty list, which is the same
// fail-closed answer an unset variable gets: no signer is trusted here, so
// no bundle can be.
func (o options) trusted() (*bundle.Trust, error) {
	if o.lookup == nil {
		return bundle.ParseTrust("")
	}
	return bundle.TrustFromEnv(o.lookup)
}

// checkVerify and checkInstall are the argument checks. Both read the file
// and put it all the way through verification, before run() opens anything,
// for the reason every other check in this package runs there: a refused
// command must not take the directory lock or create an account folder on
// its way to being refused. For install that is the whole difference between
// "this bundle is not from anybody I trust" and "this bundle is not from
// anybody I trust, and by the way there is now a database file where there
// was not one".
//
// It costs reading and verifying the file twice, which is the same trade
// checkApply already makes against apply() and is made here for the same
// stated reason: a duplicate keeps a mutation that guts one of them from
// silently changing what the other does.
func checkVerify(opts options, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: verify takes one bundle file, and got %q", ErrUsage, args)
	}
	if _, err := os.ReadFile(args[0]); err != nil {
		return err
	}
	// Not verified here. verify's entire job is to report a refusal in
	// detail, so a refusal reached at check time would print the one-line
	// error and skip the report the command exists to produce.
	_, err := opts.trusted()
	return err
}

func checkInstall(opts options, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: install takes one bundle file, and got %q", ErrUsage, args)
	}
	_, _, _, err := verified(opts, args[0])
	return err
}

// verified is read, parse, and the whole trust decision, in the order
// internal/bundle put them in.
//
// Trust.Verify is the authority on whether this bundle may be installed —
// it is what answers an empty trust list first, and what checks the
// signature before it consults the list at all, both for reasons SAPE-10
// wrote down. Check is called afterwards and only to get the key back, since
// Verify deliberately returns the operator's label instead. It is not a
// second opinion: by the time it runs, Verify has already said yes.
func verified(opts options, path string) (bundle.Bundle, string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return bundle.Bundle{}, "", "", err
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		return bundle.Bundle{}, "", "", fmt.Errorf("%s: %w", path, err)
	}

	trust, err := opts.trusted()
	if err != nil {
		return bundle.Bundle{}, "", "", err
	}
	label, err := trust.Verify(b)
	if err != nil {
		return bundle.Bundle{}, "", "", fmt.Errorf("%s: %w", path, err)
	}

	key, err := bundle.Check(b)
	if err != nil {
		return bundle.Bundle{}, "", "", fmt.Errorf("%s: %w", path, err)
	}
	return b, hex.EncodeToString(key), label, nil
}

// verify prints what a bundle is, what checked out, and what it would
// declare — and then returns whatever Verify returned, so the exit status
// says yes or no while the report says why.
//
// The report is written to out even when the bundle is refused, and this is
// the decision the command is for. An operator handed a file wants to know
// which of four different things went wrong, because each one is a different
// action: fetch it again, ask the author to sign it, ask an operator to add
// a key, or write a trust list at all. A one-line error names the refusal;
// the report also names the key that was presented, the name the bundle
// claims for its author, and the declarations, so a refusal can be taken to
// the person who sent the file.
func verify(opts options, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		// Nothing was read, so there is nothing to report about. This is the
		// one refusal with no report: every other one below has a parsed
		// bundle to describe.
		return fmt.Errorf("%s: %w", path, err)
	}

	trust, err := opts.trusted()
	if err != nil {
		return err
	}

	// Both, and in this order. Verify is the decision and is what this
	// command's exit status reports; Check is what can tell the difference
	// between "not signed" and "signed, but not over this" for the report,
	// which Verify's single error cannot once the trust list is empty.
	_, intact := bundle.Check(b)
	label, decision := trust.Verify(b)

	report := &bytes.Buffer{}
	fmt.Fprintf(report, "bundle %q version %q\n\n", b.Name, b.Version)

	presented := ""
	if b.Signature != nil {
		presented = b.Signature.PublicKey
	}

	switch {
	case b.Format != bundle.Label:
		row(report, "read", "no", fmt.Sprintf("format is %q, not %q", b.Format, bundle.Label))
		row(report, "signed", "-", "not reached")
		row(report, "intact", "-", "not reached")
	case errors.Is(intact, bundle.ErrUnsigned):
		row(report, "read", "yes", bundle.Label)
		row(report, "signed", "no", "the bundle carries no signature, so there is nobody to check it against")
		row(report, "intact", "-", "not reached")
	case errors.Is(intact, bundle.ErrBadSignature):
		row(report, "read", "yes", bundle.Label)
		row(report, "signed", "yes", bundle.Algorithm+", key "+presented)
		row(report, "intact", "no", "these are not the declarations that key signed")
	case intact != nil:
		row(report, "read", "yes", bundle.Label)
		row(report, "signed", "no", intact.Error())
		row(report, "intact", "-", "not reached")
	default:
		row(report, "read", "yes", bundle.Label)
		row(report, "signed", "yes", bundle.Algorithm+", key "+presented)
		row(report, "intact", "yes", "the signature is over exactly these declarations")
	}

	switch {
	case decision == nil:
		row(report, "trusted", "yes", "an operator of this server put that key on the list")
	case errors.Is(decision, bundle.ErrNoTrust):
		row(report, "trusted", "no", fmt.Sprintf("this server's trust list is empty; set %s", bundle.TrustEnv))
	case errors.Is(decision, bundle.ErrUntrusted):
		row(report, "trusted", "no", "no operator of this server put that key on the list")
	default:
		row(report, "trusted", "-", "not reached; the signature did not check out")
	}

	// The two names, side by side, because the difference between them is
	// the only identity fact a bundle carries. The one on the left is
	// signed and therefore cannot have been changed in transit, and is
	// written by whoever holds the key and therefore says nothing about who
	// that is. The one on the right is what an operator of this server
	// wrote beside that key by hand. A person comparing them is the whole
	// of this product's name binding; there is no PKI here to do it for
	// them.
	fmt.Fprintln(report)
	fmt.Fprintf(report, "  %-9s %q %s\n", "says it is", b.Signer, "(the bundle's own claim about its author)")
	if label != "" {
		fmt.Fprintf(report, "  %-9s %q %s\n", "known as", label, "(what an operator of this server called that key)")
	} else {
		fmt.Fprintf(report, "  %-9s %s\n", "known as", "nothing on this server")
	}

	fmt.Fprintln(report)
	if decision == nil {
		fmt.Fprintln(report, "declares")
	} else {
		// Said out loud rather than left off. What the file carries is
		// still worth reading when it was refused — it is how an operator
		// decides whether adding the key is a thing they want to do — but
		// on a refused bundle these declarations are not vouched for by
		// anybody, and a heading that did not say so would read as though
		// they were.
		fmt.Fprintln(report, "declares (nothing below is vouched for: the checks above did not pass)")
	}
	describe(report, b)

	if _, err := out.Write(report.Bytes()); err != nil {
		return err
	}
	if decision != nil {
		// Named by path, the same as every other refusal on this path: the
		// report above names the bundle, and the bundle's name is its own
		// claim about itself, not the file somebody was handed.
		return fmt.Errorf("%s: %w", path, decision)
	}
	return nil
}

// row is one line of the check list.
func row(out io.Writer, name, state, detail string) {
	fmt.Fprintf(out, "  %-9s %-3s  %s\n", name, state, detail)
}

// describe prints the declarations a bundle carries, in the shapes `sapedb
// ls` prints the ones a database already holds.
//
// Deliberately the same wording rather than shared code: ls reads a Spec the
// store has assigned ids and counters to, and this reads one that carries
// none, because declarable() refuses a bundle that claims any. Sharing a
// printer would mean one function whose output silently depends on which of
// those two it was handed.
func describe(out io.Writer, b bundle.Bundle) {
	if len(b.Collections) == 0 && len(b.Operations) == 0 {
		fmt.Fprintln(out, "  nothing")
		return
	}

	for _, spec := range b.Collections {
		fmt.Fprintf(out, "  collection %s (key %s %s", spec.Name, spec.Key.Path, spec.Key.Type)
		if spec.Key.Auto != "" {
			fmt.Fprintf(out, ", %s", spec.Key.Auto)
		}
		fmt.Fprintln(out, ")")

		for _, index := range spec.Indexes {
			flags := ""
			if index.Unique {
				flags += " unique"
			}
			if index.Array != "" {
				flags += " array:" + index.Array
			}
			fmt.Fprintf(out, "    index %s%s [%s]\n", index.Name, flags, describeFields(index.Fields))
		}
		for _, rollup := range spec.Rollups {
			totals := []string{}
			if rollup.Count {
				totals = append(totals, "count")
			}
			for _, path := range rollup.Sum {
				totals = append(totals, "sum "+path)
			}
			fmt.Fprintf(out, "    rollup %s [%s] %s\n", rollup.Name, describeFields(rollup.Group), strings.Join(totals, ", "))
		}
	}

	for _, operation := range b.Operations {
		// No version. A bundle may not carry one — declarable() refuses a
		// bundle that does, because a version is a number the receiving
		// store hands out — so printing a "v0" here would be printing a
		// field that does not exist as though it had a value.
		fmt.Fprintf(out, "  operation %s %s %s", operation.Name, operation.Action, operation.Collection)
		if operation.Index != "" {
			fmt.Fprintf(out, " via %s", operation.Index)
		}
		if operation.Limit > 0 {
			fmt.Fprintf(out, " limit %d", operation.Limit)
		}
		if len(operation.Scopes) > 0 {
			fmt.Fprintf(out, " scopes %s", strings.Join(operation.Scopes, ","))
		}
		fmt.Fprintln(out)
	}
}

func describeFields(list []store.Field) string {
	written := make([]string, 0, len(list))
	for _, field := range list {
		mark := ""
		if field.Descending {
			mark = " desc"
		}
		written = append(written, field.Path+" "+field.Type+mark+" missing:"+field.Missing)
	}
	return strings.Join(written, ", ")
}

// installedBy is what the change log records against every declaration this
// bundle puts in.
//
// Not the account. `sapedb apply` records `opts.account + " (apply)"` and
// says in its own comment what that is worth: a label, not a proof, because
// anybody holding the file's exclusive lock could have written the same
// bytes by hand. For a bundle there is something better available and it
// would be a waste not to record it — the key is the one fact here that was
// actually checked, by ed25519, over exactly these declarations. Recording
// the operator who ran the command instead would answer "who typed this",
// which the shell history already answers, and lose the only answer to "and
// where did these declarations come from", which nothing else records at
// all: a bundle leaves no other trace, since what it installs is
// indistinguishable afterwards from a declaration somebody wrote by hand.
//
// All four parts are here because each is the answer to a different
// question. The key is the identity, and it is written in full rather than
// abbreviated: it is what a later reader compares against a trust list, and
// a truncated key cannot be compared against anything. The label is what
// this operator called that key AT THE TIME — a later rename of the trust
// list does not rewrite the log, which is correct, because the log is what
// happened. The name and version are the bundle's own, self-asserted and
// signed: they cannot have been altered in transit and they are what an
// author will quote back when asked which release this was.
func installedBy(b bundle.Bundle, key, label string) string {
	return fmt.Sprintf("bundle %q %q signed by %s, trusted here as %q", b.Name, b.Version, key, label)
}

// install declares everything a verified bundle carries, or declares none of
// it.
//
// All or nothing is not a nicety here, and it is the reason this is not a
// loop over apply(). A bundle is one artifact from one author: half of it is
// not a smaller version of it, it is a database holding collections whose
// operations were never declared, or operations naming a collection that is
// not there — and there is no uninstall, so nothing can put that back. A
// half-applied bundle is also the one state that cannot be recovered by
// running the command again, because the declarations that DID land will be
// refused the second time if the reason the run stopped was that one of them
// collided.
//
// It is achieved the way apply() achieves it, by the same mechanism, and
// with one addition. Everything below writes into one open transaction and
// there is exactly one Commit, at the end, after the last declaration; any
// earlier return reaches it never. The addition is the explicit Rollback in
// refuse(): apply() can rely on the process exiting and the pager closing an
// uncommitted transaction, because apply() IS the process. This is not
// allowed to rely on that — a caller in the same process that asked for the
// catalogue after a refused install would otherwise be handed collections
// that are in the tree but will never be committed, which is a worse answer
// than an error.
func install(db *store.Store, opts options, path string, out io.Writer) error {
	b, key, label, err := verified(opts, path)
	if err != nil {
		return err
	}

	// Buffered for the reason apply() buffers: a terminal has no equivalent
	// of the transaction the database rolled back, so a line printed for a
	// collection that a later refusal un-declared would be the only surviving
	// record of something that never happened.
	var progress bytes.Buffer
	// Signer as well as Actor, and they are not the same string on purpose.
	// Actor is the sentence the change log gets, which names the bundle, its
	// version, the key and the operator's label for it, because a log is read
	// by a person. Signer is the key on its own, because it is compared by a
	// machine: it is what a namespace claim is recorded against, and a claim
	// recorded against a sentence would stop matching the moment the operator
	// renamed the key in their trust list. See store.Caller.Signer.
	by := store.Caller{Actor: installedBy(b, key, label), Signer: key}
	fmt.Fprintf(&progress, "installing bundle %q version %q, signed by %s, trusted here as %q\n",
		b.Name, b.Version, key, label)

	for _, spec := range b.Collections {
		if _, err := db.Declare(by, spec); err != nil {
			// %w, and nothing in front of it that flattens the sentinel. A
			// collision with a collection this database already declared
			// differently is store.ErrIncompatible, and it has to arrive as
			// that: it is the one refusal an operator can actually act on —
			// two bundles fighting over a name, which internal/bundle's own
			// doc says is settled here and not at verification — and
			// internal/server's codeFor turns it into the "incompatible"
			// code a client switches on. Wrapped into a plain fmt.Errorf
			// with no %w it would arrive as "failed".
			return refuse(db, fmt.Errorf("bundle %q: collection %q: %w", b.Name, spec.Name, err))
		}
		fmt.Fprintf(&progress, "collection %s\n", spec.Name)
	}

	for _, operation := range b.Operations {
		// The same "already there, unchanged" skip apply() makes, and for
		// the same reason: re-installing the release that is already
		// installed must not hand every caller a new version number to
		// migrate to. A bundle is if anything more likely to be applied
		// twice than a schema file — it is a release artifact, and the
		// obvious thing to do with one after a restore is install it again.
		before, found, err := db.Operation(operation.Name, 0)
		if err != nil && !errors.Is(err, store.ErrNoOperation) {
			return refuse(db, err)
		}
		if found && sameOperation(before, operation) {
			fmt.Fprintf(&progress, "operation %s unchanged at version %d\n", operation.Name, before.Version)
			continue
		}

		stored, err := db.DeclareOperation(by, operation)
		if err != nil {
			return refuse(db, fmt.Errorf("bundle %q: operation %q: %w", b.Name, operation.Name, err))
		}
		fmt.Fprintf(&progress, "operation %s version %d\n", stored.Name, stored.Version)
	}

	if err := db.Commit(); err != nil {
		return refuse(db, err)
	}
	_, err = out.Write(progress.Bytes())
	return err
}

// refuse throws away everything the install had written and returns why it
// stopped.
//
// errors.Join rather than replacing one error with the other: a rollback
// that itself fails is a second, worse problem, and reporting only it would
// hide the collision that started this, while reporting only the collision
// would hide a database that may now be holding a transaction nobody closed.
// Join keeps errors.Is working through both, which is what internal/server's
// codeFor walks and what the all-or-nothing test asserts on.
func refuse(db *store.Store, err error) error {
	if back := db.Rollback(); back != nil {
		return errors.Join(err, fmt.Errorf("and rolling back what it had already declared failed: %w", back))
	}
	return err
}
