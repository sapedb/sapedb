#!/usr/bin/env bash
#
# The worked example, followed from cold by a machine that is not the author's.
#
# SAPE-12's third acceptance criterion is that the worked example "is built,
# signed and installed by the commands on the page, exactly as written, on a
# machine that is not the author's". Every command on
# docs/external-operations-page.md was run once, by hand, on one laptop. This
# script is the same sequence with its answers asserted, so that it is run
# again on every push by a runner that starts from nothing.
#
# Three things about the shape of this file, each one deliberate.
#
#   1. THE COMMANDS ARE WRITTEN OUT HERE BY HAND. They are not extracted from
#      the page. A job that reads its script out of the document it is checking
#      cannot fail when that document is wrong — it would run whatever the page
#      said and call the result green. The page is checked against this file
#      separately, by TestTheWorkedExamplePageAndTheCIJobAgree in
#      docs_worked_example_test.go, which holds the command shapes a third time
#      as literals of its own and demands to find each one on both sides.
#
#   2. EVERY EXPECTED ANSWER IS A LITERAL HERE TOO. Nothing below reads an
#      expectation out of module.json, out of the bundle, or out of the
#      catalogue and compares it against itself. "Le Guin wrote two of the three
#      books" is written as the number 2, not counted out of the file that
#      declares the books.
#
#   3. THE REFUSALS ARE ASSERTED, NOT JUST THE SUCCESSES. A path that only ever
#      succeeds proves less than one that also proves the door is shut: an
#      untrusted bundle, a tampered bundle, an argument the declaration does not
#      name, a missing required argument, and an operation that is not there.
#
# It is run by .github/workflows/worked-example.yml, and can be run by hand
# from a clean checkout with:
#
#     env -i PATH="$PATH" HOME="$HOME" \
#       GOPATH="$(go env GOPATH)" \
#       GOMODCACHE="$(go env GOMODCACHE)" \
#       GOCACHE="$(go env GOCACHE)" \
#       bash .github/scripts/worked-example.sh
#
# The empty environment is not decoration. SAPEDB_SECRET, SAPEDB_TRUST,
# SAPEDB_DIR, SAPEDB_ACCOUNT and SAPEDB_DB all change what these commands do,
# and a sequence that passes only because the author's shell already exported
# one of them is exactly the thing this criterion exists to catch. The GO*
# variables are carried across because the toolchain has to keep its caches
# somewhere; none of them is a SAPEDB_ variable and none of them changes what
# any command below does. Carry nothing and `go build` stops at
# "module cache not found", which is a failure of the harness rather than of
# the page.

set -u
set -o pipefail

# -e is deliberately NOT set. Half the commands below are expected to fail, and
# each one's status is captured and checked the moment it finishes. A shell
# that died on the first refusal could not assert that the refusal happened.

# ---------------------------------------------------------------------------
# Where things go.
# ---------------------------------------------------------------------------

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The daemon's address. The page names 127.0.0.1:7455 and so does this, because
# the point of the check is that a reader who types what the page says arrives
# where the page says they will.
host="127.0.0.1:7455"

# The secret this host runs on. Invented here, held only in this script's own
# environment, never inherited: see the header on the empty environment.
secret="the secret only the control plane has"

# ---------------------------------------------------------------------------
# Assertions.
#
# `checks` is a floor, not decoration. A run that compiled nothing and asserted
# nothing also reports zero failures — the same trap .github/workflows/test.yml
# names when it refuses a report with too few PASS lines. The count is compared
# against a hand-written minimum at the end, so a sequence that exited early
# through some path that forgot to fail is red rather than silently green.
# ---------------------------------------------------------------------------

checks=0
failures=0

fail() {
  failures=$((failures + 1))
  echo "::error::$1"
}

# contains FILE LITERAL WHAT — the file must hold that exact text.
contains() {
  checks=$((checks + 1))
  if grep -qF -- "$2" "$1"; then
    echo "  ok      $3"
  else
    fail "$3: expected to find <$2> in the output, and did not"
    echo "----- what was actually there -----"
    cat "$1"
    echo "-----------------------------------"
  fi
}

# absent FILE LITERAL WHAT — the file must NOT hold that text.
#
# Used for the one claim the page makes that only a negative can express: a
# projection keeps `author` out of what library:books.by_author hands back,
# although the documents it reads carry it and the index is built on it.
absent() {
  checks=$((checks + 1))
  if grep -qF -- "$2" "$1"; then
    fail "$3: did not expect to find <$2> in the output, and it is there"
    echo "----- what was actually there -----"
    cat "$1"
    echo "-----------------------------------"
  else
    echo "  ok      $3"
  fi
}

# exactly FILE LITERAL WHAT — the file's whole content must be that one line.
#
# Stricter than `contains` where the answer IS the whole output: a count
# answers with a number, and "the output holds a 2 somewhere" is a much weaker
# claim than "the answer was 2".
exactly() {
  checks=$((checks + 1))
  if [ "$(cat "$1")" = "$2" ]; then
    echo "  ok      $3"
  else
    fail "$3: the whole output should have been <$2>, and it was <$(cat "$1")>"
  fi
}

# before FILE FIRST SECOND WHAT — both literals are present, FIRST above SECOND.
before() {
  checks=$((checks + 1))
  local top bottom
  top="$(grep -nF -- "$2" "$1" | head -1 | cut -d: -f1)"
  bottom="$(grep -nF -- "$3" "$1" | head -1 | cut -d: -f1)"
  if [ -n "$top" ] && [ -n "$bottom" ] && [ "$top" -lt "$bottom" ]; then
    echo "  ok      $4"
  else
    fail "$4: expected <$2> above <$3> (found at lines '$top' and '$bottom')"
    cat "$1"
  fi
}

# status GOT WANT WHAT — the exit code must be exactly that.
status() {
  checks=$((checks + 1))
  if [ "$1" = "$2" ]; then
    echo "  ok      $3 (exit $1)"
  else
    fail "$3: exited $1, want $2"
  fi
}

step() { echo; echo "== $1"; }

# ---------------------------------------------------------------------------
# Build. Nothing on this machine is assumed to exist except a Go toolchain.
# ---------------------------------------------------------------------------

step "build the two binaries this sequence needs"
mkdir -p "$work/bin"
if ! go build -o "$work/bin/sapedb" "$repo/cmd/sapedb"; then
  echo "::error::cmd/sapedb did not build"
  exit 1
fi
if ! go build -o "$work/bin/sapedbd" "$repo/cmd/sapedbd"; then
  echo "::error::cmd/sapedbd did not build"
  exit 1
fi
sapedb="$work/bin/sapedb"
sapedbd="$work/bin/sapedbd"

# The module is copied out of the repository into the work directory, and every
# command below names it by its bare filename, the way a stranger who was
# handed one file would. It also means a command that silently reached back
# into the checkout would be reaching for something that is not there.
cp "$repo/examples/library/module.json" "$work/module.json"
cd "$work" || exit 1

# ---------------------------------------------------------------------------
# The author's half. Neither of these two commands touches a database.
# ---------------------------------------------------------------------------

step "sapedb keygen author.key"
"$sapedb" keygen author.key > keygen.out 2>&1
status $? 0 "keygen succeeded"

# The public half is the whole of what keygen prints, so this reads it back
# rather than being told it. It is the one value in this script that cannot be
# a hand-written literal: it is different on every run, which is the point of a
# freshly generated identity. Its SHAPE is asserted instead — 64 lower-case hex
# characters and nothing else — and that shape is written out here.
key="$(tr -d '[:space:]' < keygen.out)"
checks=$((checks + 1))
if printf '%s' "$key" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "  ok      keygen printed 64 lower-case hex characters and nothing else"
else
  fail "keygen printed <$key>, which is not 64 lower-case hex characters"
fi

# And the private half went to the file, not to the terminal. The page says
# this in prose — "a private key in your scrollback is a private key in your
# scrollback forever" — so here it is as a check: the file is not empty, it is
# not the same bytes that were printed, and what was printed is one line.
checks=$((checks + 1))
if [ ! -s author.key ]; then
  fail "author.key is empty — keygen did not write the private half"
elif cmp -s author.key keygen.out; then
  fail "keygen printed exactly what it wrote to author.key — the private half is in the scrollback"
elif [ "$(wc -l < keygen.out)" -ne 1 ]; then
  fail "keygen printed $(wc -l < keygen.out) lines; it should print the public half and nothing else"
else
  echo "  ok      the private half went to author.key and only the public half was printed"
fi

step "sapedb seal module.json library.bundle.json < author.key"
"$sapedb" seal module.json library.bundle.json < author.key > seal.out 2>&1
status $? 0 "seal succeeded"
contains seal.out "sealed library.bundle.json, signed by $key" "seal named the file and the public key"

# ---------------------------------------------------------------------------
# The operator's half, refusals first. An empty trust list refuses everything,
# and there is no spelling of SAPEDB_TRUST that means "trust anything".
# ---------------------------------------------------------------------------

step "sapedb verify library.bundle.json          # SAPEDB_TRUST unset"
"$sapedb" verify library.bundle.json > verify-untrusted.out 2>&1
status $? 1 "an untrusted bundle is refused"
contains verify-untrusted.out "this server's trust list is empty; set SAPEDB_TRUST" "the refusal says the list is empty"
contains verify-untrusted.out "no signer is trusted on this server, so no bundle can be" "the refusal names the reason in full"
# The report is printed even when the bundle is refused — a refusal has to be
# takeable back to the person who sent you the file.
contains verify-untrusted.out "operation library:books.get get library_books" "the declarations are still printed on a refusal"

step "sapedb verify library.bundle.json          # on the trust list"
SAPEDB_TRUST="worked-example=$key" "$sapedb" verify library.bundle.json > verify.out 2>&1
status $? 0 "a trusted bundle verifies"
contains verify.out 'bundle "library" version "1.0.0"' "verify named the bundle and its version"
contains verify.out "read      yes  sapedb/bundle:v1" "read: yes"
contains verify.out "signed    yes  ed25519, key $key" "signed: yes, by that key"
contains verify.out "intact    yes  the signature is over exactly these declarations" "intact: yes"
contains verify.out "trusted   yes  an operator of this server put that key on the list" "trusted: yes"
contains verify.out 'says it is "the sapedb worked example"' "the bundle's own claim about its author"
contains verify.out 'known as  "worked-example"' "the label this operator wrote beside that key"

# The three collections, five indexes, one rollup and nine operations the page
# says the module holds. Written out here; not counted out of module.json.
contains verify.out "collection library_books (key id string)" "declares library_books"
contains verify.out "collection library_members (key id string)" "declares library_members"
contains verify.out "collection library_loans (key id string, ulid)" "declares library_loans, auto ulid"
contains verify.out "rollup loans_per_member [member string missing:skip] count" "declares the rollup"
contains verify.out "operation library:books.by_author scan library_books via by_author limit 50" "the row ceiling is in the report"

step "a tampered bundle: the row ceiling moved from 50 to 5000"
sed 's/"limit": 50/"limit": 5000/' library.bundle.json > tampered.bundle.json
SAPEDB_TRUST="worked-example=$key" "$sapedb" verify tampered.bundle.json > tampered.out 2>&1
status $? 1 "a tampered bundle is refused"
contains tampered.out "intact    no   these are not the declarations that key signed" "intact: no"
contains tampered.out "trusted   -    not reached; the signature did not check out" "trusted is not reached"
contains tampered.out "the signature does not match these declarations" "the refusal says the signature does not match"

step "a bundle signed by somebody else's key"
SAPEDB_TRUST="somebody-else=0000000000000000000000000000000000000000000000000000000000000000" \
  "$sapedb" verify library.bundle.json > stranger.out 2>&1
status $? 1 "a bundle signed by a key nobody here trusts is refused"
contains stranger.out "no operator of this server put that key on the list" "the refusal says whose list it is"

# ---------------------------------------------------------------------------
# Install, into a database that does not exist yet.
# ---------------------------------------------------------------------------

step "sapedb -dir lib -account acme -db main install library.bundle.json"
# Asserted rather than assumed: the directory is not there at all. An install
# into a database that already held these collections would pass every check
# below without proving anything.
checks=$((checks + 1))
if [ ! -e lib ]; then
  echo "  ok      the database directory does not exist before the install"
else
  fail "lib already exists before the install, so this proves nothing"
fi

SAPEDB_SECRET="$secret" SAPEDB_TRUST="worked-example=$key" \
  "$sapedb" -dir lib -account acme -db main install library.bundle.json > install.out 2>&1
status $? 0 "install succeeded into a database that did not exist"
# Single quotes around the two halves, with $key spliced between them, so that
# the quotation marks in this line are the quotation marks the daemon prints
# rather than backslash-escaped ones. The drift check in
# docs_worked_example_test.go looks for this sentence in this file's own bytes.
contains install.out 'installing bundle "library" version "1.0.0", signed by '"$key"', trusted here as "worked-example"' \
  "the install line names the bundle, the key and the local label"
contains install.out "collection library_books" "installed library_books"
contains install.out "collection library_members" "installed library_members"
contains install.out "collection library_loans" "installed library_loans"
for operation in \
  library:books.shelve \
  library:members.join \
  library:books.get \
  library:books.by_author \
  library:books.on_shelf \
  library:loans.borrow \
  library:loans.give_back \
  library:loans.of_member \
  library:loans.per_member
do
  contains install.out "operation $operation version 1" "installed $operation at version 1"
done

# Collections first, then operations. That order is not a style: an operation
# naming a collection that is not there yet is refused. Read off the transcript
# rather than trusted.
checks=$((checks + 1))
last_collection="$(grep -n '^collection ' install.out | tail -1 | cut -d: -f1)"
first_operation="$(grep -n '^operation ' install.out | head -1 | cut -d: -f1)"
if [ -n "$last_collection" ] && [ -n "$first_operation" ] && [ "$last_collection" -lt "$first_operation" ]; then
  echo "  ok      every collection was installed before the first operation"
else
  fail "the install did not put collections before operations (last collection line $last_collection, first operation line $first_operation)"
fi

# ---------------------------------------------------------------------------
# Start a daemon on that database and invoke the module over the wire.
# ---------------------------------------------------------------------------

step "start sapedbd on the database the bundle was installed into"
SAPEDB_SECRET="$secret" SAPEDB_DIR="$work/lib" SAPEDB_ADDR="$host" SAPEDB_INSECURE=1 \
  "$sapedbd" > daemon.log 2>&1 &
daemon=$!
trap 'kill "$daemon" 2>/dev/null; rm -rf "$work"' EXIT

# Wait for the daemon to say it is listening, rather than sleeping a guessed
# number of seconds. Bounded, so a daemon that never starts fails the job
# instead of hanging the runner until GitHub kills it.
up=0
for _ in $(seq 1 100); do
  if grep -q "listening on $host" daemon.log 2>/dev/null; then
    up=1
    break
  fi
  if ! kill -0 "$daemon" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
checks=$((checks + 1))
if [ "$up" = "1" ]; then
  echo "  ok      sapedbd is listening on $host"
else
  fail "sapedbd never announced that it was listening"
  cat daemon.log
  exit 1
fi

# invoke is `sapedb invoke HOST NAME ARG=VALUE...`, and it is what makes this
# sequence assertable from a script at all: it exits non-zero when the
# operation is refused.
invoke() {
  SAPEDB_SECRET="$secret" "$sapedb" -account acme -db main invoke "$host" "$@" -insecure
}

step "invoke the module: shelve three books and enrol a member"
invoke library:books.shelve id=bk-1 title="The Left Hand of Darkness" author="Le Guin" shelf=sf-a > shelve1.out 2>&1
status $? 0 "shelved bk-1"
contains shelve1.out 'key "bk-1"' "the insert handed back the key it was given"
contains shelve1.out "changed 1" "one document changed"

invoke library:books.shelve id=bk-2 title="A Wizard of Earthsea" author="Le Guin" shelf=sf-a > shelve2.out 2>&1
status $? 0 "shelved bk-2"
invoke library:books.shelve id=bk-3 title="Piranesi" author="Clarke" shelf=sf-b > shelve3.out 2>&1
status $? 0 "shelved bk-3"
invoke library:members.join id=mem-1 name="Ada" joined=1758412800 > join.out 2>&1
status $? 0 "enrolled mem-1"
contains join.out "changed 1" "the member insert changed one document"

step "invoke a read: library:books.get"
invoke library:books.get id=bk-1 > get.out 2>&1
status $? 0 "get bk-1"
contains get.out '"title":"The Left Hand of Darkness"' "the row carries the title it was shelved with"
contains get.out '"author":"Le Guin"' "the row carries the author"
contains get.out '"shelf":"sf-a"' "the row carries the shelf"
contains get.out '"on_loan":false' "on_loan is false, the value the declaration writes"

step "invoke a scan: library:books.by_author"
invoke library:books.by_author author="Le Guin" > by_author.out 2>&1
status $? 0 "by_author Le Guin"
# Two of the three books are Le Guin's. Written as 2; not counted out of
# module.json, and not counted out of what this script shelved either.
checks=$((checks + 1))
rows="$(grep -c '^  {' by_author.out)"
if [ "$rows" = "2" ]; then
  echo "  ok      by_author handed back 2 rows"
else
  fail "by_author handed back $rows rows, want 2"
  cat by_author.out
fi
# In index order, which for by_author is [author, title] — so the two Le Guins
# come back sorted by title, and A Wizard of Earthsea is above The Left Hand of
# Darkness. That ordering is the index doing its job, and asserting only that
# both rows are present would not have noticed it stop.
before by_author.out '"title":"A Wizard of Earthsea"' '"title":"The Left Hand of Darkness"' \
  "the two rows came back in index order, A Wizard of Earthsea first"
# The whole point of the projection. The documents this read touches carry
# `author` — the index it walks is built on it — and the projection keeps it
# out of the answer.
absent by_author.out '"author"' "author does not escape: the projection does not name it"

step "invoke a count: library:books.on_shelf"
invoke library:books.on_shelf shelf=sf-a > on_shelf.out 2>&1
status $? 0 "on_shelf sf-a"
exactly on_shelf.out "  2" "two books are on shelf sf-a, and that number is the whole answer"
invoke library:books.on_shelf shelf=sf-b > on_shelf_b.out 2>&1
status $? 0 "on_shelf sf-b"
exactly on_shelf_b.out "  1" "one book is on shelf sf-b"
invoke library:books.on_shelf shelf=nowhere > on_shelf_none.out 2>&1
status $? 0 "on_shelf on a shelf nobody used"
exactly on_shelf_none.out "  0" "a shelf with nothing on it counts 0 rather than refusing"

step "invoke a batch: library:loans.borrow, three documents in one transaction"
invoke library:loans.borrow book=bk-1 member=mem-1 borrowed=1758412800 due=1759622400 > borrow.out 2>&1
status $? 0 "borrowed bk-1"
contains borrow.out "changed 3" "one call changed three documents in three collections"
loan="$(grep '^  key ' borrow.out | sed 's/^  key "//; s/"$//')"
checks=$((checks + 1))
if printf '%s' "$loan" | grep -Eq '^[0-9A-HJKMNP-TV-Z]{26}$'; then
  echo "  ok      the loan key is a 26-character ULID, generated by the store"
else
  fail "the loan key is <$loan>, which is not a ULID"
fi

# The condition is inside the transaction, not in anything the caller
# remembered to check: borrowing the same book twice is refused by the
# declaration.
invoke library:loans.borrow book=bk-1 member=mem-1 borrowed=1758412801 due=1759622400 > borrow-again.out 2>&1
status $? 1 "borrowing the same book twice is refused"
contains borrow-again.out 'the document is not in the state this operation requires' \
  "the refusal comes from the declaration's own condition"
contains borrow-again.out 'bk-1 in "library_books" has "on_loan" of true, and it must be false' \
  "and it names the document, the field and both values"

# The refusal was a refusal, not a half-done write. The rollup still counts one
# loan, so the transaction that failed left nothing behind.
invoke library:loans.per_member > per_member_after_refusal.out 2>&1
status $? 0 "per_member after the refused borrow"
contains per_member_after_refusal.out '"count":1' "the refused borrow wrote no second loan"

step "invoke the rollup: library:loans.per_member"
invoke library:loans.per_member > per_member.out 2>&1
status $? 0 "per_member"
contains per_member.out '"count":1' "the rollup counts the one loan"
contains per_member.out '"group":["mem-1"]' "and groups it under mem-1"

step "invoke library:loans.of_member"
invoke library:loans.of_member member=mem-1 > of_member.out 2>&1
status $? 0 "of_member mem-1"
contains of_member.out '"book":"bk-1"' "the loan names the book"
contains of_member.out '"returned":false' "the loan is not returned yet"

step "invoke library:loans.give_back"
invoke library:loans.give_back loan="$loan" book=bk-1 returned_at=1758499200 > give_back.out 2>&1
status $? 0 "gave bk-1 back"
contains give_back.out "changed 2" "two documents changed"
invoke library:books.get id=bk-1 > get-after.out 2>&1
status $? 0 "get bk-1 after the return"
contains get-after.out '"on_loan":false' "the book is on the shelf again"

# ---------------------------------------------------------------------------
# The refusals. A path that only ever succeeds proves less.
# ---------------------------------------------------------------------------

step "a malformed invocation exits non-zero, with the argument named"

invoke library:books.get > missing.out 2>&1
status $? 1 "a missing required argument is refused"
contains missing.out '"library:books.get" needs "id", which is a string' "the refusal names the missing argument and its type"

invoke library:books.get id=bk-1 shelf=poetry > extra.out 2>&1
status $? 1 "an argument the declaration does not name is refused"
contains extra.out '"library:books.get" does not take "shelf"' "the refusal names the argument that is not declared"

invoke library:members.join id=mem-2 name=alan joined=yesterday > wrongtype.out 2>&1
status $? 1 "an argument of the wrong type is refused"
contains wrongtype.out '"joined" is a number, and "yesterday" is not one' "the refusal names the argument and what it should have been"

invoke library:books.burn id=bk-1 > nosuch.out 2>&1
status $? 1 "an operation that was never declared is refused"
contains nosuch.out 'there is no operation called "library:books.burn" here' "the refusal names the operation"

# ---------------------------------------------------------------------------
# The daemon is the process it was. Asserted on the process rather than
# inferred from the socket still answering.
# ---------------------------------------------------------------------------

step "the daemon served all of that without restarting"
checks=$((checks + 1))
if kill -0 "$daemon" 2>/dev/null; then
  echo "  ok      sapedbd is still pid $daemon"
else
  fail "sapedbd is gone; it was pid $daemon"
  cat daemon.log
fi

# ---------------------------------------------------------------------------
# The floor, and the verdict.
# ---------------------------------------------------------------------------

echo
echo "checks: $checks"
echo "failures: $failures"

# Hand-written, and deliberately a little under the real count so that adding
# one assertion does not force an edit here — but well above zero, which is
# what a run that fell out of the sequence early would report. .github/workflows/test.yml
# refuses a Go report with fewer than 100 PASS lines for the same reason.
floor=70
if [ "$checks" -lt "$floor" ]; then
  echo "::error::only $checks assertions ran, and this sequence has at least $floor — the run did not do what it looks like it did."
  exit 1
fi

if [ "$failures" != "0" ]; then
  echo "::error::$failures assertion(s) failed: the commands on docs/external-operations-page.md did not do what the page says they do."
  exit 1
fi

echo "the worked example was built, signed, verified, installed and invoked from cold."
