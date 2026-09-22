package build

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
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
// It is not a string compared against itself. Path is Go source; the files
// read below are make, docker and prose, written independently, and the only
// thing making them agree today is somebody having kept them in step — which
// is exactly the thing that stops happening.
//
// README.md is in the list because it is the third hand-written spelling and
// the one nobody thinks of as a build file: it hands a stranger a `go build
// -ldflags` line to paste. A rename that misses it leaves the README teaching
// a stamp that quietly does nothing, which is worse than a README that says
// nothing at all.
func TestTheBuildFilesStampTheSymbolThisPackageActuallyExports(t *testing.T) {
	root := moduleRoot(t)

	for _, name := range []string{"Makefile", "Dockerfile", "README.md"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(raw), "-X "+Path+"=") {
			t.Errorf("%s does not stamp %s, so a binary built through it will say %q no matter what version it was built from", name, Path, Version)
		}
	}
}

// aStamp matches a linker stamp written out as text: -X, a package path, a
// dot, a variable name, an equals sign. The value after the equals is not
// part of it — make and docker both write a variable reference there.
//
// It deliberately does not match the prose around these files, which mentions
// -X constantly without ever writing a whole stamp: "an -X for a symbol that
// does not exist" has no dotted path and no equals sign, and does not match.
var aStamp = regexp.MustCompile(`-X[ =]([A-Za-z0-9_./~+-]+\.[A-Za-z_][A-Za-z0-9_]*)=`)

// TestNoFileInThisRepositoryStampsASymbolThisPackageDoesNotExport is the half
// the test above still could not reach. That one asks whether the right stamp
// is present; this one asks whether a wrong one is, anywhere.
//
// The difference matters because "present" is not "used". A Makefile can hold
// the correct -X in one variable and build through a second, misspelled one; a
// Dockerfile can grow a stage for another binary with its own copy of the path;
// a workflow can stop calling make and inline a go build of its own. Every one
// of those keeps the earlier test green, and every one of them ships an
// artifact that says "dev".
//
// So: walk the whole tree, find every stamp written out as text, and require
// each one that names a symbol inside this module to be Path exactly. Stamps
// naming other modules are left alone — scripts/check-stamp-symbol.sh's own
// usage line is one, and it is documentation of a flag, not a build of this
// program. The module path comes from go.mod rather than from Path, so this is
// not Path compared against itself either.
func TestNoFileInThisRepositoryStampsASymbolThisPackageDoesNotExport(t *testing.T) {
	root := moduleRoot(t)
	module := moduleName(t, root)

	found := 0
	walked := 0
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Skip every dot-directory but .github: .git is history, and
			// .claude holds whole copies of this tree that are not this tree.
			base := filepath.Base(name)
			if strings.HasPrefix(base, ".") && base != "." && base != ".github" {
				return fs.SkipDir
			}
			// Build output, not source. Binaries here contain the stamped
			// string by design and are not text anyway.
			if base == "bin" || base == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 4<<20 {
			return nil
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if !utf8.Valid(raw) {
			return nil
		}
		walked++

		for _, match := range aStamp.FindAllStringSubmatch(string(raw), -1) {
			symbol := match[1]
			if !strings.HasPrefix(symbol, module+"/") && symbol != module {
				continue
			}
			found++
			if symbol != Path {
				shown, relErr := filepath.Rel(root, name)
				if relErr != nil {
					shown = name
				}
				t.Errorf("%s stamps %s, but this package exports %s — the linker accepts that flag, does nothing with it and exits 0, so a binary built through %s says %q", shown, symbol, Path, shown, Version)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The control that keeps this from passing because it read nothing. There
	// are hand-written stamps in this repository — the Makefile's and the
	// Dockerfile's at least — and a walk that found none found the wrong tree.
	if walked == 0 {
		t.Fatalf("read no files under %s, so nothing above was checked", root)
	}
	if found < 2 {
		t.Errorf("found %d stamp(s) naming %s in %d files; the Makefile and the Dockerfile each hold one, so this walk is not reading what it thinks it is", found, module, walked)
	}
}

// moduleName reads the module path out of go.mod. It is read rather than
// written out here so that this file holds one fewer copy of a string that
// somebody else owns.
func moduleName(t *testing.T, root string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod names no module")
	return ""
}

// TestEveryBuildPathChecksItsStampBeforeItUsesIt is the guard on the guard.
//
// scripts/check-stamp-symbol.sh is what makes a misspelled stamp fail the
// build rather than the release: it resolves the -X argument through the Go
// toolchain before anything is linked with it. That only helps where it is
// actually called, and the places that can produce a release artifact are the
// Makefile, the Dockerfile and the release workflow. Dropping the call from
// any of them is a one-line edit with a green suite — unless this is here.
//
// The Makefile is checked at the prerequisite, not at the recipe, because the
// recipe alone would not say which targets reach it.
func TestEveryBuildPathChecksItsStampBeforeItUsesIt(t *testing.T) {
	const script = "check-stamp-symbol.sh"
	root := moduleRoot(t)

	required := map[string][]string{
		"Makefile": {
			script,
			// The three targets that build something with STAMP. A target
			// that builds without going through check-stamp is a target that
			// can stamp nothing in silence.
			"build: check-stamp",
			"dist: check-stamp",
			"dist-cross: check-stamp",
		},
		"Dockerfile": {script},
		filepath.Join(".github", "workflows", "release.yml"): {"make check-stamp"},
	}

	for name, wants := range required {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s does not contain %q, so a stamp aimed at a symbol that does not exist would build there in silence", name, want)
			}
		}
	}

	// And the script it all leans on has to be there and be runnable.
	info, err := os.Stat(filepath.Join(root, "scripts", script))
	if err != nil {
		t.Fatalf("scripts/%s: %v", script, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("scripts/%s is mode %v, which docker's build stage cannot execute", script, info.Mode().Perm())
	}
}

// TestEveryLinkOptionInTheBuildFilesCarriesTheStamp closes the last way the
// checks above can all be green over a binary that says "dev": the stamp is
// spelled correctly, and checked, and then simply not passed.
//
// Holding the right string is not the same as using it. Both files here name
// their stamp once — $(STAMP) in make, $STAMP in docker — so every line that
// hands -ldflags to the linker has to carry it, and a line that links a binary
// without it is a line that ships an unidentifiable one.
//
// Comment lines are skipped: both files talk about -ldflags at length, and
// prose about the flag is not a use of it.
func TestEveryLinkOptionInTheBuildFilesCarriesTheStamp(t *testing.T) {
	root := moduleRoot(t)

	for _, file := range []struct{ name, reference string }{
		{"Makefile", "$(STAMP)"},
		{"Dockerfile", "$STAMP"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, file.name))
		if err != nil {
			t.Fatalf("%s: %v", file.name, err)
		}

		linking := 0
		for number, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if !strings.Contains(line, "-ldflags") {
				continue
			}
			linking++
			if !strings.Contains(line, file.reference) {
				t.Errorf("%s:%d links without %s:\n\t%s", file.name, number+1, file.reference, strings.TrimSpace(line))
			}
		}

		// The control: a file whose link lines this test could not find is a
		// file this test is not checking.
		if linking == 0 {
			t.Errorf("found no -ldflags line in %s, so nothing above was checked", file.name)
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
