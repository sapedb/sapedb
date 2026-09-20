// Package build is the one thing a binary knows about itself that its source
// cannot: which build it is.
//
// The wire already carries a version — protocol.Version — and it answers a
// different question. That one says what a frame is written in, and it is a
// constant that changes when the layout changes, which is to say almost
// never. Two builds half a year apart introduced themselves identically,
// because by that measure they are identical. So a report of "the server did
// X" could not be tied to the code that did it, and an operator holding a
// binary had no way to ask it what it was.
//
// Version is therefore a variable, not a constant: it is written by the
// linker at build time from the tag being built, and nothing in the program
// ever assigns it. The default below is what an unstamped build says, and it
// is deliberately a word no release will ever be called, so an unstamped
// binary is recognisable as such rather than claiming a number.
package build

// Version is the product version this binary was built as.
//
// Stamped by the linker; see Path. Read it, do not set it — a build that
// decides its own version at run time is a build that cannot be identified
// from the outside, which is the whole problem this package exists for.
var Version = "dev"

// Path is where the linker writes Version: the argument to go build's
// -ldflags -X, minus the value.
//
// It exists as a constant so that the tests which actually stamp a binary do
// not spell this path by hand. Every other place that stamps one — the
// Makefile and the Dockerfile — must spell it, because make and docker cannot
// read a Go constant; a rename of this package that misses them does not
// break the build, it silently stops stamping, and the binary quietly goes
// back to saying "dev". TestAStampedBuildSaysWhatItWasStampedWith is what
// notices, because it builds through the same flag and refuses the default.
const Path = "github.com/sapedb/sapedb/internal/build.Version"
