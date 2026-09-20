package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sapedb/sapedb/internal/bundle"
)

// TestEveryBundleRefusalHasACodeOfItsOwn pins the six words SAPE-10 froze.
//
// This test can only read the table, and a table test that only reads the
// table agrees with itself — scope_test.go says exactly that about the scope
// codes, and it is true here too. It is written anyway, and for a narrower
// reason than the one it looks like: these codes have no call site yet,
// because a bundle is verified in front of a server rather than inside one.
// An entry with no call site is the kind a later reader deletes as dead, and
// deleting one is a silent change to the tagged wire contract rather than a
// tidy-up. The literal strings below are what make that deletion loud.
//
// The wrapping is not decoration either. Every refusal in internal/bundle
// reaches a caller through fmt.Errorf("%w: ..."), so what codeFor actually
// receives is never the bare sentinel, and a table that matched only bare
// sentinels would pass this test if it asserted them and still report
// `failed` in production.
func TestEveryBundleRefusalHasACodeOfItsOwn(t *testing.T) {
	for _, known := range []struct {
		err  error
		code string
	}{
		{bundle.ErrUnsigned, "bundle_unsigned"},
		{bundle.ErrBadSignature, "bundle_signature"},
		{bundle.ErrUntrusted, "bundle_untrusted"},
		{bundle.ErrNoTrust, "bundle_no_trust"},
		{bundle.ErrKey, "bundle_key"},
		{bundle.ErrBundle, "bundle"},
	} {
		if got := codeFor(known.err); got != known.code {
			t.Errorf("codeFor(%v) = %q, want %q", known.err, got, known.code)
		}
		wrapped := fmt.Errorf("%w: as a caller of this package actually sees it", known.err)
		if got := codeFor(wrapped); got != known.code {
			t.Errorf("codeFor(wrapped %v) = %q, want %q", known.err, got, known.code)
		}
	}

	// And the six are six. A bundle refusal that collapsed into an existing
	// code — "signature" is the one it would collapse into — would tell a
	// client that a connection string failed to verify.
	if errors.Is(bundle.ErrBadSignature, ErrHandshake) || codeFor(bundle.ErrBadSignature) == "signature" {
		t.Error("a bundle's signature refusal must not be a connection string's")
	}
}
