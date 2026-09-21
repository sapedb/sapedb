# What 1.0.0 freezes, and what it refuses to promise

A version number is a promise that cannot be withdrawn. This document says which promises
1.0.0 makes, so that "may this change in 1.1?" has an answer somebody wrote down rather than
an answer the first person to ask happened to assume.

It is deliberately short. A policy nobody reads is a policy nobody keeps.

## Five frozen surfaces

Each one names the file and the symbol that holds its current value, and the test that keeps
this document from drifting away from the code. Symbols are cited rather than line numbers,
because line numbers move and a citation that has silently stopped resolving is worse than no
citation.

### 1. The on-disk format

Any 1.x build opens a database written by any other 1.x build, and writes one any other 1.x
build can open.

| | |
| --- | --- |
| Where | `internal/pager/pager.go` — `Magic` and `Format` |
| Today | `Magic` is the eight bytes `SAPEDB\0\x01`; `Format` is `1` |
| Enforced by | `TestTheFormatTagIsExactlyThis` and `TestTheFormatNumberIsExactlyThis` (`internal/pager`) |

A 1.x release **may** add a page kind, add a field to a page that older builds skip, and
change anything about how pages are laid out inside a partition file.

A 1.x release **may not** change `Magic`, change `Format`, or change the meaning of an
existing field. Any of those is 2.0, and it needs a migration, because the files already
exist on somebody's disk.

`Format` is the escape hatch, and it is the only one. It is a separate two-byte field rather
than a suffix on `Magic` precisely so that a future format can be refused politely — *"file
says 2, this build reads 1"* — instead of read as corruption. That refusal path is measured at
all four of its call sites; see `internal/pager/format_test.go`.

### 2. The wire protocol

| | |
| --- | --- |
| Where | `internal/protocol/frame.go` — the `Type` constants |
| Today | Fourteen types, `Hello` = 1 through `Establish` = 14 |
| Enforced by | `TestTheFrameTableIsExactlyThis` (repository root) |

A 1.x release **may** add a new frame type, numbered 15 or above, and **may** add a field to
an existing frame's JSON body, as long as a build that does not know the field still works
without it.

A 1.x release **may not** renumber a frame, change what an existing number means, rename a
field, or remove one. **Frame 14 is spent** — `establish` took it — so the next frame anybody
adds starts at 15.

The conformance fixtures in `fixtures/frames.json` are the machine-readable half of this
promise, and the TypeScript and PHP clients read the same file.

### 3. Declaration semantics

An operation declared against 1.0 means the same thing on every later 1.x. This is a promise
about **files people already wrote**: declarations are stored inside the database, so a change
here reaches back into databases nobody is going to re-apply.

| | |
| --- | --- |
| Where | `internal/store/ops.go` — `validateOperation` and the `Action` constants; `internal/store/store.go` — `Declare` |
| Enforced by | `internal/store`'s own suite, and the refusal texts it asserts verbatim |

A 1.x release **may** add an action, add an optional field to a declaration, and make a
refusal message clearer.

A 1.x release **may not** change what an existing declaration does when it runs, tighten a
rule so that a declaration valid in 1.0 is refused in 1.1, or change what a `limit` means.
Loosening a rule is allowed; tightening one is not, because the declaration is already in the
database.

### 4. The public API

| | |
| --- | --- |
| Where | `sapedb.go` — everything exported from `package sapedb` |
| Today | Forty exported names |
| Enforced by | `TestTheSurfaceIsExactlyTheseFortyNames` (`sapedb_test.go`) |

That test compares the exported set **in both directions**, so it fails on a name added as
well as one removed. Adding a name is a deliberate act with a test to update, not something
that happens in a refactor.

A 1.x release **may** add an exported name. It **may not** remove one, change a signature, or
change what a method does to the database.

### 5. The operation name grammar

What an operation may be called. It is a surface of its own rather than a line under
"Declaration semantics", because it is the one rule a **caller** depends on as much as a
declarer: a name is what an `invoke` frame carries, and a name that stops resolving is an
application that stops working without anything in it having changed.

| | |
| --- | --- |
| Where | `internal/store/namespace.go` — `SplitOperationName` and `NamespaceSeparator`; `internal/store/spec.go` — `usableName` |
| Today | `namespace:name`, at most one `:`; no `:` is the unnamed namespace; a namespace holds letters, digits, `.`, `-`, `_`; the name half is non-empty and holds no zero byte; 128 bytes for the whole thing, separator and namespace included |
| Enforced by | `TestWhatAnOperationNameMayBeNow` and `TestSplitOperationNameReadsBothHalves` (`internal/store`) |

A 1.x release **may** loosen this: widen the namespace charset, or raise the 128-byte
ceiling. Both make names that are refused today start working, which breaks nothing that
exists.

A 1.x release **may not** tighten it — refuse a name 1.0 accepted — or change how a name
splits, because that changes which declaration an existing caller's `invoke` reaches. In
particular, **the unnamed namespace stays unowned**: a flat name is first-come-takes-over, a
second declaration of one wins every unversioned call, and making the empty namespace
claimable in a 1.x would lock every existing declarer out of their own names.

This one was decided at the last possible moment, and SAPE-9's entry in the CHANGELOG says
so. Before it, every separator was a legal name — `internal/store/collision_test.go`'s
`TestAnOperationNameMayHoldAnySeparatorAName` declares `acme:orders.recent` among others and
all of them are accepted — so under the rule above refusing `a:b:c` in a 1.1 would have been
forbidden. There is no version of this that arrives after the tag.

Everything under `internal/` is exempt, by the language and by intent. It is not a public
surface and nothing about it is promised.

## Version numbers: one line each, bound by the protocol

The server, each of the three clients and the desktop app **each carry their own version
line**. There is no shared number.

What binds them is the **protocol version** in the handshake — `version`, which is `1` and has
been since there was a frame. It is not the same field as `productVersion`, which is the build
the server was made from and is free to differ across every component.

A shared number would force a client release every time the server fixed a bug, and undoing
that later means renumbering something somebody has already installed.

## What 1.0.0 does not promise

Said out loud, because a promise made by silence is still a promise:

- **Performance.** No number here is a commitment. Where a measurement is published, the layer
  it was measured at is published with it.
- **The relative ordering of reads and writes**, beyond what is already true: a read is
  strictly before or strictly after any given write. The README describes a snapshot-read path
  the pager already has the pieces for; if it is ever taken, reads may be served from an older
  snapshot while writes continue past them. Atomicity and durability of writes would not change.
- **The file layout inside a partition.** Which file a document lands in, and how a partition
  is divided, may change within 1.x.
- **Anything under `internal/`.** No symbol there is stable, including ones this document
  cites — the citations are for a reader, not an import.
- **Point-in-time recovery.** One log of this shape is what PITR would be built on. It is not
  PITR, and no 1.x is promised to make it so.
- **Revocation of a grant.** A grant expires; nothing takes one back early short of rotating
  the server secret.

## How a promise here gets broken on purpose

By deleting an assertion, in a commit that says it is doing that. Every surface above has a
test that fails when the value moves, so no change to any of them can arrive as a refactor.
That is the whole mechanism: there is no process document, only tests that have to be argued
with.
