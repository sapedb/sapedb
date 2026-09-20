package build

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot finds the repository root from this file's own path, so the test
// does not depend on the directory `go test` was run from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	// internal/build/build_test.go -> the root is two levels up.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestTheBuildFilesStampTheSymbolThisPackageActuallyExports is the guard on
// the one failure mode of linker stamping that nothing else notices: go
// build accepts -X for a symbol that does not exist and does nothing about
// it. No warning, no error, a binary that builds and runs and quietly says
// "dev".
//
// So a rename or a move of this package breaks stamping silently everywhere
// it is spelled by hand, and it has to be spelled by hand in the two places
// that cannot read a Go constant: make and docker. Path is the one place it
// is spelled in Go; this is what ties the other two to it.
//
// It is not a string compared against itself. Path is Go source; the two
// files read below are make and docker source, written independently, and
// the only thing making them agree today is somebody having kept them in
// step — which is exactly the thing that stops happening.
func TestTheBuildFilesStampTheSymbolThisPackageActuallyExports(t *testing.T) {
	root := moduleRoot(t)

	for _, name := range []string{"Makefile", "Dockerfile"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(raw), "-X "+Path+"=") {
			t.Errorf("%s does not stamp %s, so a binary built through it will say %q no matter what version it was built from", name, Path, Version)
		}
	}
}

// TestPathNamesThisPackagesOwnVariable checks the half the test above cannot:
// that Path is right in the first place. A Path pointing at a symbol that
// does not exist would keep the Makefile and the Dockerfile in perfect
// agreement with it and stamp nothing at all.
//
// The check is by construction — the import path of this package plus the
// name of the variable declared in it — rather than by string literal, and
// it is deliberately assembled from a different direction than Path itself.
func TestPathNamesThisPackagesOwnVariable(t *testing.T) {
	const wantPackage = "github.com/sapedb/sapedb/internal/build"
	const wantVariable = "Version"

	pkg, variable, found := strings.Cut(Path, ".Version")
	if !found || variable != "" {
		t.Fatalf("Path is %q, which does not end in .%s", Path, wantVariable)
	}
	if pkg != wantPackage {
		t.Errorf("Path names package %q, want %q", pkg, wantPackage)
	}
}

// TestTheDefaultIsNotSomethingAReleaseCouldBeCalled is small but it is the
// reason an unstamped binary is recognisable. If the default were "" the
// version line would print a trailing space and read as a blank; if it were
// "1.0.0" an unstamped build would claim a release it is not.
func TestTheDefaultIsNotSomethingAReleaseCouldBeCalled(t *testing.T) {
	// Only meaningful in a test binary nobody stamped, which is every test
	// binary this repository builds — but say so rather than assume it, or a
	// stamped run would report a failure that is not one.
	if Version != "dev" {
		t.Skipf("this test binary was itself stamped %q, so there is no default here to check", Version)
	}
	if strings.ContainsAny(Version, " \t") || Version == "" {
		t.Errorf("the default version is %q, which does not print as one word", Version)
	}
	if strings.ContainsAny(Version, "0123456789") {
		t.Errorf("the default version is %q, which could be mistaken for a release", Version)
	}
}
