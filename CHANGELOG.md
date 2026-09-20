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
