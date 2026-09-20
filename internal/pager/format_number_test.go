package pager

import "testing"

// TestTheFormatNumberIsExactlyThis pins the layout version as a literal.
//
// Its sibling TestTheFormatTagIsExactlyThis pins Magic for the same reason, and
// the measurement behind this one is the same shape: before it was written,
// changing Format from 1 to 2 left the whole suite green. Every place that
// reads it compares a file against the same constant that wrote it — including
// format_test.go, whose broken value is built as Format+1 — so the two agree
// no matter what it says.
//
// That is the trap this test exists for. Magic decides whether a file is ours;
// Format decides whether this build may read it, and it is the only escape
// hatch the on-disk format has. A change here is never a refactor: either a
// migration is being shipped deliberately and this literal moves with it, or
// every database written by an earlier build has just become unreadable by a
// refusal nobody meant to trigger.
//
// COMPATIBILITY.md names this test as what keeps that promise.
func TestTheFormatNumberIsExactlyThis(t *testing.T) {
	const want = 1
	if Format != want {
		t.Errorf("Format is %d, want %d — 1.x opens what 1.x wrote, so moving this is a 2.0 change and needs a migration", Format, want)
	}
}
