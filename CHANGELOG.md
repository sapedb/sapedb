# Changelog

This file starts at the entry below (task 0045). Nothing before this point was
recorded, so its absence is not a claim that nothing changed before it.

## Unreleased

### Changed

- **An operation name is now `namespace:name`, and this is a breaking change to
  what a name means (SAPE-9).** It had to land before 1.0.0 or never: every
  separator is a legal operation name today — `internal/store`'s
  `TestAnOperationNameMayHoldAnySeparatorAName` declares `acme:orders.recent`,
  `orders/recent` and four others and all of them are accepted — and
  COMPATIBILITY.md section 3 forbids a 1.x release from tightening a rule so
  that a declaration valid in 1.0 is refused in 1.1. Refusing `a:b:c` in a 1.1
  would have been exactly that. There is no version of this that arrives after
  the tag.

  **Every flat name keeps working, unchanged.** A name with no `:` is in the
  *unnamed namespace*. Nothing about it moved: it resolves the way it always
  did, a second declaration of one still silently takes over every unversioned
  call, and `internal/store/collision_test.go` — which measures exactly that —
  still passes without an edit. The unnamed namespace is the one nobody owns,
  and it is now documented as that rather than left as an accident.

  **What is refused, and was not before.** More than one `:` in a name; a `:`
  with either side empty (`:foo`, `foo:`); and a namespace holding anything but
  letters, digits, `.`, `-` and `_`. A namespace is compared by eye and typed by
  hand, so permitting arbitrary bytes now is a decision nobody could walk back
  later. The 128-byte ceiling is on the **whole** name — namespace, separator
  and name — because a namespace is not a way to get a longer public limit.

  **A named namespace belongs to whoever declared into it first.** Every later
  declaration into it from anybody else is refused, by name, in a sentence that
  says who holds it. That is what makes a namespace worth having: two vendors
  who each pick one can no longer redeclare over each other, and the failure
  moves from *a silent takeover on the next call* to *the install stops and the
  customer reads a sentence naming both parties*. Two vendors who both stay in
  the unnamed namespace are exactly as exposed as they were.

  **The claimant is a kind as well as a string.** A declaration that arrived
  inside a signed bundle (`sapedb install`) is claimed for the ed25519 key that
  signed it; anything else is claimed for `Caller.Actor`. Both are stored, so a
  key and an account that happen to spell the same are two different claimants
  and not one — the key was checked, over exactly the declarations it carried,
  and an actor is a name somebody was called at the time.

  **`namespace` is a new refusal code on the wire.** `store.ErrNamespace` has
  its own row in `internal/server`'s `codeFor` rather than falling through to
  `failed`, which is the bug ISS-21 is this project's record of, and it is
  deliberately not `declaration`: a namespace refusal tells its reader to pick a
  namespace of their own, and `declaration` tells them to fix their operation.
  This code is **not** in `fixtures/frames.json` — that artifact carries frame
  numbers, request body shapes and encoding cases, and has never held a failure
  code table — so no client's copy of it needs syncing for this.

  **Known limitation, not fixed here:** a dump does not carry namespace claims,
  so a database restored from one comes back with every namespace unclaimed and
  the first declarer afterwards takes it. `Restore` writes declarations straight
  into the tree without going through `DeclareOperation`, so a restore is never
  *refused* by this; it simply forgets who owned what. Adding a claim line to a
  dump is a dump-format change and belongs to its own ticket.

### Added

- **`sapedb verify -envelopes`: a bundle's cost envelope, readable before it is
  installed (SAPE-12, criterion 4).** The criterion has two halves and only one
  of them held. "Matches what it does" was measured — a live test runs each
  operation of the worked module and compares the answer against the envelope,
  and deleting a `projection` from the module makes it fail by name. "Readable
  **before** installation" was not: `verify` printed what a bundle *declares*
  and no envelope, there was no flag for one, and the only way to see an
  operation's cost was to install the bundle into a database first. That is
  backwards. The envelope exists so somebody can decide whether to trust an
  operation before running it, and an answer you can only get by installing the
  thing is not an answer to that question. The page said so out loud, in a
  paragraph that has now been replaced.

  **One derivation, reached from two places — not a second implementation.**
  `Store.envelopeOf` and `ceiling()` only ever needed a store for one thing:
  looking up the operation a composed step names. Both now take that lookup as
  an argument (`store.Operations`), so the walk itself is `store.EnvelopeOf`,
  called by the catalogue with the store's own `Operation` method and by
  `internal/bundle` with a lookup over the file. Nothing was copied. Two
  functions computing a ceiling would be two answers that can drift, and what
  this criterion asks for is precisely that the number read before installing
  equals the number read after.

  **Measured as that, not asserted.**
  `TestTheEnvelopeReadFromTheBundleIsTheEnvelopeReadFromTheStore`
  reads every envelope out of
  `examples/library/module.json` with no database in existence, then installs
  that same module over the wire and reads every envelope back out of the
  catalogue, and compares them field for field for all nine operations — plus a
  whole-struct comparison, so a tenth field added to `store.Envelope` tomorrow
  cannot quietly fall outside the check. The one field that is *allowed* to
  differ is asserted rather than skipped: a bundle carries no version, the
  store assigns 1, and both halves of that are checked. A test of the
  pre-install path alone would have been self-confirming; this one puts the
  right-hand side through sealing, verification, nine `declare` frames, a JSON
  round trip into a B-tree and a commit first.

  **A composed step's ceiling is not readable from a file, and it says so
  rather than guessing.** A step must pin a version (N1), and a bundle's own
  operations carry no version — only the receiving store assigns one — so a
  reference inside a bundle can only ever name a declaration that store already
  holds. That envelope prints as `unreadable`, naming the reference it stopped
  at, and does not change the exit status: a bundle with a composed step is
  perfectly installable, and the gap is in what the file can tell you, not in
  the file. Three guesses were considered and all three are worse — matching by
  name ignoring the version, summing only the readable steps, and falling back
  to the enclosing declared limit each produce a number that can disagree with
  what the store reports, which is the one failure this is built to rule out.
  The worked module composes nothing, so all nine of its envelopes are real.

  Nothing on the wire, in the protocol or in `fixtures/frames.json` changes.
  `store.Envelope` keeps every field and every tag it had; the catalogue sends
  exactly what it sent before. This is a new flag on a command that opens no
  database, plus one exported function and one exported type in packages no
  client links against.

- **`deleteRange`: a stretch of keys removed in key order, with a ceiling the
  host counts (SAPE-32).** Asked whether a batch could remove a few thousand
  rows atomically, this project's planner said no, and gave as the reason that
  `steps` is a field on the declaration so a batch's shape is fixed when it is
  declared. That is true and it was the wrong reason: it describes how the
  **driver** models a batch and says nothing about what the **engine** may do.
  The proof was already here — a rollup *is* the engine computing, kept
  incrementally, and the driver never lets anybody write that as an expression,
  it lets them declare *this rollup*. Engine computes, driver declares, and the
  promise is intact. This is the same trade on a second primitive.

  **It is not a cheap truncation, and the name says it is.** Reading
  `Collection.remove`: each document has to be READ before it can go, because
  its index entries are derived from its contents; then every entry is deleted,
  then `contribute(tree, document, -1)` adjusts every rollup it fed, then the
  document itself goes, then the change is recorded. So one run costs
  `limit × (one document read + its index entries + its rollup contribution +
  one log entry)` — linear in `limit`, every unit bounded, which is exactly what
  makes it declarable and exactly why anybody who reads the name and assumes
  otherwise will be surprised by the log rather than by the latency. The
  acceptance test that would catch a fast path skipping `contribute` is the one
  that deletes the identical rows one at a time in a second store and compares
  both stores' rollups and index entries afterwards: a range delete that leaves
  an index entry behind is a corruption that reads as success.

  **N deletions write N log entries, and not batching them is the answer
  rather than a thing left undone.** The entries this writes are *exactly* the
  entries the same deletes produce one at a time — same kind, one per key, same
  attribution — measured field for field, with a second assertion that nothing
  of any other kind was written among them. That is what lets a follower replay
  a range delete understanding nothing it did not already understand, and it is
  why one entry carrying a key list was not built: a new log shape is a thing
  every follower, every dump and every subscriber has to learn before the first
  one can be written. What this does do is make **ISS-23** matter more rather
  than fix it — no operator can cap retention, so removing a large file's worth
  of rows grows the log by that much. That is the reason the limit here is
  required with no default: the number bounds what is destroyed *and* what
  every follower replays, and a default would be this store deciding on
  somebody's behalf how much of their data goes.

  **The ceiling is counted, not trusted — the same rule `hashRange` wrote
  down.** Against one counter, per **run** of the operation and not per
  underlying call; what is counted is rows removed. A partitioned collection is
  where that distinction is visible without anybody writing a loop, because one
  run is several walks of several files: six rows over two partitions under a
  limit of five removes five and says more remain, where a counter restarted
  per file would see three and three and report that it had finished. That is
  the W7 shape (`pipelines/tasks/0071`: an operation declaring `limit 50`,
  correctly sandboxed and correctly signed, served **50,000,000 rows** in one
  call, because the host checked every call and never the total — a signature
  proves whose binary it is, a sandbox proves it does not escape, neither
  proves what it costs), measured here on the action where getting it wrong
  destroys data rather than returning it.

  **At the ceiling it stops and says so, where a `hashRange` stops and
  refuses**, and the difference is deliberate. SAPE-34 refuses because a digest
  of part of a range is thirty-two bytes in the same field as a digest of all
  of it: there is no shorter honest answer, so there is no answer. That
  argument is about the shape of the answer and does not survive the move here.
  This one hands back how many went, the last key removed, and whether more
  remain — `Changed`, `Key` and the `truncated` a scan already sets — so its
  answer says that it is short, which is the property the refusal exists to
  supply when it is missing. It also has to be this way for the ticket's own
  first criterion to be satisfiable: paging a thousand rows needs the last key
  to come *back*, and the rows are already gone by the time a refusal could be
  raised, so refusing would not un-remove them, only hide which ones went.

  **Key order only, and the partition rule is the existing one.** An index is
  refused: removing a document removes every entry it has, an array index
  spreads one document over several of them, and a tie in an index has no order
  of its own, so which rows a limit stopped at would not be decided by the
  declaration. A direction is refused for the sharpest version of the reason a
  `count`'s is — under a limit the two directions remove the two opposite ends
  of the stretch, so a caller-chosen direction would be a caller choosing which
  half of somebody's data goes. And a hash-partitioned collection is refused by
  `scanAcross`, the same answer `scan`, `count` and `hashRange` already get,
  matched rather than decided again; it binds harder here, because without key
  order "up to N, and here is the last key" is not a cursor at all.

  **Both layers.** The declared operation takes `from` and `to` from the caller
  and carries the limit itself; `delete <collection> [from …] [to …] limit <n>`
  does the same thing at the operator shell, prints how many went, the last key
  as JSON to paste after `after` on the next line, and the `… and more` sentence
  `scan` already prints. The two are measured against each other on the same
  range, because two spellings of one primitive that disagree is worse than one
  spelling — and they cannot drift, because the typed one is not a second path:
  `Explore` builds the Operation it would have to be declared as, holds it to
  `validateOperation`, runs it through `perform()` and hands the draft back.
  This is the **first typed access that writes**, and the sentence that used to
  stand in `Access.Kind`'s doc — nothing writes — is kept in view there rather
  than deleted, because it is still true of every change this shell should be
  used for. The limit is asked for rather than capped at `MostRows`, which is
  the third answer that rule has now given to three actions: a capped scan
  still answers, a capped hash does not answer at all, and a capped delete
  would answer perfectly well — the answer being that this shell picked how
  many of somebody's rows to destroy.

  **What a client has to learn.** An eleventh action name, `"deleteRange"`. No
  new fields on a declaration, no new fields on a result — `changed`, `key` and
  `truncated` are the ones a delete and a scan already use — and **no new
  refusal code**, because nothing here refuses in a way the existing table does
  not already name. `fixtures/frames.json` needs nothing: it carries frame
  numbers and request body shapes and deliberately leaves `store.Operation`,
  `store.Spec` and `store.Access` opaque. The one thing that is genuinely new
  to a client is that `store.Writes` now calls this a write, so a follower
  refuses it and the server takes the write lock for it.

  **One cost this cannot hide.** A visitor cannot delete from the tree it is
  being walked by — `btree.ascend` decodes a branch page once and then reads
  the children it decided on, and a delete partway through can merge and free
  exactly those pages — so the keys are collected during the walk and removed
  after it, which is what `store.go`'s own byte-level `deleteRange(prefix)` has
  always done for the same reason. That means this holds one primary key per
  row it is about to remove, where a `hashRange` holds one row at a time and
  drops it. The memory is bounded by the declared limit, which is the number
  the declaration exists to say — but it grows with that number, and a limit of
  ten million is ten million keys in memory.

  **Not in this one:** capping or compacting the change log (ISS-23), any new
  log entry shape, deleting by anything other than a key range, and dropping a
  whole collection — which is `Store.Drop`, and exists.

- **`hashRange`: the SHA-256 of a stretch of keys, without the stretch leaving
  the server (SAPE-34).** The values of a key range, in key order, joined with
  nothing between them, answered as thirty-two bytes. It is how an application
  that stored a ten-megabyte file as five thousand two-kilobyte rows — the
  workload `bench/` already drives — asks the one question it could not ask
  without moving everything back: *did all of it arrive, and is it still what it
  was?* The joined value never exists anywhere; the digest is fed a chunk at a
  time and the chunk is dropped. Measured: hashing a 512 MiB range moved the
  live heap by 3.6 MiB, the same 3.6 MiB a 32 MiB range moves, with the whole
  process under a 32 MiB `GOMEMLIMIT` and a peak RSS of 17 MB.

  **Nothing between the values, and key order only.** Both rules exist so that
  how a file was split is not a fact about the file: a separator would make the
  chunk boundaries part of the answer, and the same bytes re-chunked would hash
  differently. So the same megabyte as 1 KB rows and as 4 KB rows hashes the
  same, and that assertion is the one that would catch a delimiter creeping in.
  An index is refused rather than allowed as a second order over the same rows —
  an array index visits one document more than once, and a tie in an index is no
  order at all, so "the hash of this stretch of some index" is a sentence with
  more than one answer.

  **`decode` is a declared word, `"base64"` or absent.** Binary is stored as
  base64 text today because a `string` cannot carry arbitrary bytes — measured,
  the byte `0xFF` comes back as `U+FFFD` — so hashing the values *as stored*
  would answer the SHA-256 of a base64 transcript, which can never equal what a
  client computed from the original file. Two correct systems would disagree
  forever and the disagreement would look like corruption. It is an enumeration
  on the declaration, in the same family as an index field's
  `missing: skip | first | last`, and not an expression a caller composes. A
  real bytes type will make it unnecessary for new data; it keeps working for
  the data that is already base64.

  **A row it cannot hash refuses, and names the key.** Missing field, not text,
  or not base64 — all three stop the walk. The precedent is the rollup, which
  refuses a document it cannot add rather than skipping it because a total that
  silently skips is a total nobody can trust. A digest is worse: it is
  thirty-two bytes that look exactly as authoritative with a row missing as
  without.

  **The ceiling is counted while it walks, and this is the part that had to be
  this way.** Everything in this store before it is bounded *by construction*: a
  declaration cannot express a loop, because a step pins `operation@version`,
  version numbers only ever rise, and a pinned reference resolves only to a
  version that already exists — so a cycle would need a version to exist before
  it was declared. That is why `ceiling()` can total a declaration at declare
  time and why nothing counts during a run. **That property does not survive a
  thing that walks.** The gap is measured, in `pipelines/tasks/0071` as W7: an
  external operation declaring `limit 50`, correctly sandboxed and correctly
  signed, served **50,000,000 rows** in one call — one million host calls of
  fifty each — because the host checked every call and never the total. A
  signature proves whose binary it is; a sandbox proves it does not escape;
  **neither proves what it costs.** So the limit here is a ceiling the host
  counts against while walking, against one counter, **per run of the operation
  and not per underlying call** — a range spread over two partition files is
  still one run. What is counted is rows read. At the ceiling it **stops and
  refuses**: never the digest of a prefix, which is indistinguishable from the
  digest of the whole range and so is the most dangerous output this could
  have. The answer is thirty-two bytes however far it walked, which is exactly
  why the work needs a ceiling the size of the answer will never reveal, and
  why the declaration is the only place that cost can be written down.

  **All three layers.** The declared operation takes `from` and `to` from the
  caller and carries `field`, `decode` and `limit` itself; `hash <collection>
  field <path> [decode base64] [from …] [to …] limit <n>` does the same thing at
  the operator shell and prints the digest with the number of rows under it. The
  shell is the one place this does **not** borrow an existing habit: `scan` and
  `count` have their limit quietly capped at `MostRows` when an operator does
  not say, because a capped scan still answers — a page, and a flag saying there
  was more. A capped hash does not answer, it refuses, so capping would refuse
  every file this command exists to check. A typed `hash` is asked for its limit
  instead.

  **What a client has to learn.** A tenth action name, `"hashRange"`; two new
  optional fields on a declaration, `field` and `decode`; one new field on a
  result, `digest`, lower-case hex so it pastes against what `sha256sum` printed;
  and two new refusal codes, `digest` and `ceiling`. They are two rather than one
  because they tell their reader opposite things — `digest` means a row is wrong
  and retrying changes nothing, `ceiling` means the answer would have been
  correct and the walk was longer than the declaration paid for. Neither is in
  `fixtures/frames.json`: that artifact carries frame numbers and request body
  shapes and deliberately leaves `store.Operation`, `store.Spec` and
  `store.Access` opaque, and it has never held a failure code table — so no
  client's copy of it needs syncing for this.

  **Not in this one:** hashing a single document or a whole collection, any
  second digest algorithm, and storing the digest anywhere. This returns it;
  writing it down is SAPE-33's shape. A `hashRange` also cannot be a step of a
  batch, for a sharper version of the reason a `count` cannot: a composed answer
  holds one `digest`, so a second one would overwrite the first in place and
  hand the caller the wrong range's hash in the right field.

- **There is now a benchmark, and it measures this store under a small VPS's
  limits rather than on a laptop with everything switched off.** There was no
  performance measurement in this repository at all before this — no
  `func Benchmark`, no rig, nothing — so every sentence anyone has ever written
  about how fast this is was a sentence nobody had measured. `bench/` is the
  rig: `./bench/run.sh` (or `make bench`) builds the server image from the tree
  it is standing in, runs it under three profiles in turn — 1 cpu/512m, 1 cpu/1g,
  2 cpus/2g — drives one workload at it over the wire, samples the container's
  memory from outside while it does, pushes one profile until it runs out of
  memory, writes a report, and takes the stack and its volume down again.

  **`memswap_limit` is set equal to `mem_limit` on every profile, and that line
  is the whole exercise.** It bounds memory *plus* swap; leave it out and docker
  allows swap up to twice `mem_limit`, so the container swaps instead of hitting
  the wall, every number comes back looking healthy, and the one machine this
  rig exists to say something about is the one thing that never gets measured.
  `run.sh` reads the applied limits back off the running container and prints
  them into the report rather than trusting that the compose keys did anything.

  **The workload is the application's, not a synthetic one**: a ten-megabyte
  file stored as five thousand base64 rows of two kilobytes under keys shaped
  `<id>/000000`, read back by a paginated prefix scan with an exclusive `from`
  and a limit declared in the operation, and deleted one row per request —
  which is the baseline `deleteRange` (SAPE-32) has to be compared against, so
  it is recorded carefully rather than in passing.

  **The first run is committed as `bench/results/baseline-2026-09-21.md`,** and
  it says on its face that it is one machine on one day. Every number in it
  carries the commit, the applied limits, the fact that swap was off, the host,
  the runs it was measured over and its min/median/max — and the caveat that
  Docker Desktop on macOS is a Linux VM whose disk is not a VPS's, which is why
  these are shapes rather than promises.

  **The report now says which comparisons it supports, because it used to say
  the wrong one.** It told the reader to "read the ratios between profiles" —
  and for insert that is the one ratio the run cannot support: the three
  profiles are measured in turn on a host nothing holds still, so position in
  the run and profile identity are the same variable, and the giveaway is that
  insert gets *slower* as the limits get larger. `bench/README.md` said so; the
  report did not, and the report is the document people read. The warning is now
  emitted by the generator directly above the insert table, so it cannot be
  separated from the numbers it qualifies. Scan and delete keep the cross-profile
  comparison: there the three agree to within a few percent, and three separated
  runs landing on the same number is the evidence that the host was steady.

  **What the first run found, and it is not the number anyone expected to care
  about.** Steady-state memory for this workload never left 16–20 MiB on any
  profile, 512m included: the store is not what fills a small machine. What
  fills it is one read. A scan whose declared limit is far above what the
  collection holds — an operation declared once, when the collection was small —
  builds every row in memory first: past about ten thousand rows the answer is
  over `protocol.MaxPayload` and the server hangs the connection up, and it does
  that having *already paid the memory for the answer it cannot send*. The
  frame cap is a guard in front of the wall that does not stop anybody reaching
  it: on the 512m profile the same read was still hanging up at forty thousand
  rows and took the process with it at forty-five thousand — exit 137,
  `OOMKilled: true`. Nothing in the store's own accounting is wrong there; the
  declared limit was honoured. It is just that "how many rows" is not "how much
  memory", and on a machine with 512 MB those are not close enough to be the
  same promise. Written down here rather than fixed, because fixing it is a
  design decision about streaming a read, not a patch.

  **The second thing it found is the delete baseline itself.** Deleting five
  thousand rows one request at a time got monotonically slower every time it was
  done: 1097, 737, 561, 409 and then 304 rows/second over five consecutive
  files on the 512m profile — and 1109→322 and 1140→326 on the other two, which
  is the same curve three times rather than noise. Nothing here says why, and
  this rig cannot: it measures from outside the process. It is written down as
  the shape `deleteRange` (SAPE-32) has to beat, and as a question — a store
  whose delete rate falls by a factor of three and a half over twenty-five
  thousand deletes is a store with something growing in it.

  **Nothing here runs as part of `go test ./...`.** The load generator is a
  `main` that needs a server to point at; the only tests in `bench/` are for the
  two functions that compute min/median/max, and they take microseconds. There
  is still no `func Benchmark` in this tree, on purpose: Go's benchmark runner
  picks its own iteration count and reports one number, and this rig has to
  report a range over a fixed number of runs, pair each phase with a peak memory
  sampled from outside the container by the host, and survive the process under
  test being killed halfway through.

- **CI follows the worked example from cold on a machine that is not the
  author's (SAPE-12, criterion 3).** Every command on
  `docs/external-operations-page.md` had been run once, by hand, on one laptop,
  on macOS/arm64. `.github/workflows/worked-example.yml` runs the whole
  sequence on an `ubuntu-latest` runner on every push and pull request:
  `keygen`, `seal`, `verify` with and without a trust list, `install` into a
  database directory that does not exist yet, a real `sapedbd`, and then
  `sapedb invoke` against all nine operations of `examples/library/module.json`
  — reads, writes, a batch that changes three documents in one transaction, and
  a rollup — with the answers asserted, not just the exit codes. 94 assertions
  in `.github/scripts/worked-example.sh`, under `env -i` so that no inherited
  `SAPEDB_*` can be what makes it pass.

  **The refusals are asserted too**, because a path that only ever succeeds
  proves less than one that also proves the door is shut: an empty trust list,
  a bundle signed by a key nobody here trusts, a bundle whose row ceiling was
  edited from 50 to 5000 after signing, a required argument left out, an
  argument the declaration does not name, an argument of the wrong type, an
  operation that was never declared, and borrowing a book that is already out.

  **The job does not read its commands out of the page.** A job that did could
  not fail when the page was wrong — it would run whatever the page said and
  call it green. The commands are written out by hand in the script, and
  `docs_worked_example_test.go` holds them a third time as literals of its own
  and demands to find each one on both sides, so a page edited without the job,
  or a job edited without the page, is named as such. Whitespace is flattened
  before searching, with a control that names a sentence which genuinely spans
  a line break on the page and would be invisible to a line-based `grep`.

- **`invoke` in the operator shell and on the command line, so a declared
  operation can actually be run from the tool that installs one.** Until now
  the shipped command line could install an operation and not call one:
  `internal/cli/shell.go` handled `help`, `ls`, `declare` and the ad-hoc
  `get`/`scan`/`count`, and `grep -rn '"invoke"' internal/cli/*.go` found
  nothing outside tests. A stranger who installed `examples/library` could `ls`
  its nine operations and had nowhere to go without writing Go or TypeScript,
  which for a database whose whole argument is declared operations is the most
  quotable flaw in it.

  **The arguments come from the declaration.** `invoke library:books.get
  id=bk-1`: the name is one word, so a namespaced name with a colon in it is
  typed whole and the colon is never a separator this grammar reads; each
  argument is `name=value`, split at the first `=`, so a value may hold one.
  The declared type decides the encoding — a `string` is written as it stands,
  a `number`, a `bool` and an argument declared `any` are written as JSON — so
  the operator never guesses a quoting rule the database already knows.

  **Three refusals, each by name, before anything goes out.** A missing
  required argument names which one and what type it is; an argument the
  declaration does not name is refused with the list of the ones it does take,
  never dropped; a value that is not the declared type says which argument and
  what was expected. `internal/store`'s `bind` still checks all three on the
  far side and is still the authority — this is not a substitute for it, it is
  what makes `id=bk-1` mean a string in the first place.

  **`sapedb invoke HOST NAME ARG=VALUE...` is the same thing for a script**,
  going through the same parser and printer. It exists for one reason the
  shell cannot cover: `echo 'invoke …' | sapedb shell` prints a refusal and
  exits 0, so a deployment step cannot tell a write from a refusal. This
  returns the error, so the status is 1. The host is required rather than
  defaulted, because an operation name may hold a colon and an optional
  leading host would make the first word ambiguous between `host:port` and a
  namespaced name.

  **`InvokeVersion` is deliberately not reachable from either.** The shell's
  contribution over a raw wire call is checking the arguments against the
  declaration, and the only declaration it can read is the newest:
  `store.Operations()` keeps one entry per name, so `ls` shows no older
  versions and `WhatIsHere` carries none. Reaching version N while validating
  against the newest would check the arguments against the wrong declaration,
  and the number would have to be typed from nowhere. When the catalogue
  carries older versions, the syntax is already free — a bare word can never
  be an argument, which must hold `=`.

- **A worked external-operation module, and the tests that keep it honest
  (SAPE-12).** `examples/library/module.json` is a lending library: three
  collections with their own keys, five indexes, one rollup, and nine
  operations across five of the nine actions. It is deliberately a whole small
  module rather than one missing verb, because a module is the hard case — it
  has to bring its own storage, and until SAPE-14 shipped `establish` it could
  not.

  **What is shipped is the draft, not a sealed bundle.** `sapedb seal` needs a
  private key, and a private key in a repository is not private. The draft is
  the source; the bundle is a build product an author makes with their own key,
  and `keygen` → `seal` → `verify` → `install` is four commands. The trade is
  that nobody can verify a signature on a file shipped here, and what is gained
  is that the example cannot teach anybody to commit a signing key.

  **Every read in it declares a `projection`**, which is the rule SAPE-13 writes
  down and the thing the older `examples/ledger` fixture is silent on. Measured
  through `@ecosy/sapedb`'s generator: of the nine operations, three reads emit
  a named-field row, five writes and the count emit `row: never`, and exactly
  one emits `Record<string, unknown>` — `library:loans.per_member`, because a
  rollup row is a synthetic `{count, group}` the store builds from the rollup's
  own declaration and no projection is applied to it. The same command against
  `fixtures/ledger.schema.json` emits three, all of them document reads. The
  rollup case is recorded as debt rather than worked around: typing it means
  teaching a client to read `collections[].rollups[]`, which none does today.

  **`library_live_test.go` holds three things.** That the module installs over
  the wire, on one connection, into a database that has none of its collections
  — SAPE-14's eighth acceptance criterion, which SAPE-14 shipped without and
  carried here; the daemon's pid is compared before and after rather than
  inferred from the socket staying up. That each cost envelope says what the
  operation actually does, checked by running it — `borrow` lists three
  collections and `give_back` lists two of the same three, and the member
  document is read directly to prove `give_back` leaves alone the collection its
  envelope omits. And that every read of documents declares a projection, in
  both directions, so that adding a fourth unprojected scan reddens it too.

  Every expectation in that file is typed out on the test side. None of it is
  read out of `module.json` and compared against itself.

- **A declaration now records the identity that declared it (ISS-32).**
  `store.Operation` gains one optional field, `declaredBy`, carrying a kind and
  an identity: the ed25519 key that signed the bundle a declaration arrived in,
  or the caller's actor otherwise. SAPE-9 recorded who owns a *namespace*, in a
  record of its own; it put nothing on the declaration.

  **It had to land before 1.0.0.** COMPATIBILITY.md section 3 does permit a 1.x
  to add an optional field to a declaration, so the rule could wait — but the
  field could not. A declaration is stored *inside the database*, so every
  operation declared between the tag and some later release would be one this
  field could never be filled in for. There is no migration that invents an
  identity nobody wrote down.

  **Why on the declaration rather than in the log.** The change log already
  carries an `Attribution` for every declare, and it is not enough for two
  reasons that are both measured. A log is trimmed: `Retain` drops the entry
  naming the declarer while the declaration it describes stays stored, which is
  `internal/store`'s `TestTheLogEntryNamingADeclarerIsPrunable`. And a dump
  carries no log at all — `dump.go`'s `line` has no attribution anywhere on it
  and `Restore` handles only collections, operations and documents — so a
  replica built from a dump held every declaration and knew who wrote none of
  them. In the unnamed namespace, where there is no claim record either, the
  answer was simply gone. The field survives both, because a dump carries the
  whole `Operation`; `TestTheDeclarerSurvivesADumpAndRestore` dumps a database,
  restores into an empty one and reads the identities back out.

  **The store sets it, never the caller.** Whatever arrives in the field is
  discarded and replaced, in the same breath as `Version` and for the same
  reason: a field a client can set is a field a client can lie in, and this one
  would be worth lying in. A declaration submitted with somebody else's key in
  it comes back recorded against whoever actually declared it, and one
  submitted through a caller that names nobody comes back with no declarer at
  all rather than keeping the forged value.

  **Absent stays legal.** The field is `omitempty` and a pointer, so a
  declaration stored before this existed reads back without one and does not
  grow a value it never had, and a caller that names nobody writes no field.
  Redeclaring records the identity that made *that* version; older versions
  keep theirs, which is what makes an audit record naming an operation version
  still answerable years later.

  **It is a record, not a rule.** Nothing reads it to decide anything. Who may
  declare into a namespace is still settled by `claimFor` against the claim
  records SAPE-9 added, and writing the holder's identity into a submitted
  declaration does not get a stranger past it.

  **One behaviour changed outside the store.** `internal/cli`'s `sameOperation`
  — the comparison behind `apply` and `install` skipping a declaration that is
  already there unchanged — now ignores `declaredBy` as it already ignored
  `version`. Without that, a stored declaration differs from every file that
  produced it and `apply` run twice writes a second version of everything. The
  consequence, deliberate: an identical declaration re-applied by somebody else
  keeps the identity already on it, because nothing was declared.

  **`fixtures/frames.json` is unchanged, and no client's copy needs syncing.**
  That artifact marks the declare frame's `operation` field
  `"opaque": "internal/store.Operation"` and has never carried a field table
  for a declaration — the shapes it spells out are the top-level fields of each
  request frame's own struct, and `declaring` did not gain one.

  **Known limitation, unchanged from SAPE-9:** a dump still does not carry
  namespace *claims*. A restored database knows who declared each operation and
  still comes back with every namespace unclaimed. That remains a dump-format
  change and its own ticket.

- **A signed bundle can now be checked and installed (SAPE-28).** SAPE-10
  could seal and verify one and stopped there: nothing read a trusted key out
  of any configuration, and none of the six `bundle_*` refusal codes in
  `internal/server` had a call site. This is the install path, and it is two
  commands and one environment variable.

  **`SAPEDB_TRUST` is how an operator says whose bundles this host will look
  at.** Written `label=key`, entries separated by commas or newlines, where
  `key` is the 64 lower-case hex characters of an ed25519 public key:

  ```
  SAPEDB_TRUST='acme-eng=3f0b…, partner-co=9c1d…'
  ```

  An environment variable rather than a file, because that is the only
  configuration surface this product has — `internal/service.FromEnv` reads
  every setting `sapedbd` has out of the environment, there is no
  configuration file anywhere in the tree, and a trust file would itself need
  an environment variable to name it. Unset, empty, or whitespace is an empty
  list, and an empty list is **not** permission: every bundle is refused with
  `bundle.ErrNoTrust` ("no signer is trusted on this server, so no bundle can
  be"), which is a fact about the server rather than about the file. There is
  no spelling of `SAPEDB_TRUST` that means "trust anything", so no deployment
  can reach that state by mistyping one. A malformed entry is refused by name
  rather than dropped — an operator who mistyped a key is not told that nobody
  is trusted — and one label over two keys is refused as well as one key under
  two labels, because the label is what `verify` prints and what the change
  log records.

  **`sapedb verify FILE`** reads a bundle, runs the three checks, and prints
  what it found. No database, no lock, no secret, no network: the person
  deciding whether to trust a file may not hold this server's secret and may
  not have picked a database for it. It prints the bundle's self-asserted
  `signer` **beside** the label this operator wrote next to that key, because
  the difference between those two is the only identity fact a bundle carries
  — there is no PKI here, and a person comparing them is the whole of the name
  binding. It prints the declarations too, under a heading that says out loud
  when nothing below is vouched for. The report is printed for a refused
  bundle as well as an accepted one; the exit status is the summary, the
  report is the point.

  **`sapedb install FILE`** hands a verified bundle's `Collections` and
  `Operations` to the same `db.Declare` / `db.DeclareOperation` loop `apply`
  already runs — which is the whole reason a bundle carries those two lists
  and not a container of its own — including `apply`'s "already there,
  unchanged" skip, so re-installing a release does not hand every caller a new
  operation version. It is refused before the database is opened at all, so a
  bundle nobody trusts leaves no lock, no account folder and no empty database
  behind.

  **It installs all of the bundle or none of it.** There is no uninstall, so a
  half-installed bundle is a database holding collections whose operations
  were never declared, with no way back — and it is the one state that cannot
  be recovered by running the command again. One transaction, one `Commit` at
  the end, and an explicit `Rollback` on any refusal so that a caller holding
  the same open store is not handed declarations that will never be committed.
  Measured both ways: a copy that commits per collection leaves the first one
  behind, and a copy with the `Rollback` removed leaves it in the open store.

  **The change log records the bundle and the key, not the account.** Every
  entry an install writes carries
  `bundle "name" "version" signed by <64 hex>, trusted here as "label"`. This
  is the only record anywhere that a declaration came from outside — what a
  bundle installs is afterwards indistinguishable from one written by hand —
  and the key is the one fact on this path that anything actually checked. The
  key is written in full, because a truncated key cannot be compared against a
  trust list. `store.ErrIncompatible` — two bundles fighting over a collection
  name, which is settled here and not at verification — arrives wrapped with
  `%w` and intact, so `internal/server`'s `codeFor` still names it
  `incompatible` rather than `failed`.

- **An author can now produce a bundle without writing Go (SAPE-12).** SAPE-28
  shipped `verify` and `install` and ended its own report with the gap: nothing
  in the shipped binary could *produce* a bundle, so an author outside this
  repository had to write Go against `bundle.Seal`. A feature with a consumer
  and no producer is a feature nobody can use. These are the two commands that
  close it, and neither opens a database, needs `SAPEDB_SECRET` or touches the
  network — an author signing a release is usually not an operator of anything.

  **`sapedb keygen FILE`** makes an ed25519 identity. The private half goes
  into `FILE`, created with `O_EXCL` and mode `0600`; the public half is
  printed on stdout, one line, 64 lower-case hex characters — which is exactly
  what `SAPEDB_TRUST` takes, so
  `SAPEDB_TRUST="acme-eng=$(sapedb keygen acme.key)"` is the whole of getting a
  new author onto an operator's list. The private key is **never** printed: a
  command that writes one to stdout writes it into the scrollback of every
  author who forgets the redirect and into the log of every pipeline that runs
  it, silently, because the key still works afterwards. A path that is already
  taken is refused rather than overwritten — a signing identity that gets
  clobbered cannot be recovered, and every bundle it ever signed stops being
  attributable to anybody.

  **`sapedb seal DRAFT FILE`** signs a draft into a bundle, reading the private
  key from **stdin**. Not an argument (this package's existing rule: argv is
  visible to anyone who can run `ps`) and not the environment; stdin means the
  key need never exist on disk at all —
  `vault read -field=key … | sapedb seal orders.draft.json orders.bundle` is a
  complete release step. `< acme.key` is the simple case of the same thing.

  **A draft is a bundle without its signature, and no second declaration
  syntax was invented.** It is the `{"collections": …, "operations": …}` that
  `sapedb apply` already reads, with `name`, `version` and `signer` in front of
  it, which is what a bundle was always defined to be. An author who already
  deploys with `apply` adds three strings to the file they have. It is read
  through `bundle.Parse`, so a draft is held to the same rule a bundle is on
  the way in — unknown fields refused, one document per file — because a field
  this version does not read is a field the author believes is doing something.
  A draft claiming some other `format`, and a draft that already carries a
  signature, are both refused rather than quietly relabelled or re-signed.

  `bundle.PublicKeyText`, `bundle.PrivateKeyText` and `bundle.ParsePrivateKey`
  are the one place a key is written down, so the command that produces a key
  and the parser that reads a trust list cannot drift into two spellings.
  `ParsePrivateKey` trims surrounding whitespace (a key out of a file or a pipe
  carries a newline nobody typed) and quotes **nothing** back in its refusal,
  unlike every other parser here: a private key that failed to parse is still a
  private key, and the usual courtesy of showing what was read would put a live
  secret in a terminal.

  Measured end to end through the real commands, not the functions: keygen →
  seal → `SAPEDB_TRUST` → `verify` reporting `trusted yes` → `install` → `ls`
  reading the operations back, plus the change log naming the bundle and the
  key. Both negative halves are in the same file — a bundle sealed with a
  different key is refused as untrusted while the signature still checks out,
  and a bundle edited after sealing (its version, and separately an
  operation's `limit`) is refused as not intact. Every key in those tests is
  generated at run time by the command under test; there is not one key
  literal in this repository.

  Not built, and deliberately: key rotation, revocation, a registry, and
  anything over the wire.

- **`sapedbd` reloads part of its configuration on SIGHUP, and says exactly
  what it did (SAPE-7).** `SAPEDB_SHUTDOWN` and a rotated
  `SAPEDB_TLS_CERT`/`SAPEDB_TLS_KEY` pair take effect without a restart, with
  no dropped connections. Everything else the environment can set —
  `SAPEDB_ADDR`, `SAPEDB_DIR`, `SAPEDB_SECRET`, `SAPEDB_LABEL`,
  `SAPEDB_INSECURE`, `SAPEDB_ENCRYPT`, `SAPEDB_FOLLOW` — is refused by name
  with a reason, never silently kept: most of it because the secret is
  checked on every request and also derives the at-rest encryption key, so
  changing it live would invalidate every open connection and grant at once.

  The reload is all-or-nothing. Every reloadable setting is validated before
  any of them is touched, so a configuration where one of several changed
  entries is invalid applies none of them — the running server is left
  exactly as it was, with the rejected entry and its reason in the report.
  `internal/service.Service.Reload` and `.ReloadFromEnv` are the entry
  points; `RunWithReload` wires a signal channel to them for `sapedbd`.

- **An operation's cost envelope is now readable without running it
  (SAPE-8).** `Store.Envelope(name, version)` and every entry of
  `Catalogue.Envelopes` (from `WhatIsHere`) answer, from the declaration
  alone: which collections it touches, which indexes it walks, the most rows
  it may hand back, and which fields escape to the caller — for a composed
  operation too, without opening the operations its steps name by hand.

  The row ceiling reuses `compose.go`'s existing `ceiling()`, the same number
  already checked against a composed operation's declared limit at declare
  time, so the envelope cannot disagree with what running it actually costs.
  A standalone count is the one exception: its declared `Limit` (how far it
  walks) is reported directly, since `ceiling()`'s own "1" answers a
  different question (what a count contributes to an enclosing batch's sum,
  a shape a count can never actually appear in — it is refused as a callee).

  This also closes a real gap `Operation.Limit`'s own `json:"limit,omitempty"`
  left open: that field is absent from the wire — reading as indistinguishable
  from "unlimited" — for every action whose ceiling is not a number it writes
  down itself (a get, a write, a plain batch of them). The envelope's `Limit`
  is never absent, for any action.

### Breaking

- **A grant now expires, and the signed message changed shape to say so
  (ISS-11).** Every grant minted by any earlier build is refused by this one,
  and there is no migration and no dual-format verifier. Nothing has been
  tagged, so this costs nothing today and could not be done at all after a
  tag: adding a field to a signed message is a break, and the three clients
  that carry grants would then have had to choose between refusing 1.0.0's
  grants and accepting a message with no expiry in it — which is exactly the
  downgrade an attacker would pick.

  A grant used to say three things: these scopes, this account, this database.
  It now says five. The two new fields are **`exp`**, the moment the grant
  stops being one, and **`serial`**, a name for this particular grant. Both
  are mandatory, both are inside the signature, and neither is optional or
  "checked when present" — there is one message shape and one branch that
  reads it.

  `serial` is carried and signed and **nothing consults it**. It is here now
  because adding a field to a signed message after a tag is a break and
  adding one before a tag is an edit; the revocation list it exists for can
  wait, the field could not.

  **The signed message is length-prefixed instead of delimiter-joined.** It
  was `account_id ":" dbname ":" scope[,scope...]`. It is now:

  ```
  "sapedb/scopes:v2" "\n"
  len(account_id) ":" account_id "\n"
  len(dbname)     ":" dbname     "\n"
  len(scope_list) ":" scope_list "\n"
  len(exp)        ":" exp        "\n"
  len(serial)     ":" serial     "\n"
  ```

  Every length is the field's length in **bytes**, decimal, no leading zeros.
  `scope_list` is the scopes sorted, de-duplicated and joined with `,` — the
  same canonical form as before. `exp` is whole Unix seconds UTC in decimal.
  The first line is `signing.GrantLabel` itself, which is also the key
  derivation label: one version string rather than two that can drift apart.

  Why count the bytes rather than add two more colons. The old message was
  unambiguous, but only by an argument about three particular fields —
  `account_id` and `dbname` may not contain `:`, so the first two colons are
  fixed and the scope list is the whole of the tail. That argument is not a
  property of the format and has to be remade every time a field is added.
  Made again for five fields it fails: a scope may contain `:` and so may a
  serial, so `account ":" dbname ":" scopes ":" exp ":" serial` writes the
  identical string `acme:main:x:100:200:z` for both of

  - scopes `["x"]`, exp `100`, serial `"200:z"`, and
  - scopes `["x:100"]`, exp `200`, serial `"z"`

  which is one signature over two different grants — a forgery primitive, not
  a formatting nit. A count in front of every field leaves nothing for a
  delimiter to be mistaken for, and a sixth field can be added without
  reopening the question. Measured both ways: over 92,928 tuples built from an
  alphabet of straddling strings the new encoding produces no collision, and
  the same search over the colon-joined form produces sixteen. The same search
  over the *old* three-field form produces none, so the format being replaced
  was not itself ambiguous — it simply had no room to grow.

  **On the wire** the `grant` object on the `invoke` payload gains two fields:

  ```json
  {"grant": {"scopes": ["articles:read"], "exp": 1789995600,
             "serial": "01K5ZQ9P7B3N4M6R8T0V2W4X6Y", "sig": "…"}}
  ```

  `exp` is a JSON integer and `serial` a string; both are required whenever
  `grant` is present, and neither is `omitempty`. A client that omits them
  sends a zero expiry and an empty serial, which does not verify and is
  refused — there is no older shape for it to be read as. Clients **carry**
  these fields and do not produce them: the signature is made by whoever holds
  the server secret, offline, which is unchanged.

  **An expired grant is refused under its own code, `grant_expired`**, and not
  under `grant`. The two are different conditions with different answers — go
  and get another, against stop asking — and a program told to tell them apart
  by reading the prose is the ISS-21 mistake. `server.ErrGrantExpired` is a
  sentinel of its own and deliberately does not wrap `server.ErrGrant`, so
  `codeFor`'s answer does not depend on the order of two rows in its table.

  **No clock skew allowance.** Verification compares `exp` against the
  verifier's own clock and gives nothing away on either side: a grant is
  refused from `exp` onwards, not after it. Any leeway of *L* seconds is a
  grant that keeps working for *L* seconds after the credential says it
  stopped — invisible in the grant, invisible in the log, and the same for
  every grant on the server. The margin belongs in the number the issuer
  signs, where it is visible and per-grant, and the issuer is the party that
  can size it because it picks the lifetime. Skew is real and is not fatal
  here the way it is for a thirty-second token: a grant is issued offline for
  hours or days, so an issuing clock a minute out shifts the effective expiry
  by a minute. For an issuer to mint something already dead its clock would
  have to be wrong by more than the whole lifetime of the grant, which is a
  broken clock and not skew, and a leeway sized for skew would not save it.

  What an operator with a badly wrong clock sees, said plainly: a server whose
  clock is hours **fast** refuses every grant as expired, including ones
  minted seconds earlier, and the refusal names both the grant's expiry and
  the server's own reading of the clock, so the disagreement is in the message
  rather than left to be guessed at. A server whose clock is hours **slow**
  honours grants past their expiry and says nothing, and cannot do otherwise:
  a verifier that does not know it is slow has nothing to compare itself
  against.

  Signature changes: `signing.Grants` returns an `error` rather than a `bool`,
  because there are now two ways to refuse and a caller has to tell them
  apart, and takes a `time.Time` rather than reading the clock itself, so that
  no caller can verify a grant without having decided what time it is.
  `signing.Held` gains `Expires` and `Serial`. `server.Server.Grant` takes an
  expiry and a serial and defaults neither — an expiry it chose for itself
  would be a policy hidden inside a signature, and a serial it generated would
  be one the issuer could never name in a revocation list.
  `Client.Present(scopes []string, grant string)` becomes
  `Client.Present(grant Grant)`, and `Grant` is the fortieth name on this
  module's public surface: two of the four values are strings, so a
  four-argument form compiles just as happily with the last two the wrong way
  round and would produce a grant that silently never verifies.

  `fixtures/signing.json` — the file the TypeScript and PHP clients check
  themselves against — carried only connection-string triples. It now carries
  a `grant` section as well: the label, the message recipe, and seven vectors
  giving the fields, the **exact message bytes** and the signature, including
  the collision pair above (which must sign differently) and one set written
  twice in different orders (which must sign identically).

  **This does not build revocation.** There is no revocation list, nothing
  reads the serial, and taking back a live grant before its expiry still
  means rotating the server secret and invalidating every connection string on
  that server with it. What changed is that a grant is no longer good forever,
  so the window that has to be lived with is the one the issuer chose rather
  than the life of the secret.

- **The old product name is gone from the on-disk format and from every
  encryption key.** Two things that used to spell the product's former name
  now spell `sapedb`, and both of them decide whether an existing file can be
  opened at all:

  - the eight-byte format tag at the head of every database file, and
  - the three key-derivation labels
    (`sapedb/server:database:v1`, `sapedb/pager:page:v1`,
    `sapedb/pager:key-check:v1`).

  **Every database file written by any earlier build is unreadable by this
  one, and there is no migration.** An unencrypted database and an encrypted
  one fail the same way, at the same place, before any key is tried:

  ```
  $ sapedb -account acme -db main ls
  sapedb/pager: not a sapedb file
  ```

  That is the format tag being checked, and it is deliberately the first
  thing checked. An encrypted database from an earlier build would otherwise
  have failed later and far less clearly — with
  `this database is encrypted and the key does not open it`, about a secret
  that is in fact correct — because its pages are now keyed under a different
  label. Measured both ways: with the tag change in place, an old encrypted
  file is refused as `not a sapedb file`; with only the label change, the
  same file is refused as a wrong key.

  There is nothing to do about an existing file except recreate it. `dump`
  and `restore` do not help, because an earlier build is the only thing that
  can read one, and it cannot write a file this build accepts.

  **Why this is happening now rather than later.** It could only happen
  before the first release, and it has not happened yet: the repository
  carries no tags, its remote carries no tags, and this changelog has never
  had a heading other than `Unreleased`. So no database written by a build
  anybody else ran exists to be broken by it. From 1.0.0 onwards both the tag
  and the three labels are frozen, and moving either is a migration's job.

  The lengths and the layout are untouched: the tag is still eight bytes with
  the same trailing two, the labels keep their `:v1` suffix because the
  derivation scheme itself did not change, and `Format` is still 1. Only the
  name inside them moved.

- **Environment variables under the product's former prefix are no longer
  refused by name.** Both binaries used to stop, and name the variable to set
  instead, when they found one set under the old prefix. That signpost is
  gone along with the prefix it had to spell in order to look for it.

  The consequence is limited to a variable still set under the old name: it
  is now simply not read. A secret set that way gets
  `sapedb: SAPEDB_SECRET is not set` rather than a sentence naming both, and
  a directory set that way falls back to `/var/lib/sapedb` silently, which is
  the case the signpost was originally written for. For the same reason as
  above — there has never been a release — nothing can be set under the old
  prefix by anyone who has run a published build.

  This entry deliberately does not spell the old prefix out. `internal/naming`
  walks every file in the tree, this one included, and the rename is only
  finished when nothing left in it carries that name — a changelog describing
  the removal is not an exception to that, it is the last place that would
  quietly become one.

- **A `count` must now declare a `limit`.** A declaration with
  `"action": "count"` and no positive `"limit"` is refused when it is
  declared — through `sapedb apply`, through the `Declare` frame, and
  through `store.DeclareOperation` itself.

  What will be refused after upgrading:

  ```json
  { "name": "orders.how_many", "collection": "orders", "action": "count",
    "index": "by_customer" }
  ```

  ```
  sapedb/store: the declaration does not make sense: a count must declare how
  far it walks — it hands back a number rather than rows, so the walk is the
  whole of what it costs
  ```

  How to fix it: add a `limit`, and make it the largest number of documents
  this count is allowed to walk over.

  ```json
  { "name": "orders.how_many", "collection": "orders", "action": "count",
    "index": "by_customer", "limit": 100000 }
  ```

  Pick the number deliberately, because it changes an answer rather than only
  refusing one: a count that reaches its limit stops there, reports that
  limit, and sets `truncated` on the result. That behaviour is not new —
  `run()` has stopped a count at its declared limit for as long as counts
  have had one — but a count that had no limit never stopped, so a
  declaration that was silently unbounded and now names a number will start
  answering `limit` with `truncated: true` instead of the true total for any
  stretch larger than that number. A caller that ignores `truncated` reads
  that as the count.

  Why it is being done before v1 rather than after: a rule that refuses a
  declaration cannot be added to a released database engine. Every schema
  already written down would stop applying at whatever version added it, and
  the only kind answer at that point is to not add it — which means the gap
  would have been permanent. `count` was the one action that could be
  declared without declaring its cost: the limit was required of a scan, of a
  rollup read and of a composed batch, and the count fell between them. The
  cost it said nothing about is a full walk of the collection.

  This is a declaration-time rule only. Nothing about how a stored count runs
  changed, no stored declaration is rewritten, and a `count` typed at the
  shell is unaffected — `Explore` fills a limit in for a typed access before
  it validates anything, for a count exactly as it always has for a scan.
  That asymmetry is deliberate and is now measured on both actions
  (`TestACountDeclaredOverTheWireMustSayHowFarItWalks`,
  `internal/server`).

  One refusal disappeared with it: a negative limit on a count used to be
  refused with `a limit of -1`, a sentence from a branch that sat further
  down `validateOperation`. That branch is gone, because the new rule covers
  every limit that is not positive, and a count with `"limit": -1` is now
  refused with the sentence above instead.

### Added

- **An operation bundle can be signed and verified (SAPE-10).** A bundle is a
  set of declarations that came from somebody else. `internal/bundle` is the
  two questions a server has to answer before it stores one — who signed this,
  and is it still the thing they signed — and nothing more. **A bundle cannot
  be installed yet**; SAPE-12 is the worked example that will do it.

  **What a bundle is.** An apply file with a name, an author and a signature
  on it. That was read out of the tree rather than invented: `sapedb apply`
  already reads `{"collections": [Spec...], "operations": [Operation...]}`,
  and those two lists are what an external operation module needs to be. The
  R&D note called the v1 shape "collections, operations, shapes"; there is no
  shape type in this repository, so v1 carries the two that exist.

  **What it deliberately cannot express**: code — no custom action, no
  expression language, no trigger, no plugin, no WASM, so a verified bundle
  can widen what a database holds but not what the server can do, which is why
  signing is enough and sandboxing is not needed; data — it declares shapes
  and leaves them empty; removal — there is no uninstall; namespacing — two
  bundles that both declare `orders` are in a fight that `store.Declare`
  settles later, not one verification catches; dependencies between bundles;
  freshness — nothing compares the version string against anything, so a
  correctly signed old bundle verifies forever; and where it may be installed
  — unlike a grant it names no account and no database, on purpose.

  **ed25519, not the HMAC everything else here uses.** Every existing
  signature in this repository is made with the server's own secret and so
  proves the signer held that secret. A bundle's author is outside; signing it
  with the server's secret would prove only that the server signed a file it
  received. So the author keeps a private key and an operator keeps a list of
  public keys. A verified bundle proves exactly this and no more: *these
  declarations are the ones the holder of this public key signed, and an
  operator of this server wrote that key into its configuration.* It does not
  say who that is in the world — there is no PKI and no name binding, and the
  `signer` field is signed but self-asserted, so `Verify` returns the label
  **this operator** wrote beside the key rather than the name the bundle
  claims about itself. There is no revocation and no rotation in v1. So v1 is
  narrower than "anyone can publish": an operator decides by hand whose
  bundles this server will look at, and an empty list refuses everything.

  **The signed message is length-prefixed and counts its lists**, following
  ISS-11's decision about a grant for the same reason and one more:

  ```
  "sapedb/bundle:v1" "\n"
  len(name)       ":" name       "\n"
  len(version)    ":" version    "\n"
  len(signer)     ":" signer     "\n"
  len(public_key) ":" public_key "\n"
  len(itoa(C))    ":" itoa(C)    "\n"   C = how many collections
    len(json_i)   ":" json_i     "\n"   each collection, in order
  len(itoa(O))    ":" itoa(O)    "\n"   O = how many operations
    len(json_j)   ":" json_j     "\n"   each operation, in order
  ```

  A count in front of every field is what ISS-11 bought, and it stops one
  field running into the next. It does nothing about one *list* running into
  the next: with per-member counts alone, two collections and no operations
  writes what one collection and one operation writes. The count in front of
  each list is what closes that, and it is measured rather than argued —
  `TestNoTwoBundlesShareOneMessage` sweeps 8,192 tuples through this encoding
  and through the delimiter-joined form beside it: **400 collisions in the
  delimiter-joined form, 0 in this one**, with the collided pairs' signatures
  cross-checked so that the test measures forgeries and not formatting.

  **The signature covers declarations, not file bytes.** The file is parsed
  into `store.Spec`/`store.Operation` and re-marshalled, so whitespace,
  permissions and timestamps survive a transfer. Unknown fields are refused
  rather than dropped, as `sapedb apply` already does, and so is a second JSON
  document in the same file. Hex must be lower case — `hex.Decode` reads `AB`
  and `ab` alike, so accepting both would be a byte of the file with two
  spellings. `encoding/json` still folds the case of an object's *key* names,
  which cannot be closed from here; the byte-flip test states that instead of
  not reaching it, and holds those changes to the opposite standard — they
  must still verify. Of 2,114 single-byte changes to a signed bundle, 0 alter
  a declaration and go unnoticed.

  **Six refusal codes, frozen here**: `bundle_unsigned` (nobody signed it),
  `bundle_signature` (signed, then changed), `bundle_untrusted` (intact, but
  by a key this server does not know), `bundle_no_trust` (this server trusts
  nobody, so nothing installs — an empty list fails closed and says so in
  those words), `bundle_key` (a key in an operator's own configuration that is
  not one) and `bundle` (not a bundle this version can read). None is
  reachable from the wire yet, because verification is offline. They are in
  `codeFor` anyway: a code added after a tag is a change every client has to
  cope with, and ISS-21 is this project's record of what an unnamed refusal
  costs.

- **A tag-driven release: cross-platform binaries, checksums, and a workflow
  that builds and publishes them.** Before this, a stranger who wanted to run
  sapedb had to clone the repository and build it from source — there was no
  `.github` directory at all, and `make dist` only ever built for whichever
  single platform it ran on.

  `make dist-cross` now builds `sapedb` and `sapedbd` for four platforms:
  `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` — where this is
  actually deployed (the Dockerfile's own base and runtime are Linux, and both
  architectures are ordinary on cloud VMs now) and where a stranger evaluates
  it without Docker (an Intel or an Apple Silicon laptop). Every binary is
  stamped exactly as `make dist` stamps a host build, through the same
  `internal/build.Path` the Makefile and Dockerfile already share.

  `make checksums` covers every one of those eight artifacts in one
  `checksums.txt`, in the plain format both `sha256sum -c` and
  `shasum -a 256 -c` read.

  `make verify-dist` is the guard against the specific failure ISS-18 named:
  `go build` accepts an `-ldflags -X` for a symbol that does not exist and
  silently does nothing, so a build succeeding is not evidence it stamped
  anything. It re-derives the version from each artifact's own bytes instead
  of trusting the build that produced them — running the one artifact that
  matches the host directly (`sapedb version`, or reading `sapedbd`'s own
  first log line), and grepping the stamped string out of every artifact that
  cannot run on this machine, since `-X` sets a Go string that stripping
  (`-s -w`) does not remove. Checked against a deliberately unstamped binary
  built alongside the real ones: `verify-dist` reported `printed sapedb dev,
  which does not contain v0.1.0-test` and failed, rather than passing quietly.

  `.github/workflows/release.yml` runs all of the above, plus a container
  image build, on every push of a tag matching `v*`, then publishes the
  binaries to a GitHub Release and the image to a registry. The same workflow
  runs from `workflow_dispatch` as a dry run, sharing every build and
  verification step with the real path — including starting the built image
  and connecting to it with the binary this same run just built, checking
  that the welcome's `productVersion` is the stamped version rather than
  `dev` — and stopping short of the four publishing steps, which it never
  reaches. The dry run uploads its binaries, checksums and verification
  output as a workflow artifact instead, private to whoever can already see
  this repository's Actions runs.

  Measured locally at this commit with a synthetic version
  (`v0.1.0-test`, chosen because no tag exists to build from and none was
  created for this): all eight cross-compiled binaries built, `checksums.txt`
  verified clean with `shasum -a 256 -c`, `verify-dist` passed for all eight
  and failed correctly against a deliberately unstamped one, and a container
  built with `--build-arg VERSION=v0.1.0-test` — run, connected to with
  `sapedb shell`, and torn down — answered `connected to sapedb v0.1.0-test`.
  What the workflow file itself does on a real tag push, and against a real
  registry, is not measured here and could not be without doing the thing
  this task exists to not do.

- **`sapedbd` can follow another `sapedbd`.** Set `SAPEDB_FOLLOW` to the
  connection string of a database on another server and this daemon keeps a
  copy of it: it subscribes to that database's change log from wherever its
  own copy has got to, applies every entry as it arrives, and refuses every
  write of its own. `SAPEDB_FOLLOW_INSECURE=1` dials the leader without TLS,
  separately from `SAPEDB_INSECURE`, which is about how this daemon is
  reached. The account and database name come out of the connection string:
  a copy of `acme/main` is served here as `acme/main`.

  One connection string reaches both. The signature covers the account, the
  password and the database name and deliberately not the host, so the same
  string a caller uses against the leader reads from the follower.

  **Where a follower keeps its place: nowhere of its own.** There is no
  cursor file and no marker key beside the data, because two things that must
  agree and are written separately are two things that will one day disagree
  — and the two ways they disagree are the two ways replication goes quietly
  wrong. Applying an entry already writes the log counter (`store.Apply`
  calls `setLSN`, into the same tree, inside the same transaction as the
  document it changed), and one commit makes the change and the number
  durable together or neither of them. So the place a follower resumes from
  is the database's own `LatestLSN`, plus one.

  That removes both halves of the trap rather than balancing them. Crash
  after applying and before saving the position: impossible, they are one
  write. Crash after saving and before applying: impossible, same reason.
  Reconnect and be sent entries already held: `store.Apply` ignores an entry
  at or below what is there.

  Measured on two real `sapedbd` processes
  (`TestAFollowerKilledMidStreamComesBackWithoutAGapOrARepeat`): 602 entries
  on the leader, the follower killed at entry 200 of them, 40 more writes
  while it was down, then the same directory started again. It announced
  `from entry 201`, caught up, and its log was compared to the leader's
  **entry by entry** — every entry present, identical, and the numbering
  contiguous — with the documents then compared field by field on top of
  that.

  What that measurement is worth was checked by breaking it four ways, each
  in a copy of the tree. A follower that resumes one entry too far never
  applies anything (`store.Apply` refuses the out-of-order entry) and the
  test times out; one that resumes from entry 1 every time is caught by the
  line it announces; one that drops an entry — with `store.Apply`'s ordering
  guard removed as well, or nothing would have let it — is caught by name:
  `entry 300 is on the leader and not on the follower`; and a follower
  without the write gate is caught accepting a write.

  Two other mutants were **equivalent**, which is worth writing down because
  each looks like a defect and is not. Applying every entry twice changes
  nothing: every arm of `store.Apply` is idempotent, which is what the log's
  own comment has always claimed ("applying one twice is the same as applying
  it once"). And making `Apply` record a change of its own on the put arm
  also changes nothing here, because the number it mints is the number the
  entry already had — `takeLSN` returns `latest+1`, which is exactly the
  entry being applied — and `Apply` overwrites the log entry with the
  leader's verbatim afterwards. Neither is a reason to relax either guard;
  they are the reasons "a repeat" is not a failure this can produce.

- **A server can be read-only, and a follower is one.** `server.Options.ReadOnly`
  refuses every request that would put an entry in the change log, with a
  sentinel of its own (`server.ErrReadOnly`) and a code of its own
  (`read_only`), because the caller's answer to it is specific: go and write
  to the leader.

  **What counts as a write is wider than it looks**, and this is the part to
  read before pointing a tool at a follower. Reading the catalogue
  (`WhatIsHere`) and running an operator's typed access (`Explore`) both
  record a `ChangeRead` entry — deliberately, so that an audit trail does not
  stop at the primary — and an entry is an entry. **Both are refused on a
  follower**, which costs a follower its operator shell: `sapedb shell`
  against one cannot so much as list the collections. Invoking a declared
  operation that only reads is not refused, because it records nothing, and
  that is how anything reads a follower today.

  A write accepted on a follower would be worse than a write lost. It would
  mint an entry number of its own, and from that moment the two sides'
  numbers mean different things: the next entry the leader sends is refused
  as out of order and the copy stops following. So being a follower and
  refusing writes are one switch rather than two, set together in
  `internal/service`.

  **Out of scope, and none of it is an oversight.** Replication is
  asynchronous: the leader does not wait for a follower and does not know
  whether one is keeping up. A follower never promotes itself. Nothing
  merges, because a follower takes no writes and there is no conflict to
  resolve. Nothing measures lag. Several followers of one leader do not know
  about each other. A daemon follows one database, not a list.

  **Known debt.** A follower commits once per entry it applies, so a batch
  of 512 entries off the leader's feed is 512 commits and 512 syncs here.
  Nothing has needed it to be faster yet. And `too_far_behind` — a follower
  away longer than the leader keeps its log — is handled (`follow.Run`
  returns rather than reconnecting into a refusal for ever) but **cannot be
  produced against `sapedbd` at all today**, because no server option
  reaches `store.Retain` and so a daemon's log is never trimmed. That path is
  covered only by the in-process test that sets the cap itself
  (`TestASubscriptionFromAnEntryThatWasTrimmedIsRefused`).

- **A client can read the change feed.** `Client.Subscribe(from)` and
  `Client.NextChange()`, on `internal/wire` and on the public `sapedb`
  surface, with `Change`, `Attribution` and `Following` alongside them.

  The server has carried `Subscribe` and `Event` frames since before the
  public package existed, with a handler (`internal/server/subscribe.go`) and
  tests of its own; the engine has been able to apply an entry for as long
  (`store.Apply`, and `internal/store`'s replica tests). Neither end was
  wrong. What did not exist anywhere was the join: the word `Subscribe`
  appeared zero times in `internal/wire` and zero times in `sapedb.go`, so no
  client in this repository could read an `Event` frame at all, and nothing
  had ever turned one back into a change and applied it. A feed nothing can
  read is a feed nobody has proved is a feed.

  A connection that subscribes carries the subscription and serves no other
  request. That is the honest shape of this client rather than a limitation
  invented for the occasion: everything else here is one request and one
  answer read straight off the socket, and a subscription puts frames on that
  socket which nothing asked for — so a request issued alongside one reads an
  event where it expects its answer. It is refused by name, because the
  alternative is what the mutation run printed with the guard removed:
  `asked 5 and was answered 4`, which is the same fault described as an
  accident. A caller that wants both opens two connections.

  `NextChange` blocks with no deadline, whatever `Options.RequestTimeout`
  says. A subscription that has caught up is waiting for somebody to write,
  and on a quiet database that is not a fault; a deadline there would turn
  "nothing is happening" into an error and teach every follower to ignore it.

  Entries are numbered from 1, and `from` is the first entry wanted — so 1
  asks for everything there has ever been. **0 is refused**, as
  `too_far_behind`, which is the same answer an entry that has been trimmed
  gets. That is deliberate and worth knowing before writing a follower: a
  consumer quietly started somewhere other than where it asked is one that
  believes it has seen changes it has not.

  Measured end to end on a real `sapedbd` process
  (`TestAChangeStreamedFromARunningDaemonRebuildsTheDatabase`): a collection
  seeded before the daemon starts, an operation and three documents written
  before anybody follows, then — while a second connection is following — a
  collection established over the wire, four operations declared (one twice,
  so an older version has to travel), and three inserts, an update and a
  delete. Every change that arrives is applied into a blank engine in the test
  process. The daemon is then stopped, its own file opened, and the two
  databases compared **document by document, field by field, and declaration
  by declaration, including every operation version** — never by counting,
  because a document that arrived missing a field is still one document on
  each side. The comparison has its own control,
  `TestTheComparisonSeesADocumentThatLostAField`, which shows it failing on
  exactly that difference while a count of documents agrees.

  The public surface is now **39 names**, up from 34: five are this change
  (`Subscribe`, `NextChange`, `Change`, `Attribution`, `Following`).

- **Nothing in the product sets a retention cap.** Not new, and recorded here
  because reading a feed is what makes it matter: `store.Retain` exists, no
  server option reaches it, and so a daemon's log is never trimmed. The
  `too_far_behind` refusal — the one thing that tells a follower it has fallen
  too far behind to catch up and must be rebuilt from a dump — is therefore
  unreachable against `sapedbd` today except by asking for entry 0. It is
  reachable by an embedder who sets the cap, which is what
  `TestASubscriptionFromAnEntryThatWasTrimmedIsRefused` does, on a server in
  the test process rather than a separate one. A cap that nobody can set is a
  disk that fills up while everything looks healthy; wiring one to a server
  option is not done here.

- **A collection can be declared on a server that is already running.** The
  `Declare` frame could put an *operation* on a live daemon; there was no way
  to put a *collection* there. So adding one still meant stopping the server
  and running `sapedb apply` — that command opens the database file directly
  and takes the exclusive lock, which is refused while the server is up — and
  every live connection dropped for a change that writes one key and builds
  one index.

  Half of a hole is worse than the whole of one here, because of what the
  missing half blocks. An external operation that solves a whole problem
  through the shapes it publishes is not one operation: it is a module, and a
  module arrives with somewhere to keep its data (a collection, its indexes,
  its rollups), the vocabulary to reach it (operations) and the shapes it
  publishes. With only the operation half declarable over a connection, such
  a module could not be installed into an empty database at all — it could
  only ever be a handful of operations pointed at somebody else's
  collections.

  **New frame: `establish`, type 14.** Frames 1 to 13 do not change what they
  are, so this is a new code rather than a field added to `declare`: a frame
  whose meaning depended on which of two fields was set would be exactly that
  change, dressed as an addition. `fixtures/frames.json` gains the type and
  one case, which is a change the TypeScript client's own fixture test will
  see. The name avoids "declare" on purpose — in `internal/store`, `Declare`
  is the *collection* one and `DeclareOperation` is the *operation* one,
  while frame 13 named `declare` carries the operation, and doubling that
  crossed pair on the wire would help nobody.

  It is not a second, looser way in. Only an operator may send it, proved the
  same way `explore` and `declare` are — by answering the welcome's challenge
  with the server's own secret. It calls `store.Declare`, which is the
  function `sapedb apply` calls, which begins with `spec.validate()`; there
  is no second copy of the rules and nothing is normalised on the way in.
  That is measured rather than asserted:
  `TestEstablishingOverTheWireRefusesExactlyWhatApplyRefuses` runs nineteen
  declarations down **both** paths against **one** database and demands the
  same sentence, word for word — including the three refusals only a
  redeclaration can reach, which `validate` cannot see, so a handler that
  validated and stopped would pass sixteen rows and fail those three. Three
  of the nineteen must be **accepted** by both paths, and the test fails if
  that count is not three, because a table of nothing but refusals measures
  nothing.

  **Declaring a name that already exists is not a new version.** This is
  where `establish` and `declare` part company, and the reason is physical
  rather than a matter of taste. An operation is versioned because a caller
  is built against a version and a redeclaration must not move the ground
  under it. A collection is *where the documents are*, and there is one of
  those: two versions would be two sets of index entries over one set of
  documents, or two sets of documents, and either way the next writer has to
  be told which it meant through an interface that has never carried a
  version. So establishing a name that exists brings **that** collection up
  to date, in place — which is `store.Declare`'s own long-standing behaviour,
  not something this path invents. Indexes and rollups the declaration names
  are added (built over the documents already stored) or kept with the ids
  they already had; ones it leaves out are dropped with their entries; and
  what cannot be done in place — the primary key, how the collection is
  divided, an index that keeps its name and changes its shape — is refused
  in the store's own words rather than done quietly.

  The answer is the collection as it now stands, read off the store rather
  than echoed back, so a caller sees what actually took effect: the ids the
  store assigned, and the index it did not mention missing from the list.

  `Client.Establish` is the method on the public surface (the guard there now
  counts thirty-four names, not thirty-three).

- **The change log says who declared or dropped a collection.** Three
  `record()` calls in `internal/store/store.go` carried no attribution at
  all: the first declaration of a collection, the redeclaration that upgrades
  one, and the drop that destroys one. All three are fixed —
  `Store.Declare` and `Store.Drop` now take a `Caller` the way
  `Store.DeclareOperation` already did, and the entries go in under operation
  `declare` and `drop` with that caller's actor.

  The drop was the worst of the three and not by a little. A declaration can
  be read back off the collection that now exists, so its entry is a
  convenience; a drop leaves nothing behind to ask, and the entry that did
  not say who removed it was the only record that the collection had ever
  been there.

  What the name is worth differs by path and neither path pretends otherwise:
  over the wire it is the account whose connection the server itself
  verified, through `sapedb apply` it is the account the command was pointed
  at — a label rather than a proof, since running apply means holding the
  file's exclusive lock. An embedder that passes `Caller{}` gets an entry
  that says so, which is measured as the contrast row in every one of these
  tests rather than assumed.

- **A build can say which build it is.** Until now it could not, and the
  measurement is the whole point: two binaries compiled six months apart
  introduced themselves to a client with byte-identical words. The `version`
  field in the welcome is the **protocol** version — `protocol.Version`, the
  constant `1`, what a frame's layout is written in — and it has not moved
  since there was a frame, nor should it. There was no `version` command in
  `cmd/sapedb` at all. So a bug report could name what the database did and
  never what did it, and an operator holding a binary had no way to ask it.

  Three things now answer, and they all answer with the same value:

  - `sapedb version` prints it. It is the only command in that tool that
    names no database, so it runs before the secret, the account and the name
    are required — which is the state anyone asking the question is usually
    in.
  - `sapedbd` says it on the first line of its log, since a daemon has no
    subcommand to ask and is usually a container nobody has a shell into.
  - the **welcome carries it as `productVersion`**, so any connected client
    reads it at the handshake, with nothing to ask for and nothing to prove
    first. `sapedb shell` prints it as `connected to sapedb <version>`.

  The value is written in by the linker (`-X
  github.com/sapedb/sapedb/internal/build.Version=...`), from the tag being
  built; `make dist` and the Dockerfile do it, and `make image` passes it
  through as a build argument. A binary nobody stamped says `dev` — a word no
  release will be called, so an unstamped build is recognisable as one instead
  of claiming a number.

  **This is a wire change, and it is an added field only.** Nothing is renamed
  and nothing is removed. A client compiled against the old welcome ignores
  `productVersion` and reads everything it came for —
  `TestAClientThatPredatesTheProductVersionStillReadsTheWelcome` decodes a real
  welcome into a hand-written copy of the old struct and checks the protocol
  version, the account, the name, the mode and the challenge all still arrive.
  `fixtures/frames.json` is unchanged: it carries `welcome` in its type table
  but has never had a case with a welcome body in it, so there was nothing
  there to update. **The client fixture in the TypeScript repository therefore
  needs no change either** — but a client that wants to read the new field
  needs it added to its own welcome type.

  Not included, deliberately: nothing checks for a newer version and nothing
  updates itself. Asking what this is and going to fetch another one are
  different jobs, and the second one is not a job a database server should do.

- **Scopes can be reached over the wire.** An operation has been able to
  declare `scopes` for as long as there have been operations, and
  `store.allowed` has checked them fail-closed the whole time — but nothing on
  the wire could ever present one. `internal/server` built its `store.Caller`
  with no `Scopes` at all, over a comment explaining that a caller naming its
  own permissions has named nothing. The comment was right about the danger and
  the consequence was that an operation declaring a scope was an operation
  **nobody could call**: a field that refused everything and permitted nothing,
  which is not a permission system but a way of disabling an operation by
  mentioning a word. It also meant the check had never once run end to end, and
  a security mechanism nothing exercises is one nobody can say works.

  What a caller sends now is not a list of scopes. It is a list **and a
  signature over it**, made with a key derived from the server's own secret —
  the same secret that signs connection strings, under a label of its own
  (`signing.GrantLabel`) so neither can ever be presented as the other. The
  message signed is `account_id ":" dbname ":" scope[,scope...]`, sorted and
  de-duplicated, so:

  - a caller cannot mint one, for exactly the reason it cannot mint a
    connection string: it does not have the secret;
  - a caller cannot edit the list under a real signature, because the list is
    inside the message;
  - a grant minted for one database cannot be presented at another, and one
    minted for one account cannot be presented by another, even on the same
    server under the same secret.

  On the wire it is an optional `grant` object on the `invoke` payload; it
  carried `{"scopes": [...], "sig": "..."}` when this entry was written and
  carries two more fields now (see *A grant now expires*). Sending none is
  sending no scopes, so
  **every existing client keeps working unchanged** and keeps exactly the
  permissions it had. A grant that does not verify refuses the call outright,
  with the code `grant` rather than `not_allowed`: a bad credential is a
  different thing from a missing permission, and a caller whose grant was
  issued for the wrong database must not be left reading "you need
  articles:read" about a grant that says `articles:read`.

  New names: `signing.Held`, `signing.Granting`, `signing.Grants`,
  `signing.GrantLabel`, `signing.ErrScope`; `server.Server.Grant` (which sits
  next to `Sign` — issuing a grant is the secret holder's job, the same as
  issuing a connection string, and nothing on the wire reaches it); and
  `Client.Present` on this module's public client, which takes the surface
  from 32 names to 33. (`Client.Present` has since changed signature and the
  surface has since grown again — see *A grant now expires*.)

  Deliberately **not** a challenge-response like the operator proof. A nonce
  would stop a grant being replayed and would also require whoever issues
  grants to be online at the moment of every connection, turning an offline
  control plane into a request path. A grant is a bearer credential of exactly
  the same weight as the connection string it travels beside: whoever holds
  that string can already reach the database, and the grant says what they may
  do once there.

  Known limits, written here rather than left to be discovered: a scope may
  contain `:` (every scope this product uses does) and may not contain `,`.

  This entry once ended by saying that a grant had **no expiry and no serial
  number**, so it was good until the secret changed. That is no longer true
  and the sentence has been corrected rather than left standing with a
  contradiction added below it — see *A grant now expires* under Breaking.
  What is still true is the second half of it: **there is no revocation.**
  Nothing keeps a list of withdrawn grants and nothing consults the serial, so
  taking a live grant back before its expiry still means rotating the server
  secret, which invalidates every connection string on that server too.

  With this, the scope-union rule composed operations landed with is measured
  end to end for the first time. In-process it was already true that a parent
  must ask for every scope its callees ask for; over a socket, the other half
  of that sentence — "and then a caller that does not hold it is refused at
  call time" — had never been observed, because nothing could hold one. Both
  halves now run on one connection in
  `TestComposingIsNotAWayRoundAScopeOverTheWire` (`internal/server`).

- Composed operations: a step of a batch may name an operation that is already
  declared, instead of a collection. `Step` gains `operation`, `version` and
  `with`; a step does one or the other and never both, and writing both is
  refused when it is declared.

  It exists to answer one objection without answering it with a query
  language. "You are missing a `scan-with-total`, when do I get one?" — never;
  you compose it out of two declarations you already have:

  ```
  operation "catalog.page", action batch, limit 51
    input:  shelf (string)
    step "items":  operation "items.on_shelf@1"      with { shelf }   -- scan,   ceiling 50
    step "total":  operation "items.total_on_shelf@1" with { shelf }  -- totals, ceiling 1
  ```

  Five refusals hold it up, all of them made when the operation is declared
  and none of them a check made while it runs:

  - the reference pins a version, and that version already exists. Version
    zero means "the latest" everywhere else in the package, and that is the
    one meaning it may not have here: a reference that followed the newest
    would make the limit printed above stop being true the moment somebody
    else redeclared `items.on_shelf`, and nobody would be told. Pinning is
    also what makes recursion impossible to write down rather than something
    that has to be detected — a version is only ever handed out going up, and
    a pinned reference only resolves backwards, so the reference graph is a
    DAG by construction and there is no maximum depth.
  - the callee is itself a declaration this store validated when it was made,
    so its own ceiling is already true of it.
  - the callee's ceiling is readable from its declaration at all.
  - a term that names another step may only name one whose ceiling is 1. This
    is the rule that decides what this feature is: taking a value from a step
    that may hand back many rows is "run this once for each of them", which is
    a for loop written in JSON.
  - a composed operation declares a limit, and the ceilings of its steps add
    up to no more than that limit.

  That last one is what makes a declaration readable by a person. By induction
  over the declarations it names, the ceiling of a composed operation **at any
  depth is the number written in that operation** — not a product, and not a
  sum the reader has to work out: the same one number a flat scan declares,
  meaning the same thing. Measured rather than argued by
  `TestTheCeilingOfAComposedOperationAtAnyDepthIsTheLimitItDeclares`
  (`internal/store/compose_test.go`), which builds a tower three levels deep
  where a fan-out reading would be 50 to the eighth power and the ceiling is
  the 400 the top declares.

  What this buys is **atomicity** and a **composable** vocabulary, and it
  saves **K−1 round trips** for a composed operation of K steps — a count read
  off the declaration. It is not sold as faster and nothing here says it is:
  task 0069 measured both places a saving could come from, and one of them
  (fsync) a batch already saves with nothing further for composing to take,
  while the other came to 0.099–0.131 ms on loopback, which is the size of the
  difference between two runs of the same measurement. Nobody has measured it
  across a real network. Until somebody has, the words are atomic, composable
  and fewer round trips.

  What it does *not* do, written here rather than discovered: rows from every
  step land in one flat `Result.Rows` in step order and nothing in the list
  says which step a row came from, which is what a batch of plain steps has
  always done. A `count` may not be a step — its answer is a number and a
  composed answer has one `Count` field for its rows, so the number would be
  paid for and dropped; read a `totals`, whose answer is a row. Scopes are the
  union and are checked when the operation is declared: `allowed()` only ever
  sees the operation a caller named, so without that rule composing would be a
  way round it.

  End to end on a running daemon, over one connection that was never dropped:
  `TestAComposedOperationIsDeclaredOnARunningDaemonAndAnsweredInOneCall`
  (`declare_live_test.go`).

  **The TypeScript client's `Step` does not carry the three new fields.**
  `fixtures/frames.json` is unchanged — no fixture case carries a step — but
  `@ecosy/sapedb` cannot build or read a composed declaration until its own
  `Step` is widened, and nothing compares the two.

- A `Declare` frame (type 13) on the protocol: an operation can now be
  declared on a database that is already being served, without stopping the
  server. Before it there was no path at all —
  `grep -rn "Declare" internal/protocol/ internal/server/server.go` came back
  empty, and `store.DeclareOperation` could only be reached through
  `sapedb apply`, which opens the file directly and takes the exclusive lock
  (`internal/cli`'s own package doc: "a command run while the server is up is
  refused"). Creating an operation meant stopping the server, applying and
  starting it again, which dropped every live connection for a change that
  writes one key.

  Refused on a connection that has not answered the welcome's challenge with
  the server's own secret, exactly as `Explore` is, and reported with the
  same `not_operator` code. It calls `store.DeclareOperation` — the same
  function `sapedb apply` calls, so the same `validateOperation` — and
  normalises nothing on the way in. That last part is the point:
  `internal/store`'s `Explore` claims to check a typed access "with the same
  validation a declaration gets", and for the limit rule that has never been
  true, because `asOperation` forces `Limit` to `MostRows` when it is missing
  *before* `validateOperation` runs, so "a scan must declare how many rows it
  may return" can never fire through `Explore`. It does fire through
  `Declare`. Measured by `TestDeclaringOverTheWireRefusesExactlyWhatApply
  Refuses` (`internal/cli/declare_apply_test.go`), which runs one table of
  fourteen declarations down both `apply()` and the wire against the same
  database and demands the same sentence from each, and by
  `TestAScanDeclaredOverTheWireMustSayHowManyRowsItMayReturn`
  (`internal/server/declare_test.go`), which pins the `Declare`/`Explore`
  asymmetry side by side.

  It takes the database's write lock (`Lock`, not `RLock`) and commits, and a
  name that is already declared gets a new version with every older one left
  readable — `store.DeclareOperation`'s own behaviour, unchanged. The
  end-to-end case is `TestAnOperationIsDeclaredOnARunningDaemonAndCalledOnThe
  SameConnection` at the module root: it builds `cmd/sapedbd`, starts it,
  declares an insert and a scan over one socket, calls both, redeclares the
  scan, and ends by checking the daemon is the same pid, never signalled.

  `fixtures/frames.json` gains `"declare": 13` and one case. **That file is
  kept byte-identical in the TypeScript client's repository and nothing
  compares the two copies — the change has to be carried across by hand.**
  `TestTheDeclareFixtureIsTheStructTheServerDecodes` now decodes the case
  with the server's real `declaring` struct and `DisallowUnknownFields`, so
  at least this side cannot drift from its own fixture in silence.

- `Client.Declare` on the public `sapedb` package and on `internal/wire`'s
  client.

- `Client.InvokeVersion(name, version, args)`, on the public `sapedb` package
  and on `internal/wire`'s client. The wire has carried a version since long
  before this — `internal/server`'s `call` struct decodes it and
  `store.Store.Invoke` takes it — and nothing in either client could set one,
  so every version a redeclaration left behind was stored, readable and
  runnable by the engine and unreachable by anybody holding the published
  package. Zero means the newest, which is what `Invoke` asks for; the field
  is left off the request entirely when it is zero, matching the `omitempty`
  on the server's struct and on the TypeScript client's request body.

  It is a second method rather than a third parameter, because `Invoke`'s
  signature is part of the surface published above and taking it back would
  break every caller for a field almost none of them pass. A variadic
  `version ...int` would have kept source compatibility and cost more than it
  saved — `Invoke(name, args, 1, 2, 3)` compiles, and the documentation would
  read as something the method is not. Measured by
  `TestAnOlderVersionIsStillCallableThroughThePublicPackage`
  (`declare_live_test.go`), which declares three versions of one scan on a
  live daemon and tells them apart by row count.

- The module-root surface is 32 names now, not 30, and
  `TestTheSurfaceIsExactlyTheseThirtyNames` is renamed twice over to match
  the number it guards.

- A public Go package, `sapedb` (`github.com/sapedb/sapedb`), at the module
  root: task 0068 §1's answer to "what does a Go client outside this
  repository import". Twenty type aliases for the value shapes a caller
  sends and receives (`Access`, `Bound`, `Catalogue`, `Condition`,
  `Endpoint`, `Field`, `Index`, `Key`, `Operation`, `Parameter`,
  `Partition`, `Result`, `Rollup`, `Spec`, `Step`, `Term`, `Connection`,
  `Refused`, `Welcome`, `Options`), `Parse` and `Dial`, a `Client` that
  wraps `internal/wire`'s (kept a wrapper rather than an alias so its
  methods' documented return types are this package's own, not an
  unlinkable `internal/` path), and `Explored`. `internal/store`,
  `internal/wire` and `internal/connection` stay exactly where they are —
  nothing moved out of `internal/`, only aliased or wrapped. Guarded by
  `TestTheSurfaceIsExactlyTheseThirtyNames` in `sapedb_test.go`, which reads
  every `*.go` file at the module root and fails on any exported name added,
  removed or renamed.

- `internal/server`'s per-database lock is now a `sync.RWMutex` instead of a
  `sync.Mutex` (task 0070 §1). A call still has to look its operation up
  before anyone can say whether it reads or writes, and that lookup is itself
  a read of the declaration tree — so `invoke()` takes the read lock first,
  asks `Store.SharedRead` what it found and whether this call may share, and
  either runs it right there (still under the read lock, via the new
  `Store.Run`, which performs an operation `SharedRead` already looked up and
  allowed — added so a call that shares does not pay for the lookup twice) or
  drops to the write lock and re-invokes normally. Sharing needs both: the
  action must not write (`writes()` already decided that), and the
  collection must not be partitioned — a `get`/`scan` against a partitioned
  collection reaches `Collection.into`, which can open a partition file for
  the first time (mutating the store's open-partition map) and run expiry
  (which can unlink one), so it is a write wearing a read's name. Dropping
  that second check reproduces a real `-race` failure on
  `Store.Part`'s open-partition map — kept as a mutant during measurement,
  not part of this repo. Guarded by `TestReadsRunTogether`
  (`internal/server/parallel_reads_test.go`), which times whether N
  concurrent reads of one database cost N times one read's time (queueing)
  or close to one read's time (sharing) over a real socket, and
  `TestPartitionedReadsAreWrites` (`internal/server/partition_reads_test.go`),
  which is the `-race` positive case for the partition check above. See
  README.md's "Every write is a transaction" paragraph for what this does
  and does not change about read/write ordering.

- `wire.Options.RequestTimeout` (task 0070 §2): `Options.Timeout` only ever
  bounded the TCP/TLS dial (`net.DialTimeout` / `tls.DialWithDialer`) — once
  connected, nothing in `internal/wire` called `SetReadDeadline` or used a
  context, so a peer that accepted a connection and then never answered (a
  stuck server, or one just built wrong) hung every request forever,
  handshake included. Because a session on the server side holds its
  database's lock for the duration of a call, one such client could pin a
  whole database. `RequestTimeout`, when set, is applied with
  `net.Conn.SetDeadline` around each request's send-and-read pair
  (`Client.request`, `internal/wire/wire.go`) — both directions, not only the
  read, because a peer that stops draining what this client writes can wedge
  the write half of a round trip the same way a silent peer wedges the read
  half. Default is `0`, meaning no deadline is set — the exact hang this
  closes is opt-in to close, not opt-out: a caller doing a legitimate
  long-running request through this client (`sapedb dump`/`restore`'s bulk
  read, a large batch, an operator-shell command with no natural bound) must
  not start failing the moment this field exists just because nobody set it.
  This is a deliberate departure from `Timeout`'s own zero-means-10-seconds
  habit, made because the two fields bound different things: a dial with no
  address to reach is always a mistake worth failing fast on, while a slow
  *request* against a database that is genuinely doing the work asked of it
  is not. Guarded by
  `TestRequestTimeoutBoundsAHandshakeAgainstASilentPeer` (a listener that
  accepts and then never answers, proving the timeout fires) paired with
  `TestRequestTimeoutStillSucceedsAgainstAnOrdinaryServer` (the same path
  against a real, answering server, run first so the timeout test is not
  trusted on its own) in `internal/wire/wire_test.go`.

### Fixed

- **`store.ErrIncompatible` now has its own wire code, `"incompatible"`.**
  `codeFor` maps every sentinel a server refusal can carry to a word a client
  switches on, and `ErrIncompatible` — the refusal `store.Declare` gives when
  a redeclaration cannot be applied in place, such as "the primary key of `X`
  was declared ..." — had no entry in that table. It reached a caller as the
  generic `"failed"`, indistinguishable from a disk error or a closed
  database.

  This only started mattering with the `Establish` frame: before it,
  `ErrIncompatible` could only be raised offline inside `sapedb apply`, where
  a human reads the sentence. `Establish` sends it down a connection instead,
  where a program has to switch on a code. It has to land before the first
  tagged release, because `codeFor`'s table is a contract this frees: adding
  a code today is an addition, and adding one after clients have written
  `"failed"`-keyed handling for this refusal would be a breaking change.

  The `Declare` frame (which stores an *operation*, not a collection) does
  not reach this error by the same route: it calls `store.DeclareOperation`,
  whose own validation only ever raises `store.ErrDeclaration` (already
  mapped, to `"declaration"`), and redeclaring an operation name that already
  exists writes a new version rather than conflicting with the old one.
  Measured by `TestAnIncompatibleEstablishReachesTheWireAsIncompatible`,
  which sends the frame and reads the code back over a real connection
  rather than reading `codeFor`'s table, and
  `TestDeclareFrameDoesNotReachErrIncompatible`, which measures that the
  `Declare` frame keeps versioning instead
  (`internal/server/incompatible_test.go`).

- **A database that applies a declaration now spends the collection id that
  arrived with it.** `Store.Apply` installs the spec it is given, numbers and
  all, which is exactly right — a replica whose collections were numbered
  differently would store its documents somewhere else. What it did not do was
  move the counter those numbers are handed out from, because that counter is
  only touched by `takeCollectionID`, and a replay takes nothing.

  The counter is the only record of which ids have been used, so a database
  that had followed a log and then declared a collection of its own — which is
  what happens the moment it is promoted, or used for anything besides
  following — was handed id 1 again. A collection id is written into the key of
  every document and every index entry it has, so the new collection's
  documents would have been written into the middle of the followed one's,
  where a scan of either returns both. `Collection.live` already names this
  hazard for a dropped collection ("a later collection given the same id would
  inherit it"); a replica reached it without dropping anything.

  `Apply` now calls `reserveCollectionID`, which moves the stored counter past
  any id it did not hand out. Measured by
  `TestAFollowerThatDeclaresOfItsOwnDoesNotReuseAnID`
  (`internal/store/apply_test.go`), which first shows a database that hands out
  its own numbers gives two collections two of them — so the repeat it looks
  for afterwards is a finding and not a check that cannot fail. Before the fix
  it read: `the follower gave "local" id 1, which "articles" already has`.

- **A caller's own write id now travels with the entry it belongs to.** A write
  carrying an `Attribution.WriteID` records, beside the log entry, what that id
  did, so the same id arriving twice is answered rather than applied twice.
  `record` wrote both; `Apply` wrote only the entry. A database fed somebody
  else's log therefore held every change and remembered no write id, and
  `Wrote` on it answered "never seen" for writes it was itself holding.

  That is the one case the id exists for, on the one database it is most likely
  to be asked of: a client that never heard back about a write retries it, and
  after a failover the retry is aimed at the copy that was following. It would
  have been applied a second time. Both ends now go through one `noteWrite`, so
  what a replica remembers about a write id cannot drift from what the primary
  remembers. Measured by `TestAWriteIDArrivesWithTheEntryItBelongsTo`, which
  checks the id is answerable on the database that made the write before
  asking the one that followed it.

- The change log now says **who declared an operation**. Every other entry in
  it has carried an `Attribution` since there was a log — a write names the
  operation that made it and the actor it ran for, and even a read through the
  shell names `explore` and the operator who typed it. A declaration named
  nobody: `store.DeclareOperation` recorded `{"kind":"operation"}` with an
  empty `by`.

  That was survivable while declaring meant `sapedb apply`, which takes the
  database file's exclusive lock and therefore cannot run while the server is
  up: whoever declared anything was standing at the machine with everything
  else stopped. The `Declare` frame ended it. An operation can now be declared
  from another host, over a live connection, against a running server — and
  the entry with nobody's name on it was the one recording the change that
  decides what everybody else may do.

  Entries now read `"by": {"operation":"declare","actor":"..."}`. The actor is
  the account whose connection reached the database, written by the server,
  never read out of the request — over the wire it is `"<account> (operator)"`,
  matching what an explore already records, and through `sapedb apply` it is
  `"<account> (apply)"`. What the name is worth differs by path and neither
  pretends otherwise: over the wire the account was proved by the signature on
  the connection string, while running `apply` means holding the file, so the
  account there is a label rather than a proof. A label is still the difference
  between an audit trail and a blank.

  `store.DeclareOperation` takes a `store.Caller` as its first argument for
  this, the way `Explore` and `WhatIsHere` already do. That is not a change to
  this module's public surface: package `sapedb` exports value types and a
  client, never `store.Store`. Entries written before this change keep their
  empty `by` — nothing rewrites the log.

  Measured on both paths, each with a control that fails loudly rather than
  vacuously: `TestDeclaringOverTheWireGoesIntoTheLogWithAnActor`
  (`internal/server`), which first proves the log reader can see an actor on an
  explore entry — one that carried an actor before this change — so that an
  empty actor on the declaration is the declaration's fault and not the
  reader's, and `TestApplyRecordsWhoDeclared` (`internal/cli`).

- A key taken from an earlier step was the one value in a declaration whose
  type nobody compared with the place it was used. `check()`
  (`validateOperation`, `internal/store/ops.go`) has always compared an
  `{"arg": ...}` term's declared type against the position it sits in, and its
  own comment says why: "an argument whose type does not fit where it is used
  would sort somewhere else in the index, and the caller would get an empty
  answer with no error." The `{"step": ..., "field": "key"}` branch returned
  before reaching that line. So a batch whose first step read a collection
  keyed by string and whose second handed that key to a collection keyed by
  number was accepted when it was declared, and produced exactly the outcome
  that comment describes every time it ran.

  Both doors now go through the same comparison, measured side by side in
  `internal/store/steptype_test.go` — the argument door that was already shut,
  a control showing a key of the right type still flows between steps and the
  result really is stored under it, and the door this closes. Nothing narrows:
  a step's key in a position declared `any`, which is every `Require`
  condition, is still accepted.

- `apply` (`internal/cli/cli.go`) printed `collection NAME` and
  `operation NAME version N` to stdout as each declaration was decided,
  before the run's single `db.Commit()` at the end of the file loop. A
  multi-file run where a later file failed — even one `checkApply`'s
  pre-open JSON-shape pass cannot catch, such as an operation naming a
  collection nothing declared — left stdout claiming an earlier file's
  collection had been created, when the whole transaction (one `Commit` for
  every file, by design — see `apply`'s own doc comment) was never
  committed and nothing was actually written. The database itself was
  never at risk: nothing durable existed until `Commit` ran either way. Now
  every line is buffered and only written to stdout once `Commit` actually
  succeeds, so a failed run's stdout matches what is really on disk.
  Guarded by `TestApplyDoesNotPrintWhatItRollsBack` in
  `internal/cli/cli_test.go`.
- `internal/wire.Client.Invoke` sent its arguments under the JSON key
  `"arguments"`; the server's `call` struct (`internal/server/server.go`)
  decodes `"args"`. Every declared operation invoked through this client
  silently ran with no arguments at all. Unexercised until now — the CLI's
  shell only ever calls `Explore`/`WhatIsHere`, never `Invoke` — and caught
  by the new `sapedb` package's end-to-end wrapper test, the first thing in
  this repository to call `Invoke` against a real server.
- `describe` (`internal/store/describe.go`), which satisfies (`batch.go`)
  formats a mismatched condition value through, still crashed with
  `fatal error: stack overflow` for a value whose Go type is a *named* type
  built on top of `[]any` or `map[string]any` (for example `type Rows
  []any`) and which holds itself — `fits`/`renderCapped`'s type switch only
  ever matched the two bare types, so a value of a named type fell through
  to the always-safe-leaf default case and reached a raw `fmt.Sprintf`
  with no cycle protection, reopening the exact crash this file exists to
  close. Only reachable by a direct Go caller of the exported `Invoke`
  handing a self-referential value of such a type as a `{"equals": {"arg":
  ...}}` argument — this repository's own CLI and server front doors both
  decode every argument with `json.Unmarshal` first, which never produces
  one. `fits`/`renderCapped` now widen to a `reflect.Kind()` check
  (`fitsByReflection`, `renderCappedByReflection`) alongside the exact-type
  one, closing the gap without changing any byte of the existing
  byte-identical-to-`fmt.Sprintf` behavior for an ordinary value. Guarded by
  `TestDescribeOfANamedSelfReferentialSliceTypeReturnsABoundedStringInstead
  OfCrashing` in `internal/store/describe_named_cycle_test.go`.
- `stretch` (`internal/store/scan.go`) and `refusedBackwardsRange`
  (`internal/store/ops.go`) refuse a `From` that sorts after `To` on every
  field width except one: a single-field `bool` index has exactly two
  values, `keys.Encode(false)` and `keys.Encode(true)` one step apart, so
  the one backwards pair a `bool` field can be written with (`From: true,
  To: false`, both ends inclusive) always encoded to `lower == upper` — the
  same bytes an intentionally self-pinned range produces — and read back as
  a silent, errorless empty result instead of the `ErrArgument`/
  `ErrDeclaration` refusal every other field type gets for the same
  mistake. Closed with a value-level check (`backwardsBoolRange`, `scan.go`)
  run alongside the existing byte comparison at both call sites — true and
  false are never ambiguous the way two floats one ULP apart are, so this
  is judged before either side reaches `keys.Encode`, without touching the
  wider (and deliberate) `lower == upper` leniency documented on `stretch`.
  Scoped to a single-field `bool` index; a composite index carrying a
  `bool` field alongside others is a different, unmeasured shape this does
  not cover. Guarded by `TestABackwardsBoolRangeIsNowRefused` in
  `internal/store/bool_backwards_test.go`, replacing
  `TestABackwardsBoolRangeIsAcceptedRatherThanRefused`, which used to pin
  the old (accepted) behavior in place.
- `runBatch` (`internal/store/batch.go`) rolled back an abandoned
  transaction only on its own explicit error-return path; a panic partway
  through a step skipped that call entirely and left `s.pages.Pending()`
  true, which `runBatch`'s own first line reads as "something is already
  uncommitted" — refusing every future call on that `*Store`, for as long
  as it stays in memory, with `ErrUncommitted`. Harmless on the network
  server (`internal/server`'s `guard` recovers a connection's panic only
  long enough to notify the client and the operator before re-panicking
  and taking the whole process down with it), but not for a direct Go
  caller embedding the store as a library and recovering panics at its own
  boundary — the ordinary shape of an HTTP framework's per-request
  recover middleware — which would otherwise keep running against a
  permanently wedged `*Store`. `runBatch` now defers a recover that rolls
  back before re-panicking the exact original value unchanged, folding in
  a second error only if the rollback itself also fails. No input-reachable
  panic exists in the current write path to trigger this through the
  public API — every one this package's history found (`sameValue`,
  `satisfies`/`describe`, the backward-bool range above) is already closed
  — so this is defense in depth, measured directly by calling `runBatch`
  with a manufactured panic. Guarded by
  `TestRunBatchRollsBackAndRepanicsWhenAStepPanics` in
  `internal/store/runbatch_panic_rollback_test.go`.
- `handshake` (`internal/server/server.go`) wrapped every failure with
  `fmt.Errorf("%w: %v", ErrHandshake, err)` — `%v`, not `%w`, on the inner
  error — so `errors.Is` walking the returned error never reached anything
  past `ErrHandshake`. For a bad signature this meant `codeFor`
  (`server.go`), which deliberately checks `signing.ErrBadSignature` before
  the vaguer `ErrHandshake` fallback, could never reach that check and
  always reported the wire code `"handshake"` instead of the more specific
  `"signature"`. All three call sites now wrap with `"%w: %w"` (Go's `fmt`
  has supported more than one `%w` per `Errorf` since 1.20), so `errors.Is`
  reaches the real cause and `codeFor`'s existing ordering finally gets to
  fire. Guarded by `TestARejectedSignatureLeavesExactlyOneNoticeLine` in
  `internal/server/server_test.go`, updated from asserting the old
  `"handshake"` code to the corrected `"signature"` one.

### Changed

- **The standing gate for shared reads no longer measures time.** SAPE-18 —
  reads share a database instead of queueing behind each other — was guarded
  by `TestReadsRunTogether` in `internal/server`, which timed one read against
  four at once and failed when four cost more than three times one. It passed
  on a developer laptop and failed all three of this repository's first
  automated runs (4.59x, 3.41x, 3.62x), against a build whose reads do share,
  printing `they are queueing, not sharing` — a sentence that was false each
  time it appeared.

  The cause was not a loaded CI box, which is what the test's own comment
  claimed and what its 3x bar was chosen to allow for. Speedup from
  concurrency is bounded by the number of cores: four counts over 4000
  documents are four pieces of CPU work, so on a small runner four cannot cost
  much under four times one however well the locking behaves. Measured at
  `a6c8b48` on a 10-core machine, the unmodified sharing build reports
  1.68-2.10x at `GOMAXPROCS=10`, 2.34-2.76x at 2 and 4.02-4.06x at 1 (8, 8 and
  3 runs); a copy with the read lock made exclusive, so that reads genuinely
  queue, reports 3.61-4.35x, 3.89-3.91x and 3.90-4.08x. At one thread the two
  builds are indistinguishable and the old gate fails both. That is the whole
  problem with it: on a machine short of cores it could not tell a lock from a
  core count, and it reported the lock.

  `TestReadsShareTheDatabase` replaces it and counts instead of timing. A read
  now adds itself to a counter on the open database for as long as it is
  inside the store — `database.reading` and `database.everReading` in
  `internal/server/server.go`, two atomic adds on a call that walks a whole
  collection — and the test asserts that the high-water mark reached two. A
  database behind an exclusive lock cannot reach two on any number of cores,
  because the second read has not been let in. The bar is two because two is
  where sharing begins, not because a number was fitted to a measurement: 40
  runs at `GOMAXPROCS` 10 and 2 every one reached 3 or 4, within 1-7ms, and
  the serialized copy reports 1 at both. What the gate needs is more than one
  thread, not a spare one — two threads busy with other work still run these
  reads inside each other, only slower — and it asserts that precondition
  loudly instead of skipping, because this repository's CI counts a skip as a
  failure on purpose. What it does not claim is that sharing is worth
  anything: it observes overlap, not speed, and a read path that overlapped
  while contending on something inside itself would still pass.

  `TestReadsRunTogetherAtN32` goes with it. It asserted nothing, loaded 20000
  documents and opened 32 connections on every run of the suite, and logged a
  ratio (19.45x on CI, 13.69-14.22x here) that is the reader count divided by
  the cores available and says nothing about locking — an alarming number with
  no claim attached to it.

  Nothing about how the server locks changed: the read path takes the same
  locks in the same order it did before.

- `Spec.Indexes` (a collection's declared indexes, as `Collection.Spec()` and
  the `Catalogue` a `catalogue` explore answers with both hand it out) now
  marshals as `[]` for a collection declared with no indexes, instead of
  `null`. Same policy as `Catalogue.Collections` above, applied one level
  down: `Declare` never allocates an `Indexes` slice for a collection that
  has none, and the field carries no `omitempty`, so the first index a caller
  ever asked about for that collection came back `null`. Fixed at
  `Collection.Spec()` (`internal/store/collection.go`) — what `Declare` and
  `writeSpec` put on disk is untouched; only the copy handed to a caller is
  normalized.

- `Endpoint.Terms` (the `from`/`to` of a declared scan or rollup read) now
  marshals as `[]` rather than `null` for an endpoint declared with no bound
  at all — `{"exclusive": true}` with no `"terms"` key, a legal way to write
  "unbounded here" in a hand-authored `schema.json`. `Explore`'s own path
  never produced this shape (it always builds `Terms` with `make`), but
  `DeclareOperation` decodes an `Operation` straight from a file, and `Terms`
  carries no `omitempty`. Fixed in `validateOperation`
  (`internal/store/ops.go`), which both `DeclareOperation` and `Explore` run
  before anything is marshaled or stored, so this also reaches what gets
  written to disk for any operation declared from now on.

- Measured, not changed: `internal/server`'s wire request field `writeId`
  and `internal/store`'s persisted log field `write_id` carry the same value
  (a write's idempotency key) but are not the same field wearing two
  spellings — one is what a client sends, the other is what the change log
  keeps, and the TypeScript client already keeps them apart the same way.
  Documented at the `call.WriteID` struct tag in `internal/server/server.go`
  so a future "make it consistent" pass does not merge two things that were
  never meant to match.

- `Catalogue.Collections` (the `here.collections` field a `catalogue` explore
  answers with) now marshals as `[]` on a database with nothing declared yet,
  instead of `null`. Before this change, `WhatIsHere` started from a nil
  slice and only ever appended to it, so an empty database — every new
  user's first read — answered `{"collections":null,"operations":[]}`. A
  caller that reaches straight for `.map()`/`for...of` on `collections`, the
  ordinary way to use an array, broke on exactly that database. Fixed at
  `WhatIsHere` (`internal/store/explore.go`), where the slice now starts
  empty rather than nil, not at the marshaling boundary.

- `Bound.Values` and `Bound.Exclusive` (the ends of a scan's `from`/`to`,
  inside an `Access`) now carry `json:"values"` and
  `json:"exclusive,omitempty"` tags. Before this change they had no tag at
  all, so Go marshaled them as `Values`/`Exclusive` (capitalized) while the
  TypeScript client has always written them lower-case; the only thing that
  made a request from that client parse was `encoding/json`'s
  case-insensitive fallback on unmarshal, not an agreed contract. This also
  changes what sapedb's *own* embedded Go client (`internal/wire`, used by
  the CLI) writes when it sends an `Access` to the server: it used to emit
  `Values`/`Exclusive` like any other untagged Go struct, and now emits
  `values`/`exclusive`, matching the TypeScript client. No caller inspecting
  the wire bytes of a `Bound` by field name is known to exist yet — this is
  believed to be pure convergence, not a break — but any archived request
  bytes or fixtures captured with the old, untagged spelling would need
  updating if one turns up.

- A scan or a rollup read whose `from` sorts after its `to` is refused
  instead of run. Before this change, such a declaration read back an empty
  result — no `rows`, no error, no `truncated` — indistinguishable on the
  wire from a stretch that is legitimately empty. Now it is an error:
  `code=declaration` when both ends are constants (caught when the operation
  is declared, since no argument could ever make it return a row), or
  `code=argument` when either end depends on an argument or a batch step's
  key (caught when the operation runs, since that is the first point either
  value exists). `from` is the low end of a stretch and `to` is the high end,
  in both the forward and the reverse direction — writing them the other way
  round is not a different, valid stretch, in either direction.

  This does not change what `from == to` means: two ends pinned to the same
  point, with `exclusive` telling them apart, is how a caller writes an
  empty range on purpose, and it keeps returning an empty result rather than
  an error.

  Not a migration: this package has never been tagged and carries no
  `CHANGELOG.md` entry before this one, so there is nobody upgrading from a
  released version who could be relying on the old silent-empty answer.

- A command line the CLI refuses because of a bad argument no longer creates
  anything under `SAPEDB_DIR`. Before this change, `run()` opened the
  database — taking `.lock`, creating the account folder, and creating
  `<db>.sapedb` and `<db>.parts/` — before any command checked its own
  arguments, so `sapedb apply` with no file, `sapedb apply` given a file
  that does not exist or is not valid JSON, `sapedb log not-a-number`, and
  several others like them left a database sitting on disk despite exiting
  with an error. Argument checking for all seven subcommands now runs
  before the database is opened, so a refusal for a bad argument leaves the
  directory exactly as it found it — including at a directory two levels
  deep, and including the case where the only thing a refusal used to add
  back was a single empty directory.

  This is about arguments only. `sapedb restore` reading garbage or nothing
  at all from stdin is unchanged: it still opens (and leaves behind) an
  empty database before failing on the stream, because stdin is not argv,
  and because that empty database is what a correct `restore` run
  immediately afterward needs. See the code comment on `restore`'s entry in
  the command table.

- **Breaking**: `sapedb ls`, `sapedb dump`, `sapedb restore` and `sapedb url`
  now refuse an extra argument instead of silently ignoring it, and
  `sapedb log` now refuses more than one. Before this change none of `ls`,
  `dump`, `restore` or `url` checked their argument count at all, and `log`
  read only the first of any extra arguments it was given:

  - `sapedb ls junk` and `sapedb dump junk` exited 0, argument thrown away.
  - `sapedb restore junk` exited **1** on an empty or invalid dump on
    stdin — same as `sapedb restore` with no argument at all — because
    what failed it was the stream, not the argument; the argument was
    still thrown away in silence, and it still opened (and left behind) an
    empty database on the way to that failure. (An earlier draft of this
    entry said `restore junk` exited 0. That was wrong — checked against a
    `sapedb` built from the commit before this change, with an empty
    stdin, exit status is 1, from `EOF` while reading the dump header, not
    from the extra argument.)
  - `sapedb url HOST - junk` exited 0, the trailing word after the
    password marker thrown away.
  - `sapedb log 1 2 3` exited 0, only `1` read.

  A script relying on any of that being ignored will now see the command
  refused instead. To migrate, drop the extra words:

  ```diff
  - sapedb ls junk
  + sapedb ls

  - sapedb dump junk
  + sapedb dump

  - sapedb restore junk
  + sapedb restore

  - sapedb url localhost:7433 - junk
  + sapedb url localhost:7433 -

  - sapedb log 1 2 3
  + sapedb log 1
  ```

  If a script depends on `sapedb restore`'s stdin failure exiting 1
  regardless of any trailing argument, that part is unchanged — only the
  argument-thrown-away part is new, and only for `ls`, `dump`, `url` and
  `log`, whose earlier commands used to exit **0**.
