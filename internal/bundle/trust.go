package bundle

import (
	"fmt"
	"strings"
)

// How an operator says whose bundles this server will look at.
//
// # Why an environment variable
//
// Because that is the only configuration surface this product has. Measured
// before it was decided: internal/service.FromEnv reads every setting
// sapedbd has — the address, the directory, the secret, the label, both TLS
// paths, the two flags, the shutdown wait and the leader to follow — out of
// the environment, and there is no configuration file anywhere in the tree,
// no flag parser in cmd/sapedbd, and no path that opens a file to be told
// what to do. internal/cli reads the same environment for the same reasons.
// A trust list in a file of its own would be the first file of its kind
// here, and it would need a path to find it by — which would itself be an
// environment variable, so the file buys nothing that this does not already
// have, and costs a search order, a permissions question and a reload story
// that nothing else in this product has to answer.
//
// What the file would genuinely buy is room: a list of twenty keys is a very
// long line. That is real, and it is the reason to revisit this, not a
// reason to guess now — an operator with twenty trusted authors is a
// situation nobody here has, and SAPE-11 is not the place to design for it.
// Nothing about this shape forecloses a file later: a file would be read
// into the same []TrustedSigner that NewTrust already takes.
//
// # The shape, and what is refused
//
//	SAPEDB_TRUST='acme-eng=3f0b…64 hex, partner-co=9c1d…64 hex'
//
// One entry per trusted signer, written label first. Entries are separated
// by a comma or a newline — both, because an environment variable set in a
// shell is one line and one set from a systemd EnvironmentFile or a
// docker-compose block may not be, and an operator should not have to know
// which of the two this parser wanted.
//
// The label comes first and is separated by the first "=", so a label may
// not contain one; the key never does. It is refused rather than trimmed to
// the last "=", because a label with an "=" in it would parse as a
// different label than the one written, and a trust list that silently
// renames a signer is a trust list whose output nobody can check against
// what they typed.
//
// A label is required, and two entries may not share one. The label is the
// only part of this an operator ever reads back — `sapedb verify` prints it
// beside the name the bundle claims for itself, and it is what the change
// log records against every declaration a bundle installs — so one label
// standing for two keys would make both of those say something untrue. The
// duplicate-key check NewTrust already does is the same rule from the other
// side, and neither replaces the other: one key under two labels and one
// label over two keys are two different mistakes.
//
// Empty entries are skipped, so a trailing comma or a blank line in a
// multi-line value is not an error. An empty variable, or one that is not
// set at all, is an empty list — which is not an error either, and is also
// not permission: Verify refuses every bundle with ErrNoTrust and says why.
// That fail-closed default is SAPE-10's decision and this does not soften
// it. It is the reason this parser has no "trust anything" spelling at all:
// there is no value of SAPEDB_TRUST that means "accept whatever arrives",
// so no deployment can reach that state by mistyping one.

// TrustEnv is the variable an operator writes the trust list into. Named
// here, beside the parser, rather than in the two packages that read it, so
// the CLI and the daemon cannot end up reading two different variables.
const TrustEnv = "SAPEDB_TRUST"

// ParseTrust reads an operator's trust list as it is written in TrustEnv.
//
// The empty string is an empty list and not an error: a server whose
// operator has not decided yet is a legitimate state, and the refusal that
// belongs to it is ErrNoTrust, said by Verify at the moment a bundle is
// actually presented.
func ParseTrust(value string) (*Trust, error) {
	var signers []TrustedSigner
	labelled := map[string]string{}

	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		label, key, split := strings.Cut(entry, "=")
		if !split {
			return nil, fmt.Errorf("%w: %s entry %q is not label=key", ErrKey, TrustEnv, entry)
		}
		label, key = strings.TrimSpace(label), strings.TrimSpace(key)

		if !printable(label) {
			return nil, fmt.Errorf("%w: %s entry %q has no usable label; a label is 1-%d bytes and holds no control characters",
				ErrKey, TrustEnv, entry, TextBytes)
		}
		if previous, already := labelled[label]; already {
			return nil, fmt.Errorf("%w: %s calls two different keys %q, and a label is what a refusal and the change log print; the other one is %s",
				ErrKey, TrustEnv, label, previous)
		}

		labelled[label] = key
		signers = append(signers, TrustedSigner{Label: label, PublicKey: key})
	}

	return NewTrust(signers)
}

// TrustFromEnv is ParseTrust over a lookup function — os.LookupEnv in a real
// process, a map in a test, exactly as internal/service.FromEnv and
// internal/cli both take theirs.
//
// A variable that is set but empty is the same as one that is not set: an
// empty list. That is deliberately NOT the reading internal/cli and
// internal/service give an empty variable elsewhere, where `SAPEDB_DIR=`
// falls back to a documented default; there is no default to fall back to
// here, and an empty list is already the safe state.
func TrustFromEnv(lookup func(string) (string, bool)) (*Trust, error) {
	value, _ := lookup(TrustEnv)
	return ParseTrust(value)
}
