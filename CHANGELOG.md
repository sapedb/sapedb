# Changelog

This file starts at the entry below (task 0045). Nothing before this point was
recorded, so its absence is not a claim that nothing changed before it.

## Unreleased

### Added

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
