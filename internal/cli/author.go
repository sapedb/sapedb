package cli

// The author's half of a bundle: the two commands that make internal/bundle
// reachable from the other side.
//
// SAPE-28 shipped `verify` and `install` and said, in its own report, exactly
// what was still missing: nothing in the shipped binary could PRODUCE a
// bundle. An author outside this repository had to write Go against
// bundle.Seal, which means there was no path at all — "publish an operation
// module" was a feature with a consumer and no producer. These two commands
// are the producer.
//
// `sapedb keygen FILE` makes the identity. `sapedb seal DRAFT FILE` turns a
// declaration file into a signed bundle. Between them they are the whole of
// what an author does, and neither of them touches a database: both are
// standalone for the reason `verify` is, and more so — an author signing a
// release is very often not an operator of any server at all, and demanding a
// secret, an account and a database name from them would be demanding three
// things that do not exist on their machine.
//
// # Why a key is a file and not a printed value
//
// keygen takes the path to write the private key to, and prints only the
// public half. The alternative — print both and let the author redirect — was
// rejected: a command that writes a private key to stdout writes it to the
// terminal of every author who forgets the redirect, into their scrollback,
// into `script` output, and into the CI log of anybody who runs it in a
// pipeline. There is no undo for that, and the failure is silent: the key
// still works, so nothing ever complains. Writing to a named file makes the
// author choose the location before the key exists, which is the moment they
// are actually thinking about it.
//
// What IS printed is the public key alone, one line, with no label and no
// prose around it, so that
//
//	SAPEDB_TRUST="acme-eng=$(sapedb keygen acme.key)"
//
// is the whole of getting a new author onto an operator's trust list. A
// friendly "wrote your key to acme.key" line would break that, and would tell
// the author only the path they just typed.
//
// # Why the private key comes back in on stdin
//
// `seal` reads the key from stdin, not from an argument and not from the
// environment. Arguments are the rule this package already wrote down for
// passwords — anybody who can run ps can read them — and while a key PATH is
// not itself a secret, stdin buys something a path cannot: the key never has
// to exist on disk at all. `vault read -field=key ... | sapedb seal draft.json
// orders.bundle` is a complete release pipeline in which the signing key is
// never a file, never in an environment block a crashed process dumps, and
// never in argv. The file that keygen writes is the simple case of the same
// thing, reached with `< acme.key`.
//
// # Why the draft is a bundle without its signature
//
// No new syntax was invented, because inventing one would have been the
// second declaration format in a product that already has one. A draft is
// exactly the JSON `sapedb verify` reads, minus the signature block:
//
//	{"name": ..., "version": ..., "signer": ...,
//	 "collections": [...], "operations": [...]}
//
// and those last two lists are, field for field, the two lists `sapedb apply`
// already reads out of a schema file — see the schema type in cli.go, which
// declares the same two. An author who already deploys with `apply` turns
// their schema file into a draft by adding three strings to it. Nothing about
// the declarations changes, which is the point: a bundle was always meant to
// be "an apply file with a name, an author and a signature on it", and this
// keeps the file an author edits the same shape as the thing it becomes.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/sapedb/sapedb/internal/bundle"
)

// keyFileMode is what a private key file is created with. Owner read/write and
// nothing else: the file IS the identity, and a key another account on the
// host can read is an identity another account on the host holds.
const keyFileMode = 0o600

// bundleFileMode is what a sealed bundle is written with. Deliberately not
// keyFileMode: a bundle is the thing an author hands out, it holds only the
// public key, and a release artifact nobody but its author can read is a
// release artifact that has to be chmodded before it can be shipped.
const bundleFileMode = 0o644

// checkKeygen refuses a call that would go on to create something.
//
// The "already there" check is here rather than only in the O_EXCL below,
// which is the belt to that brace, for the reason every other check in this
// package runs at this stage: a refusal that has already created the account
// directory is a refusal that changed the disk. It is also a much better
// sentence than the one the OS produces — "file exists" does not say that the
// thing at risk was an identity.
func checkKeygen(opts options, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: keygen takes one file to write the private key into, and got %q", ErrUsage, args)
	}
	if _, err := os.Stat(args[0]); err == nil {
		return fmt.Errorf("%w: there is already a file at %s, and keygen will not write over a key: "+
			"a signing identity that gets overwritten cannot be recovered, and every bundle it ever signed "+
			"stops being attributable to anybody", ErrUsage, args[0])
	}
	return nil
}

// keygen makes an ed25519 identity: the private half into the file named, the
// public half onto out.
//
// O_EXCL, not a second Stat: checkKeygen has already looked, and the gap
// between looking and creating is exactly where a key gets clobbered by a
// second copy of this command. The check is for the message; this is for the
// truth.
func keygen(path string, out io.Writer) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}

	secret, err := bundle.PrivateKeyText(private)
	if err != nil {
		return err
	}
	shown, err := bundle.PublicKeyText(public)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, secret+"\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	// Only now, and only this. The private key has been written and is not on
	// this stream and never was.
	_, err = fmt.Fprintln(out, shown)
	return err
}

// checkSeal is everything about a seal call that can be decided from argv:
// two paths, and a draft that reads as a draft.
//
// It parses the draft here and seal() parses it again, which is the duplicate
// checkApply already makes against apply() and is made here for the same
// written reason: two independent readings keep a mutation that guts one of
// them from silently changing what the other does. What it deliberately does
// NOT do is touch stdin — stdin is not argv, and a check that consumed the
// key would hand seal() an already-read stream.
func checkSeal(opts options, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%w: seal takes a draft file and a bundle file, and got %q", ErrUsage, args)
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	_, err = draft(raw, args[0])
	return err
}

// draft reads the unsigned bundle an author wrote.
//
// bundle.Parse rather than a decoder of its own, so that an author's draft is
// held to exactly the rule a bundle is held to on the way in: unknown fields
// refused, one document per file. A field this version does not read is a
// field an author believes is doing something, and signing it silently is how
// a signed message grows a place to hide a mistake.
func draft(raw []byte, path string) (bundle.Bundle, error) {
	b, err := bundle.Parse(raw)
	if err != nil {
		return bundle.Bundle{}, fmt.Errorf("%s: %w", path, err)
	}

	// A draft may leave format out — seal writes it — but if it says
	// anything, it has to say this. Relabelling a file that claims to be some
	// other format is the one thing that must not happen quietly: the author
	// believed it was something else.
	if b.Format != "" && b.Format != bundle.Label {
		return bundle.Bundle{}, fmt.Errorf("%w: %s says its format is %q, and seal only writes %q",
			ErrUsage, path, b.Format, bundle.Label)
	}

	// A draft carrying a signature is refused rather than re-signed. Seal
	// would throw the old one away and nothing would be wrong with the
	// result, but the author handed over a file that already claimed to be
	// signed by somebody, and quietly replacing that claim with a different
	// one is how a release gets signed by the wrong key without anybody
	// noticing. Sealing an already-sealed bundle is re-sealing it; say so.
	if b.Signature != nil {
		return bundle.Bundle{}, fmt.Errorf("%w: %s already carries a signature, and seal will not replace one: "+
			"start from the draft it was sealed from", ErrUsage, path)
	}
	return b, nil
}

// seal signs a draft with the key on in, and writes the bundle to path.
//
// Nothing is written until the signature exists. A half-written bundle file
// would be a file `verify` refuses for the wrong reason — "that is not a
// bundle" instead of "your key was not readable" — and an author debugging
// that is debugging the wrong end of their pipeline.
func seal(draftPath, path string, in io.Reader, out io.Writer) error {
	raw, err := os.ReadFile(draftPath)
	if err != nil {
		return err
	}
	b, err := draft(raw, draftPath)
	if err != nil {
		return err
	}

	// io.ReadAll and not a line reader: a key arriving down a pipe from a
	// secret store may not end in a newline, and one read out of a file does.
	// ParsePrivateKey trims, so both are the same key.
	typed, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if len(typed) == 0 {
		return fmt.Errorf("%w: seal reads the signing key from stdin, and stdin was empty; "+
			"try `sapedb seal %s %s < your.key`", ErrUsage, draftPath, path)
	}
	private, err := bundle.ParsePrivateKey(string(typed))
	if err != nil {
		return err
	}

	if err := bundle.Seal(&b, private); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}

	// Read back what is about to be written, before it is written (ISS-35).
	//
	// A bundle is not the author's file; it is a re-emission of it. The
	// declarations go out as marshalled Go values, so every constant in one is
	// spelled the way a float64 prints — and for a narrow class of integers
	// that is not the spelling the author wrote. 9223372036854775808 is
	// accepted (float64 holds 2^63 exactly, and nothing is lost) and comes
	// back out as 9223372036854776000, which is a DIFFERENT integer and not
	// one a float64 holds exactly. So the bundle seal wrote would be refused
	// by the install that read it.
	//
	// That refusal is real but it lands in the wrong place: on an operator,
	// days later, holding a signed artifact they cannot use and a message
	// about a number they never typed. Sealing is the moment the author is
	// still looking at the file, so the refusal belongs here.
	//
	// It is the same rule and the same reader — bundle.Parse, the function
	// `verify` and `install` come through — asked about the bytes rather than
	// about the draft. Nothing is written when it says no, which is the
	// property this function's own doc already claims about signing.
	if _, err := bundle.Parse(encoded); err != nil {
		return fmt.Errorf("%w (a bundle carries its declarations as JSON, and this value cannot be written into one "+
			"and read back as itself — send it as a string, or use a value a float64 holds exactly)", err)
	}

	if err := os.WriteFile(path, append(encoded, '\n'), bundleFileMode); err != nil {
		return err
	}

	// The public key, not the file name, and not the private key. It is what
	// the author sends to whoever has to put them on a trust list, and
	// printing it here means the value is available at the moment it is
	// needed even to an author who has mislaid the line keygen printed.
	_, err = fmt.Fprintf(out, "sealed %s, signed by %s\n", path, b.Signature.PublicKey)
	return err
}
