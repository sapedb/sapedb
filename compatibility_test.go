package sapedb

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/protocol"
)

// COMPATIBILITY.md is a promise on a public surface, and the two tests here are
// what stop it becoming a promise nobody kept.
//
// A policy document is a claim about the code, which means it ages at exactly
// the speed the code changes and gives no sign when it has. This repository has
// already paid for that once: ISS-15 records a release plan that went stale the
// same day it was written, in the section it called most important.

// TestTheFrameTableIsExactlyThis pins every frame number as a literal.
//
// Deliberately not derived from protocol's own constants, and not from
// fixtures/frames.json either: a table generated from the thing under test can
// only ever agree with itself, and the fixture is a copy of the same decision
// rather than an independent one. These numbers are written out by hand so that
// moving one means editing two places on purpose.
//
// What this catches, and what COMPATIBILITY.md promises it catches: renumbering
// a frame, or giving an existing number a new meaning. What it deliberately
// allows: adding a type numbered 15 or above, which needs no edit here — the
// check below is that every number this table names still holds, not that the
// set is closed.
func TestTheFrameTableIsExactlyThis(t *testing.T) {
	frozen := map[protocol.Type]string{
		1: "Hello", 2: "Welcome", 3: "Ping", 4: "Pong",
		5: "Invoke", 6: "Result", 7: "Failure",
		8: "Subscribe", 9: "Event", 10: "Goodbye",
		11: "Elevate", 12: "Explore", 13: "Declare", 14: "Establish",
	}

	live := map[protocol.Type]string{
		protocol.Hello: "Hello", protocol.Welcome: "Welcome",
		protocol.Ping: "Ping", protocol.Pong: "Pong",
		protocol.Invoke: "Invoke", protocol.Result: "Result",
		protocol.Failure: "Failure", protocol.Subscribe: "Subscribe",
		protocol.Event: "Event", protocol.Goodbye: "Goodbye",
		protocol.Elevate: "Elevate", protocol.Explore: "Explore",
		protocol.Declare: "Declare", protocol.Establish: "Establish",
	}

	// The two maps are built from different sides — literals above, constants
	// below — so a constant that moved lands on a different key and the name
	// under the old number goes missing. Comparing counts first would hide
	// exactly that: two constants swapping numbers keeps the count at fourteen.
	if len(live) != len(frozen) {
		t.Fatalf("the live table holds %d numbers and the frozen one %d: two constants share a number, which is the collision this test exists to find", len(live), len(frozen))
	}
	for number, name := range frozen {
		got, found := live[number]
		if !found {
			t.Errorf("frame %d is %s in COMPATIBILITY.md and nothing holds that number now", number, name)
			continue
		}
		if got != name {
			t.Errorf("frame %d is now %s, and 1.0.0 froze it as %s — renumbering a frame is a 2.0 change", number, got, name)
		}
	}

	// Frame 14 is spent. The next one starts at 15, and that is the sentence
	// COMPATIBILITY.md carries, so it is measured rather than trusted.
	if protocol.Establish != 14 {
		t.Errorf("Establish is %d, want 14 — the document tells the next author to start at 15", protocol.Establish)
	}
}

// TestEveryPathCompatibilityCitesResolves opens every file COMPATIBILITY.md
// points at.
//
// A citation that has quietly stopped resolving is worse than no citation: it
// reads as evidence and is not. Symbols are cited rather than line numbers for
// the same reason, since a line number is a citation that rots on contact.
func TestEveryPathCompatibilityCitesResolves(t *testing.T) {
	document, err := os.ReadFile("COMPATIBILITY.md")
	if err != nil {
		t.Fatalf("reading COMPATIBILITY.md: %v", err)
	}

	// Anything in backticks that looks like a path into this repository: it
	// has a slash and a known extension. Prose in backticks (a symbol, a
	// constant) has neither.
	paths := regexp.MustCompile("`([A-Za-z0-9_./-]+/[A-Za-z0-9_.-]+\\.(?:go|json|md))`").FindAllStringSubmatch(string(document), -1)

	// A run that matched nothing is a run that proved nothing, and it would
	// otherwise pass. The document cites at least this many; the number is a
	// floor, not a count to maintain.
	if len(paths) < 5 {
		t.Fatalf("found %d cited paths in COMPATIBILITY.md, which is fewer than it has: the pattern above has stopped matching, so this test is checking nothing", len(paths))
	}

	seen := map[string]bool{}
	for _, match := range paths {
		path := match[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err != nil {
			t.Errorf("COMPATIBILITY.md cites %s and it is not there: %v", path, err)
		}
	}

	// The document names the tests that enforce each surface. Those names are
	// the enforcement, so a rename that leaves the document behind turns the
	// document into a lie about its own guarantees.
	for _, name := range []string{
		"TestTheFormatTagIsExactlyThis",
		"TestTheFormatNumberIsExactlyThis",
		"TestTheFrameTableIsExactlyThis",
		"TestTheSurfaceIsExactlyTheseFortyNames",
	} {
		if !strings.Contains(string(document), name) {
			t.Errorf("COMPATIBILITY.md does not name %s, which is one of the tests it relies on", name)
		}
	}
}
