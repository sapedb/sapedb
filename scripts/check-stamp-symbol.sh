#!/bin/sh
# check-stamp-symbol.sh refuses a linker stamp that would silently do nothing.
#
# The product version is written into a binary by the linker:
#
#	go build -ldflags "-X github.com/sapedb/sapedb/internal/build.Version=1.0.0"
#
# That flag is silent in two directions, and both of them end with a released
# binary claiming to be an unstamped development build:
#
#   - a symbol that does not exist is accepted and ignored. No warning, no
#     error, exit 0. So renaming the package or the variable does not break
#     the build; it stops the stamping, and nothing says so (ISS-18).
#   - an empty value stamps nothing, also silently. That is why the Makefile's
#     VERSION falls back to `git describe --always` rather than to nothing.
#
# The symbol is named in Go by internal/build.Path, and make and docker cannot
# read a Go constant, so each of them spells it out by hand. This script is
# what turns a wrong spelling into a failed build instead of a quiet one: it
# is handed the very -X argument the build is about to use, and it resolves
# that argument through the Go toolchain before anything is built with it.
#
# Usage:
#	check-stamp-symbol.sh "-X some/package.Variable=value"
#
# The Go toolchain used is $GO when set — the Makefile passes its own, which
# may be a container — and `go` otherwise.
#
# Exit status: 0 when the stamp will land, 1 when it would not, 2 when this
# script was called wrongly.
set -eu

me="check-stamp-symbol.sh"

argument="${1:-}"
if [ -z "$argument" ]; then
	echo "$me: no -X argument given" >&2
	echo "$me: usage: $me \"-X some/package.Variable=value\"" >&2
	exit 2
fi

# Accept the two spellings go build itself accepts, so this script checks the
# string the build uses rather than a tidied-up copy of it.
case "$argument" in
-X\ *) spec=${argument#-X } ;;
-X=*) spec=${argument#-X=} ;;
*)
	echo "$me: $argument is not an -X stamp, so there is nothing here to check" >&2
	exit 2
	;;
esac

case "$spec" in
*=*) ;;
*)
	echo "$me: $argument names no value: -X without an = stamps nothing" >&2
	exit 1
	;;
esac

symbol=${spec%%=*}
value=${spec#*=}

if [ -z "$value" ]; then
	echo "$me: the stamp $argument has an empty value" >&2
	echo "$me: an empty -X value is accepted by the linker and stamps nothing, so the binary would still say the default" >&2
	exit 1
fi

if [ -z "$symbol" ]; then
	echo "$me: the stamp $argument names no symbol" >&2
	exit 1
fi

# The last dot separates the package path from the variable: the path itself
# is full of dots, the variable name cannot contain one.
variable=${symbol##*.}
package=${symbol%.*}
if [ -z "$variable" ] || [ "$package" = "$symbol" ]; then
	echo "$me: $symbol is not a package path and a variable name separated by a dot" >&2
	exit 1
fi

go_tool=${GO:-go}

# go doc resolves the two halves separately and says which one is wrong, which
# is the whole difference between this and the linker: it fails, and it names
# what it could not find.
if ! found=$($go_tool doc "$package" "$variable" 2>&1); then
	echo "$me: the stamp $argument aims at $symbol, which does not exist:" >&2
	printf '%s\n' "$found" >&2
	echo "$me: go build would accept that flag, do nothing with it, and exit 0 — see internal/build.Path and ISS-18" >&2
	exit 1
fi

# -X writes a string variable. A constant or a function of the same name would
# resolve above and still not be stampable, so say what was actually found.
if ! printf '%s\n' "$found" | grep -qE "^var ${variable}([^A-Za-z0-9_]|\$)"; then
	echo "$me: $symbol exists but is not a variable, so -X cannot write to it:" >&2
	printf '%s\n' "$found" >&2
	exit 1
fi
