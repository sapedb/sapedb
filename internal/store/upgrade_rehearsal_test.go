package store

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	buildinfo "github.com/sapedb/sapedb/internal/build"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// SAPE-6: the upgrade rehearsal.
//
// Nobody has ever upgraded a sapedb — there are no tags, so there has never
// been a build that was not the only build. The two tests in this file are
// the rehearsal for the day that stops being true: one build writes a real
// database file, a different build is asked to make sense of it, and the
// two directions that can go are both exercised with real, separately
// compiled binaries rather than assumed from reading the code.
//
// Without a tag, "two builds" has to be produced another way, and which way
// matters:
//
//   - TestANewerBuildReadsEveryFieldAnOlderBuildWrote builds the CLI from an
//     actual earlier commit (git archive, not a copy with a flag flipped) and
//     from this source tree, and proves the newer side reads back exactly what
//     the older side wrote — field by field, not by counting. This is the
//     positive half, and it is real: the two builds differ in real code, even
//     though the on-disk format has not moved between them.
//
//   - TestABuildThatCannotReadAFutureFormatRefusesItAndLeavesTheFileAlone is
//     the negative half. Because the format has genuinely never moved, there
//     is no real commit anywhere in this repository whose reader would refuse
//     today's writer, so the only way to produce that refusal for real is to
//     build a throwaway copy of this source with pager.Format bumped by one
//     and let it write a file. TestTheFormatNumberIsExactlyThis fails inside
//     that throwaway copy — correctly, which this test checks rather than
//     assumes — and that failure is exactly why the copy is never the one
//     built into the suite. This is also the "downgrade" shape SAPE-6 asks
//     for: a file written by a build ahead of this one, opened by this one.
//     There being only one format in this product's history, "downgrade" and
//     "a build that cannot read a file" are the same test, not two.
//
// A two-way -ldflags stamp (same source, different version string) was
// considered and rejected as the sole method: it would satisfy "two builds,
// identifiable by `sapedb version`" without exercising a single line of
// different behaviour, which is not a rehearsal, it is a label. Both tests
// below still check the version stamp, because AC1 asks for it, but as a
// belt on top of a real difference, never as the difference itself.

// rehearsalOlderCommit is a real commit this repository already has, chosen
// for two reasons: it is after 6cb845a, the rename that made Magic spell
// "SAPEDB" (an older-still commit would be refused as ErrNotSapedb, which is
// a different rehearsal from the one SAPE-6 asks for), and it is five commits
// behind HEAD at the time this test was written — genuinely different code
// (ISS-11's grant expiry, the ErrFormat tests themselves, SAPE-2's frozen
// surfaces, a dbname fix), not a relabeled copy of the same tree.
const rehearsalOlderCommit = "631454adbc0b9899b1dec513d7ab597041f06b27"

// rehearsalModuleRoot finds the repository root the same way
// internal/build's own test does: from this file's own path, so the test
// does not depend on the directory `go test` happened to be run from.
func rehearsalModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this file's own path")
	}
	// internal/store/upgrade_rehearsal_test.go -> the root is two levels up.
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// rehearsalExtractCommit checks a real commit out into dest, via git archive
// piped into tar, without touching this worktree's own working tree. dest
// must already exist.
func rehearsalExtractCommit(t *testing.T, root, commit, dest string) {
	t.Helper()

	// Ask whether the commit is here BEFORE trying to read a tree out of it.
	// git archive's own answer to a missing commit is "fatal: not a tree
	// object", which reads like damage rather than absence, and absence is by
	// far the likelier cause: a shallow clone (what actions/checkout does
	// unless told otherwise) contains one commit, and this one is twenty
	// back. Distinguishing the two matters because the rest of this file
	// exists to report that an upgrade broke, and "the history is not here"
	// is not that.
	if err := exec.Command("git", "-C", root, "cat-file", "-e", commit+"^{commit}").Run(); err != nil {
		t.Fatalf("commit %s is not in this clone, so the upgrade rehearsal cannot run: %v\n"+
			"This is almost certainly a shallow clone rather than a broken upgrade. "+
			"A checkout needs full history (actions/checkout with fetch-depth: 0); "+
			"`git rev-parse --is-shallow-repository` will say which it is.", commit, err)
	}

	archive := exec.Command("git", "-C", root, "archive", commit)
	tar := exec.Command("tar", "-x", "-C", dest)

	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("git archive %s: %v", commit, err)
	}
	tar.Stdin = pipe

	var archiveErr, tarErr bytes.Buffer
	archive.Stderr = &archiveErr
	tar.Stderr = &tarErr

	if err := tar.Start(); err != nil {
		t.Fatalf("tar -x: %v: %s", err, tarErr.String())
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("git archive %s: %v: %s", commit, err, archiveErr.String())
	}
	if err := tar.Wait(); err != nil {
		t.Fatalf("tar -x: %v: %s", err, tarErr.String())
	}
}

// rehearsalBuild builds cmd/sapedb from srcDir, stamped with versionStamp
// through the same linker flag the Makefile uses (build.Path, not a copy of
// its string, so a rename of internal/build.Version breaks this test the
// same way it breaks the real build rather than silently stamping nothing).
func rehearsalBuild(t *testing.T, srcDir, versionStamp, outPath string) {
	t.Helper()
	cmd := exec.Command("go", "build",
		"-ldflags", "-X "+buildinfo.Path+"="+versionStamp,
		"-o", outPath, "./cmd/sapedb")
	cmd.Dir = srcDir
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build in %s: %v\n%s", srcDir, err, stderr.String())
	}
}

// rehearsalRun runs a built sapedb binary with a deliberately narrow
// environment — only the four SAPEDB_* variables a command needs, plus PATH
// — so this rehearsal proves what it claims: an operator swapping a binary,
// not a binary that happens to inherit a working developer's shell.
func rehearsalRun(t *testing.T, bin, dir string, stdin []byte, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SAPEDB_DIR=" + dir,
		"SAPEDB_ACCOUNT=acct",
		"SAPEDB_DB=rehearsal",
		"SAPEDB_SECRET=rehearsal-secret-not-used-anywhere-real",
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return outBuf.String(), errBuf.String(), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("running %s %v: %v", bin, args, err)
	}
	return outBuf.String(), errBuf.String(), exitErr.ExitCode()
}

// rehearsalDataPath is where rehearsalRun's fixed SAPEDB_ACCOUNT/SAPEDB_DB
// land inside a SAPEDB_DIR, matching cli.dbPath.
func rehearsalDataPath(dir string) string {
	return filepath.Join(dir, "acct", "rehearsal.sapedb")
}

// rehearsalDataset builds a real store in memory with two collections and
// documents in varied optional-field states — some carry a field, some
// don't, one carries an extra field none of the others do — because the
// failure this rehearsal exists to catch is a document that survives with a
// field silently missing, and a dataset with every field present on every
// document could not tell that apart from a field being dropped by
// coincidence. It returns the dataset dump bytes and the *Store it came
// from, still open, for sameStores to compare against later.
func rehearsalDataset(t *testing.T) (*Store, []byte) {
	t.Helper()

	_, store := fresh(t, 7001)

	articleCollection, err := store.Declare(Caller{}, articles())
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []map[string]any{
		{"author": "bob", "title": "Article Zero", "slug": "s0", "tags": []any{"go", "db"}, "published": float64(0)},
		{"author": "ann", "title": "Article One", "published": float64(1)},
		{"author": "ann", "title": "Article Two", "slug": "s2", "published": float64(2)},
		{"author": "bob", "title": "Article Three", "tags": []any{"db"}, "published": float64(3)},
		{"author": "ann", "title": "Article Four"},
	} {
		put(t, articleCollection, document)
	}

	accounts, err := store.Declare(Caller{}, Spec{
		Name: "accounts",
		Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
		Indexes: []Index{
			{Name: "by_handle", Unique: true, Fields: []Field{{Path: "handle", Type: TypeString, Missing: MissingSkip}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []map[string]any{
		{"handle": "ann", "bio": "Writes about go and databases."},
		{"handle": "bob"},
		{"handle": "cleo", "bio": "", "muted": true},
	} {
		put(t, accounts, document)
	}

	declareOp(t, store, byAuthor())

	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if _, err := store.Dump(out); err != nil {
		t.Fatal(err)
	}
	return store, out.Bytes()
}

// TestANewerBuildReadsEveryFieldAnOlderBuildWrote is SAPE-6's positive half.
//
// A real earlier build (git archive of rehearsalOlderCommit, actually
// compiled) restores a dataset into a real file on disk. This build's own
// code — which is what a `sapedb` compiled from this tree does, the same
// store.Open the CLI's open() calls — then opens that exact file and reads
// it back. sameStores (internal/store/log_test.go) is reused unmodified for
// the comparison: it walks every collection's declaration, every document
// and every index, so a document that came back with a field missing is
// exactly what it is built to catch, not something a count could hide.
func TestANewerBuildReadsEveryFieldAnOlderBuildWrote(t *testing.T) {
	root := rehearsalModuleRoot(t)

	olderSrc := t.TempDir()
	rehearsalExtractCommit(t, root, rehearsalOlderCommit, olderSrc)

	olderShort := strings.TrimSpace(runGitRehearsal(t, root, "rev-parse", "--short", rehearsalOlderCommit))
	newerShort := strings.TrimSpace(runGitRehearsal(t, root, "rev-parse", "--short", "HEAD"))
	if olderShort == newerShort {
		t.Fatalf("the older commit and HEAD are the same commit (%s) — this test needs two different builds, not one built twice", olderShort)
	}

	olderBin := filepath.Join(t.TempDir(), "sapedb-older")
	rehearsalBuild(t, olderSrc, "rehearsal-older-"+olderShort, olderBin)

	newerBin := filepath.Join(t.TempDir(), "sapedb-newer")
	rehearsalBuild(t, root, "rehearsal-newer-"+newerShort, newerBin)

	// AC1: two distinguishable builds, identifiable via `sapedb version` —
	// checked against the commits actually built, not against the labels
	// this test chose, so a build that silently stamped "dev" (see
	// internal/build's own tests for why -X can fail silently) is caught
	// here rather than assumed away.
	olderVersion, _, code := rehearsalRun(t, olderBin, t.TempDir(), nil, "version")
	if code != 0 || !strings.Contains(olderVersion, olderShort) {
		t.Fatalf("the older build's own `version` does not name %s: %q (exit %d)", olderShort, olderVersion, code)
	}
	newerVersion, _, code := rehearsalRun(t, newerBin, t.TempDir(), nil, "version")
	if code != 0 || !strings.Contains(newerVersion, newerShort) {
		t.Fatalf("the newer build's own `version` does not name %s: %q (exit %d)", newerShort, newerVersion, code)
	}
	if olderVersion == newerVersion {
		t.Fatalf("both builds report the same version %q — they are not distinguishable", olderVersion)
	}

	reference, dump := rehearsalDataset(t)

	dataDir := t.TempDir()
	restoreOut, restoreErr, code := rehearsalRun(t, olderBin, dataDir, dump, "restore")
	if code != 0 {
		t.Fatalf("the older build's restore failed: exit %d, stdout %q, stderr %q", code, restoreOut, restoreErr)
	}
	if !strings.Contains(restoreOut, "restored to change") {
		t.Fatalf("the older build's restore did not report success: %q", restoreOut)
	}

	// A real binary, from this same source, can also read the file the older
	// build wrote — checked before the in-process comparison below, because
	// this is the tool an operator actually runs, and a bug wired into
	// cli.go's open() but not into store.Open would otherwise go unnoticed.
	lsOut, lsErr, code := rehearsalRun(t, newerBin, dataDir, nil, "ls")
	if code != 0 {
		t.Fatalf("the newer build's `ls` on the older build's file failed: exit %d, stderr %q", code, lsErr)
	}
	if !strings.Contains(lsOut, "collection articles") || !strings.Contains(lsOut, "collection accounts") {
		t.Fatalf("the newer build's `ls` does not see both collections: %q", lsOut)
	}

	// Now open the same file this build's own code, in this process — the
	// path sameStores below actually exercises.
	path := rehearsalDataPath(dataDir)
	file, err := vfs.OpenFile(path, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()
	pages, err := pager.Open(file, 0)
	if err != nil {
		t.Fatalf("pager.Open on a file the older build wrote: %v", err)
	}
	opened, err := Open(pages)
	if err != nil {
		t.Fatalf("store.Open on a file the older build wrote: %v", err)
	}

	// The deep comparison: every collection's declaration, every document
	// field by field, and every index, reusing sameStores exactly as
	// internal/store's own dump/replica tests do.
	sameStores(t, reference, opened)

	// Bonus: the operation the older build's restore wrote came back too,
	// and still runs — dump/restore round-trips more than documents.
	result, err := opened.Invoke(Caller{}, "articles.by_author", 0, map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("the operation the older build wrote did not survive: %v", err)
	}
	if result.Count == 0 {
		t.Errorf("articles.by_author returned nothing for ann, want at least one row")
	}
}

// TestABuildThatCannotReadAFutureFormatRefusesItAndLeavesTheFileAlone is
// SAPE-6's negative half, and — there being only one Format this product has
// ever shipped — its downgrade case too: a file written by a build ahead of
// this one must be refused, named by both numbers, and left untouched.
//
// There is no real commit whose Format differs from this one's, so the
// "future build" here is a throwaway: a full copy of HEAD's source with
// pager.Format bumped by exactly one. TestTheFormatNumberIsExactlyThis is
// run inside that copy before anything else, and this test insists it fails
// — a silent no-op patch (the sed found nothing, or found two things) would
// otherwise make the rest of this test pass for the wrong reason, which is
// exactly the empty-result trap the rest of this task was written against.
func TestABuildThatCannotReadAFutureFormatRefusesItAndLeavesTheFileAlone(t *testing.T) {
	root := rehearsalModuleRoot(t)

	futureSrc := t.TempDir()
	rehearsalExtractCommit(t, root, "HEAD", futureSrc)

	badFormat := pager.Format + 1
	pagerGoPath := filepath.Join(futureSrc, "internal", "pager", "pager.go")
	original, err := os.ReadFile(pagerGoPath)
	if err != nil {
		t.Fatal(err)
	}
	from := fmt.Sprintf("const Format uint16 = %d", pager.Format)
	to := fmt.Sprintf("const Format uint16 = %d", badFormat)
	if count := strings.Count(string(original), from); count != 1 {
		t.Fatalf("expected exactly one %q in pager.go, found %d — the patch below would silently do the wrong thing", from, count)
	}
	patched := strings.Replace(string(original), from, to, 1)
	if err := os.WriteFile(pagerGoPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	// The guard has to actually fire in the copy it was made to fire in.
	// go test's own exit code is the check: 0 would mean the patch above did
	// nothing, which is the one way this whole test could pass for a reason
	// that has nothing to do with ErrFormat.
	testCmd := exec.Command("go", "test", "./internal/pager/", "-run", "TestTheFormatNumberIsExactlyThis", "-count=1", "-v")
	testCmd.Dir = futureSrc
	testCmd.Env = os.Environ()
	redOutput, runErr := testCmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("TestTheFormatNumberIsExactlyThis passed inside the patched copy, so the patch did not take: %s", redOutput)
	}
	if !strings.Contains(string(redOutput), fmt.Sprintf("Format is %d, want %d", badFormat, pager.Format)) {
		t.Fatalf("the guard failed for a reason other than the Format bump: %s", redOutput)
	}
	t.Logf("confirmed the guard actually fires in the throwaway copy:\n%s", redOutput)

	futureBin := filepath.Join(t.TempDir(), "sapedb-future")
	rehearsalBuild(t, futureSrc, fmt.Sprintf("rehearsal-future-format-%d", badFormat), futureBin)

	currentBin := filepath.Join(t.TempDir(), "sapedb-current")
	rehearsalBuild(t, root, "rehearsal-current-reader", currentBin)

	_, dump := rehearsalDataset(t)

	dataDir := t.TempDir()
	restoreOut, restoreErr, code := rehearsalRun(t, futureBin, dataDir, dump, "restore")
	if code != 0 {
		t.Fatalf("the future build's restore failed: exit %d, stdout %q, stderr %q", code, restoreOut, restoreErr)
	}

	path := rehearsalDataPath(dataDir)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file the future build wrote: %v", err)
	}

	lsOut, lsErr, code := rehearsalRun(t, currentBin, dataDir, nil, "ls")
	if code == 0 {
		t.Fatalf("this build's `ls` opened a format-%d file without complaint: %q", badFormat, lsOut)
	}

	want := fmt.Sprintf("sapedb/pager: file format is from another version: file says %d, this build reads %d", badFormat, pager.Format)
	if strings.TrimSpace(lsErr) != want {
		t.Fatalf("refusal message:\n got  %q\n want %q", strings.TrimSpace(lsErr), want)
	}
	if lsOut != "" {
		t.Errorf("a refused `ls` printed something anyway: %q", lsOut)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file after the refused open: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a refused open changed the file on disk (%d bytes before, %d after)", len(before), len(after))
	}
}

// runGitRehearsal is a small git wrapper for the two rev-parse calls above;
// separate from rehearsalExtractCommit because it returns output rather than
// extracting a tree.
func runGitRehearsal(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
