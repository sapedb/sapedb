package store

import (
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
)

// namedSelfCycle is built on top of []any the way describe.go's own doc
// comment describes a caller doing it — "a named type built on top of
// either shape, such as `type Rows []any`" — specifically so that fits()'s
// exact type switch (case []any:) does NOT match it: a type switch tests
// the dynamic type, not the underlying one, so a value of this type used to
// fall through to fits' default branch, be treated as an always-safe leaf,
// and reach a bare fmt.Sprintf with no cycle protection at all.
type namedSelfCycle []any

func buildNamedSelfCycle() namedSelfCycle {
	v := namedSelfCycle{nil}
	v[0] = v
	return v
}

// TestDescribeOfANamedSelfReferentialSliceTypeReturnsABoundedStringInstead
// OfCrashing is cycle_test.go's argument-door test
// (TestArgDoorCyclicValueNoLongerCrashesThroughSatisfiesItReturnsErrCondition),
// with one thing different: the cyclic value handed to Invoke through the
// exported {"equals": {"arg": ...}} door is a namedSelfCycle, not a bare
// []any. Before fits/renderCapped widened past the exact []any /
// map[string]any type switch to a reflect.Kind() check, this exact shape
// reached fmt.Sprintf("%v", v) unguarded and crashed the process with
// `fatal error: stack overflow` — reproduced in a child process here for
// the same reason cycle_test.go isolates its own crash cases: a real crash
// of this kind takes the whole `go test` binary down with it, not just one
// red test.
func TestDescribeOfANamedSelfReferentialSliceTypeReturnsABoundedStringInsteadOfCrashing(t *testing.T) {
	if os.Getenv("SAPEDB_NAMEDCYCLE_CHILD") == "1" {
		debug.SetMaxStack(16 << 20)
		defer func() {
			if r := recover(); r != nil {
				os.Stdout.WriteString("RECOVERED\n")
				os.Exit(3)
			}
		}()

		_, s := fresh(t, 832)
		widgets, err := s.Declare(Spec{Name: "widgets", Key: Key{Path: "id", Type: TypeString}})
		if err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		yes := true
		if _, err := s.DeclareOperation(Caller{}, Operation{
			Name: "widgets.check", Collection: "widgets", Action: ActionBatch,
			Input: []Parameter{
				{Name: "id", Type: TypeString, Required: true},
				{Name: "t", Type: TypeAny, Required: true},
			},
			Steps: []Step{{
				Action: ActionUpdate, Collection: "widgets", Key: &Term{Arg: "id"}, Exists: &yes,
				Require: []Condition{{Path: "tags", Equals: &Term{Arg: "t"}}},
				Set:     map[string]Term{"ok": {Value: true}},
			}},
		}); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if err := s.Commit(); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if _, err := widgets.Put(map[string]any{"id": "w1", "tags": []any{"a", "b"}}); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}
		if err := s.Commit(); err != nil {
			os.Stdout.WriteString("SETUP FAILED: " + err.Error() + "\n")
			os.Exit(5)
		}

		_, invokeErr := s.Invoke(Caller{}, "widgets.check", 0, map[string]any{
			"id": "w1", "t": buildNamedSelfCycle(),
		})
		if invokeErr == nil {
			os.Stdout.WriteString("RETURNED NIL ERROR\n")
			os.Exit(4)
		}
		os.Stdout.WriteString("RETURNED: " + invokeErr.Error() + "\n")
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestDescribeOfANamedSelfReferentialSliceTypeReturnsABoundedStringInsteadOfCrashing$",
		"-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SAPEDB_NAMEDCYCLE_CHILD=1")
	out, err := cmd.CombinedOutput()
	text := string(out)
	t.Logf("child exit err=%v\n%s", err, text)
	if strings.Contains(text, "fatal error:") {
		t.Fatalf("child crashed with a fatal error — describe should have returned a bounded string instead:\n%s", text)
	}
	if strings.Contains(text, "RECOVERED") {
		t.Fatalf("child panicked and recovered; it is supposed to return an ordinary error, not panic:\n%s", text)
	}
	if err != nil {
		t.Fatalf("child exited with an error: %v\n%s", err, text)
	}
	if !strings.Contains(text, "RETURNED:") {
		t.Fatalf("child did not report returning an error:\n%s", text)
	}
}
