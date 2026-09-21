package bundle

// Reading a bundle's cost envelopes out of the file, before anything is
// installed anywhere.
//
// SAPE-12's fourth criterion has two halves. "Matches what it does" was
// already held up by a live test that runs each operation and compares the
// answer against the envelope. "Readable BEFORE installation" was not: the
// only way to see an envelope was to install the bundle into a database and
// read the catalogue, which is backwards — the envelope exists so somebody
// can decide whether to trust an operation before running it, and an answer
// you can only get by installing the thing first is not an answer to that
// question.
//
// Nothing new is computed here. store.EnvelopeOf is the same derivation the
// store uses; all this file supplies is the one thing that derivation needs
// and a file does not have on its own — a way to look up an operation a step
// calls. See operations below for why, for a bundle, that lookup can only
// ever refuse.

import (
	"errors"
	"fmt"

	"github.com/sapedb/sapedb/internal/store"
)

// ErrNotInThisFile is a cost that is not readable from the file alone.
//
// It is not a refusal of the bundle and must not be reported as one. A bundle
// whose operations call other operations is perfectly installable; what
// cannot be done is read its ceiling out of the file, because the declaration
// that carries the rest of that ceiling is not in the file. Saying so is the
// honest answer, and it is the whole reason this is an error rather than a
// number: an envelope that guessed would be a number that disagrees with what
// the store says afterwards, which is exactly the disagreement criterion 4
// exists to rule out.
var ErrNotInThisFile = errors.New("sapedb/bundle: this cost is not written in this file")

// Reading is one operation's cost envelope as it reads out of the bundle, or
// the reason it does not read out of the bundle at all.
//
// Both shapes are reported rather than one of them being dropped. An operator
// looking at a module wants the envelopes that ARE readable even when one of
// them is not, and an operation quietly missing from the list would read as a
// module with fewer operations in it than it has.
type Reading struct {
	// Operation is the name the bundle declares. Always set, including when
	// Err is.
	Operation string

	// Envelope is the derived envelope, valid only when Err is nil.
	//
	// Its Version is zero and will stay zero: declarable() refuses a bundle
	// that carries a version, because a version is a number the receiving
	// store hands out. Everything else in it is the same field the store will
	// report once this bundle is installed — that equality is the criterion,
	// and TestTheEnvelopeReadFromABundleIsTheEnvelopeReadFromTheStore
	// measures it by doing both and comparing.
	Envelope store.Envelope

	// Err is why this one could not be read here, wrapping ErrNotInThisFile
	// when the reason is a reference the file does not carry.
	Err error
}

// Envelopes derives the cost envelope of every operation this bundle
// declares, in the order the bundle declares them, without a store and
// without installing anything.
func Envelopes(b Bundle) []Reading {
	readings := make([]Reading, 0, len(b.Operations))
	for _, operation := range b.Operations {
		reading := Reading{Operation: operation.Name}
		envelope, err := store.EnvelopeOf(operation, operations(b))
		if err != nil {
			reading.Err = err
		} else {
			reading.Envelope = envelope
		}
		readings = append(readings, reading)
	}
	return readings
}

// operations is the lookup store.EnvelopeOf resolves a composed step through,
// and for a bundle it always refuses. That is a conclusion rather than a
// shortcut, and it follows from two rules that were already written down
// independently of each other:
//
//   - A bundle's own operations carry NO version. declarable() refuses a
//     bundle that carries one, because a version is the receiving store's
//     account of when it was declared, and an author writing their own would
//     be writing somebody else's record.
//
//   - A composed step MUST pin a version, and a positive one. That is N1 in
//     compose.go: store.validateComposedStep refuses `version <= 0` outright,
//     because a reference that followed whichever version is newest would
//     make the limit the enclosing operation declares stop being true the
//     moment somebody redeclared the callee, with nobody told.
//
// Put together: a reference inside a bundle can only ever name a version, and
// a bundle's own operations have no version to be named by. So the target of
// any reference this file carries is a declaration in the STORE the bundle is
// installed into — possibly an earlier release of an operation this same
// bundle also declares, possibly something another author installed, and
// there is no way to tell which from here.
//
// Three things were considered and all three are worse than refusing:
//
//   - Match the reference by name against this bundle's own operations,
//     ignoring the version. That is a guess about identity. The store will
//     resolve `x@2` to whatever x@2 actually is, which on a store that
//     already holds an older x is not the x in this file — so the number
//     printed before installation would differ from the number read after
//     it, which is the one failure mode this criterion is about.
//
//   - Sum what is readable and leave the rest out. A ceiling with a step
//     missing from it is a smaller number than the truth, presented in the
//     place a person reads to decide whether they can afford to run
//     something. An under-reported ceiling is worse than no ceiling.
//
//   - Report the enclosing operation's own declared limit instead. N5 makes
//     that a true upper bound, so it is not a lie — but it is not what the
//     store reports either (the store reports the resolved sum, which may be
//     lower), and a field that silently means a different thing before and
//     after installation is the drift this whole design is arranged to
//     prevent.
//
// So: no number, and the reason, naming the reference. An operator who needs
// that one envelope still has the old route — install into a scratch database
// and read the catalogue — and now they are told when they need it, instead
// of it being the only route for everything.
func operations(b Bundle) store.Operations {
	declaredHere := make(map[string]bool, len(b.Operations))
	for _, operation := range b.Operations {
		declaredHere[operation.Name] = true
	}

	return func(name string, version int) (store.Operation, bool, error) {
		if declaredHere[name] {
			return store.Operation{}, false, fmt.Errorf(
				"%w: a step calls %q version %d, and this bundle declares an operation of that name but at no version — only the store it is installed into assigns one, so this reference is to whatever that store already holds and its cost is readable there",
				ErrNotInThisFile, name, version)
		}
		return store.Operation{}, false, fmt.Errorf(
			"%w: a step calls %q version %d, which this bundle does not declare — its cost is in the store this bundle is installed into and not in this file",
			ErrNotInThisFile, name, version)
	}
}
