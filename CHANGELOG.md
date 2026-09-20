# Changelog

This file starts at the entry below (task 0045). Nothing before this point was
recorded, so its absence is not a claim that nothing changed before it.

## Unreleased

### Breaking

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

  On the wire it is an optional `grant` object on the `invoke` payload:
  `{"scopes": [...], "sig": "..."}`. Sending none is sending no scopes, so
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
  from 32 names to 33.

  Deliberately **not** a challenge-response like the operator proof. A nonce
  would stop a grant being replayed and would also require whoever issues
  grants to be online at the moment of every connection, turning an offline
  control plane into a request path. A grant is a bearer credential of exactly
  the same weight as the connection string it travels beside: whoever holds
  that string can already reach the database, and the grant says what they may
  do once there.

  Known limits, written here rather than left to be discovered: a grant has
  **no expiry and no serial number**, so it is good until the secret changes,
  and withdrawing one scope from one account today means reissuing every
  connection string on that server. Adding an expiry changes the signed
  message, which is a change this side and the TypeScript signer make together
  — so it is not being done halfway now. A scope may contain `:` (every scope
  this product uses does) and may not contain `,`.

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
