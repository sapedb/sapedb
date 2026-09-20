// Package naming is the patrol that keeps the old product name from coming
// back. A rename is not "done" because a sed command ran once; it is done
// when nothing in the tree can reintroduce the old name without this package
// noticing. So this walks every file, not a sample of the ones the rename
// touched.
//
// It intentionally does not know the old name as a whole word anywhere in
// its own source — see target below — which is what lets it also patrol
// itself.
package naming

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// errNotText marks a file readFile declined to scan because it is not valid
// UTF-8. Never returned across a package boundary as anything other than
// "skip this file" — Walk does not distinguish it from any other read
// failure, on purpose: either way there is nothing to check.
var errNotText = errors.New("naming: not text")

// target is the word this patrol is looking for, spelled apart. Not for
// disguise: it is so that adding this package to the tree does not itself
// become a hit, and so that nobody can "fix" a red patrol by deleting the
// one place that still names what it is watching for.
var target = string([]byte{'r', 's', 'q', 'l'})

// skipDirs are trees this patrol does not enter. .git is not source; the
// other two only exist in the TypeScript sibling, but staying the same list
// shape in both repos is worth more than the two entries being dead weight
// here.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
}

// readFile returns a file's content as text, or an error if it plainly is
// not text. It exists so Walk has one place to decide that, instead of the
// decision being implicit in whatever os.ReadFile happens to return.
func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", errNotText
	}
	return string(raw), nil
}

// Hit is one line, in one file, that contains the old name.
type Hit struct {
	File string // relative to the root Walk was given
	Line int    // 1-based
	Text string
}

// Walk reads every regular file under root and reports every line that
// contains target, case-insensitively, regardless of how deep it is nested.
//
// A directory is not a signal either way; only file content is. A file this
// patrol cannot decode as text (there are none in either repo today) is
// skipped rather than failing the walk, because a patrol that cannot finish
// is a patrol nobody can trust the silence of.
//
// What Walk does NOT bound, and why naming_test.go cannot tell the
// difference: Walk has no file-size cap, no file-count cap, and no rule
// that skips a file by its name prefix. None of the fixtures in
// naming_test.go plant enough files, or a big enough file, to reach any of
// those, so the test suite cannot distinguish a version of Walk carrying
// such a cap from one without one.
//
// Three mutants measured against this suite and still alive, left
// unaddressed on purpose: round 4 of task 0038 closed most of the "widen
// the edge and it survives" class of mutant; these three are where that
// chase was deliberately stopped (task 0051 keeps that call, it does not
// reopen it):
//
//   - a file-size cutoff at 64KB here, 100KB on the TypeScript sibling
//   - a cap that stops the walk after 85 files
//   - a rule that skips any file whose name starts with "#"
//
// Where this repo's tree stands today, measured with the same exclusions as
// skipDirs above:
//
//	find . \( -name .git -o -name node_modules -o -name dist \) -prune \
//	  -o -type f -print0 | xargs -0 stat -f "%z %N" | sort -rn | head
//	find . \( -name .git -o -name node_modules -o -name dist \) -prune \
//	  -o -type f -print | wc -l
//
// Both these numbers move every time a file is added or grows, and this
// paragraph is only ever as current as the last time somebody re-ran the two
// commands above and edited it — it is not a claim this file checks itself
// (see the sign paragraph below for why that gap is left open, not closed).
// Measured against commit 86c5f0a: the largest file is
// internal/cli/cli_test.go at 90201 bytes — already PAST the 64KB mark, not
// "well under" it — and the total file count is 114, already PAST the
// 85-file mark too. Both premises this paragraph once rested on ("the tree
// isn't big enough to reach it") are wrong for the tree as it stands today:
// if either the file-size cutoff or the stop-after-85-files cap were really
// present in Walk right now, each would already be silently cutting into
// this tree's own scan — the size cap on cli_test.go specifically, the count
// cap on whatever filepath.WalkDir visits past the 85th entry — with nobody
// told either way. No file in this tree starts with "#", so that third edge
// is the one still sitting inside its boundary; the other two have already
// been crossed, and only the absence of those two caps from the actual code
// (confirmed by reading Walk above: no size check, no count, no name-prefix
// rule) is what keeps this quiet rather than already narrowing the patrol.
//
// There is no sign that either of these two crossed edges has been hit,
// and that is not an oversight this paragraph forgot to fill in — an
// earlier version of it here promised one ("Sign that this has been hit:
// ...") and then went on to describe the opposite of a sign: a file this
// patrol used to scan quietly dropping out of what it scans, "with nothing
// telling anyone that happened." A cap that fires does not announce
// itself; it just narrows what Walk looks at, and nothing downstream of
// Walk (naming_test.go included, by its own admission a few paragraphs up)
// is built to notice a file going missing from that set. If one of these
// two caps is ever reintroduced for real, the only way to catch it is the
// same way this paragraph caught the size one above: rerun the two find
// commands and compare against what Walk actually returns for those files
// by name — there is nothing here that does that automatically, on
// purpose, per task 0051's own call not to reopen this chase.
func Walk(root string) ([]Hit, error) {
	var hits []Hit

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		content, err := readFile(path)
		if err != nil {
			// Not text, or not readable. Either way there is nothing this
			// patrol can check, and that is not the same claim as "clean".
			return nil
		}

		lower := strings.ToLower(content)
		if !strings.Contains(lower, target) {
			return nil
		}

		lines := strings.Split(content, "\n")
		lowerLines := strings.Split(lower, "\n")
		for i, lowerLine := range lowerLines {
			if strings.Contains(lowerLine, target) {
				hits = append(hits, Hit{File: rel, Line: i + 1, Text: lines[i]})
			}
		}
		return nil
	})

	return hits, err
}

// Exception is one line this patrol lets through: a file, and a fragment of
// that exact line that does not itself contain the old name, so this table
// can name what it excuses without becoming one more hit.
type Exception struct {
	File   string
	Marker string
	Why    string
}

// Exceptions is every place the old name is allowed to remain, and the whole
// point of writing it down: a new entry shows up in a diff, and a reviewer
// either agrees with Why or does not.
//
// It is empty, and that is the finished state this package was written to
// reach — "a rename is done when nothing in the tree can reintroduce the old
// name without this package noticing", and there is now nothing left to
// excuse. It held four rows until the rename was completed: the on-disk
// format tag and three key-derivation labels, each kept on the argument that
// changing it breaks every database that already exists. That argument was
// spent the moment it was checked against the repository, which carries no
// tags at all and so has never released anything for such a database to have
// been written by.
//
// The table and Allowed stay rather than being deleted with their last row,
// because the next justified exception should arrive as one reviewable line
// here rather than as a reason to rebuild this machinery under time
// pressure. allowedIn below is what keeps that machinery measured while the
// table is empty: the behaviour tests run against a fixture table, so they
// still fail when Allowed is widened, instead of passing vacuously because
// there is nothing to widen.
var Exceptions []Exception

// Allowed reports whether a hit is one this patrol excuses.
func Allowed(hit Hit) bool { return allowedIn(hit, Exceptions) }

// allowedIn is Allowed against a given table rather than the live one. It
// exists so the tests that measure what Allowed excuses — that a marker is
// scoped to its own line and its own file, and that it pins a value rather
// than a keyword — keep measuring it now that Exceptions is empty. Against
// the live table those tests would all pass without exercising a single
// comparison below, which is the one way a patrol goes quiet without anybody
// editing it.
func allowedIn(hit Hit, table []Exception) bool {
	for _, exception := range table {
		if hit.File == exception.File && strings.Contains(hit.Text, exception.Marker) {
			return true
		}
	}
	return false
}
