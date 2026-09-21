# sapedb

A storage service whose only interface is a **named, declared operation**.

No query text ever reaches the engine. An operation is declared once — which
collection, which index, where along it, how many rows at most — and stored in
the database itself, versioned. Clients call it by name and pass arguments. An
argument cannot widen a scan, reach another collection, or become part of a key.

Three things follow, and they are the whole point:

- **Cost is known before anything runs.** A scan declares its limit, so there is
  no such thing as a query that turns out to be expensive in production.
- **Injection cannot exist**, because there is no text to inject into.
- **Every call leaves a trace** — who ran which operation, at which version, in
  the same log that replicas replay.

It is built for code nobody fully trusts: generated code, plugin code, code
written by an agent. The safety is in the storage layer rather than in a proxy
in front of it.

## What it is not

Said plainly, because a database that oversells itself costs somebody a quarter.

- **Not an analytics database.** One writer per database, and a commit means an
  fsync. Load spreads across tenants, not within one.
- **No interactive transactions**, ever. A client cannot hold one open. Several
  writes in one transaction is a declared *batch* operation instead.
- **No query language**, and no plan to add one. What exists for the questions
  nobody declared is an operator shell, which can only do what an operation
  could have declared, and which writes down everything it did.
- **No recovery path.** Not an omission: the commit order is data → sync → meta
  → sync, and a crash lands on a meta page describing a completed transaction.
  A recovery path is code that only runs when everything is already wrong, which
  makes it the least exercised code in the system.

## What is here

| Path | What |
| --- | --- |
| `cmd/sapedbd` | The server |
| `cmd/sapedb` | The CLI: apply, ls, dump, restore, log, url, shell |
| `internal/vfs` | The only thing that touches a disk, and a simulated disk that can be told to lie |
| `internal/pager` | Pages, checksums, two alternating meta pages, encryption at rest |
| `internal/btree` | Copy-on-write B+tree |
| `internal/keys` | Order-preserving key encoding |
| `internal/store` | Collections, indexes, operations, batches, the change log, partitions, rollups |
| `internal/protocol` | Frames |
| `internal/server` | The server, subscriptions, the operator channel |
| `internal/wire` | A small Go client, for the tools that ship with the server |
| `internal/signing` | The signing contract, shared with the client through `fixtures/signing.json` |
| `examples/ledger` | Order → payment → double-entry ledger, running |

The client for TypeScript applications is
[`@ecosy/sapedb`](https://github.com/material-atomic/ecosy-sapedb).

## The parts, briefly

**Operations.** Declared in a JSON file and applied with `sapedb apply`. Get by
key, scan a declared index between declared bounds, count, insert, put, update,
delete — and `batch`, which is several of those in one transaction, where a step
may require the document to already be in a particular state. That last part is
optimistic locking, written down in the schema where somebody deciding whether
to trust an operation can read it.

**Composed operations.** A step of a batch may name an operation that is
already declared instead of a collection, which is how a vocabulary gets built
out of itself rather than by adding an action for every shape somebody wants:

```
operation "catalog.page", action batch, limit 51
  input:  shelf (string)
  step "items":  operation "items.on_shelf@1"       with { shelf }   -- scan,   at most 50
  step "total":  operation "items.total_on_shelf@1" with { shelf }   -- totals, at most 1
```

A page and that page's true total, in one call and therefore in one state —
which is what this buys. Done as two calls they are two states, and a write
landing between them is how a user is shown "51 results" over a page of 50
that just lost one. It also saves K−1 round trips for K steps, which is a
number you read off the declaration. It is **not** a claim about speed: every
measurement so far is on loopback, where the saving is the size of the
difference between two runs of the same measurement, and nobody has measured
it across a network.

A step runs exactly once, and that is enforced rather than intended: a
reference pins a version, so a reference cycle cannot be written down and a
redeclaration of the callee cannot move what the caller costs; a term may only
take a value from a step that returns exactly one row, so "once for each row
the other step returned" — a loop — has no spelling; and the limit above must
cover the sum of its steps' limits. That last rule is why the ceiling of a
composed operation **at any depth is the number written in it**, not a product
and not a sum you have to work out. The same one number a flat scan declares.

**Every write is a transaction.** Atomic across the document, every index
entry and the log line; durable before the answer goes back; isolated because
only one write runs against a database at a time. Which is why there is no
`begin`: there is nothing it would add.

A read may now run alongside other reads instead of queueing behind them, but
never alongside a write — a database's lock is a writer's alone or every
reader's together, never both at once. So today, a read is always strictly
before or strictly after any given write, the same guarantee the single-writer
design has always made, just no longer serialized against *other* reads too.
That describes what sapedb does today, not a promise about the mechanism: the
pager already carries the pieces a later snapshot-read path would need
(`Store.Take`, `pager.Snapshot`, a freelist that counts open snapshots), and
none of it is called from the serving path yet. If a read is ever served from
an older snapshot while writes continue past it, this paragraph is what
changes — atomicity and durability of writes would not.

**The change log** is written in the same transaction as the change it
describes. One log, so replication, change feeds and audit cannot disagree with
each other. `subscribe(from)` streams it, and `sapedb log` prints it.

Point-in-time recovery is what a single log of this shape is *for*, and it is
not built: no command steps the log to a chosen point, and `restore` only
replays a dump into a database with nothing in it. The material is there and
the command is not, which is a different sentence from the one this paragraph
used to carry.

**Partitions** divide a collection into files, decided by the primary key —
by time (the key is a ULID, which already carries the millisecond it was made)
or by hash. Dropping one is an unlink. Indexes are local to their partition,
which is what keeps that true, and every consequence of it is checked where an
operation is declared rather than where it runs.

**Rollups** are totals kept by the transaction that changes them, so a count can
never disagree with the data it counts.

**The operator shell** (`sapedb shell`) is for the question nobody declared. It
can do exactly what an operation could declare, nothing more; it proves it holds
the server's own secret before it may; every access it makes goes into the
change log with a name against it; and `declare` prints the operation that would
do what you just did, so exploring ends in something to commit.

**Build identity** is what a binary answers when asked which one it is. The
version is written in by the linker from the tag being built, `sapedb version`
prints it, `sapedbd` says it on the first line of its log, and the welcome every
client gets carries it as `productVersion` — which is not the `version` beside
it in that same frame: that one is the protocol, it has been 1 since there was a
frame, and it is why two servers built six months apart used to introduce
themselves identically. Build with `make dist`, or stamp it by hand:

    go build -ldflags "-X github.com/sapedb/sapedb/internal/build.Version=$(git describe --tags)" ./cmd/...

A binary nobody stamped says `dev`, which is a word no release will be called.
Nothing here checks for or installs a newer version; asking what this is and
going to get another one are different jobs, and only the first one is here.

**Releases** are built for four platforms — `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64` — with `make dist-cross`, each binary stamped
the same way `make dist` stamps one for the host it runs on. `make checksums`
covers all eight artifacts in one `checksums.txt`
(`sha256sum -c checksums.txt` or `shasum -a 256 -c checksums.txt` both read
it), and `make verify-dist` re-derives the stamped version from each
artifact's own bytes rather than trusting that the build succeeded — see
ISS-18, the reason that check exists at all. `.github/workflows/release.yml`
runs all of this, plus the container image, on every push of a tag matching
`v*`, and publishes the binaries to a GitHub Release and the image to a
registry. It also runs on `workflow_dispatch` as a dry run that shares every
one of those build and verification steps — including starting the built
image and checking that a real connection to it reports the stamped
`productVersion` — and stops before publishing anything, uploading the
results as a workflow artifact instead.

**Encryption at rest** is AES-GCM per page under a key derived from the secret.
The meta page is not encrypted, on purpose — something has to be readable
without the key or "not our file" and "wrong key" become one answer.

## Running it

    go build ./cmd/sapedbd ./cmd/sapedb

    export SAPEDB_SECRET=... SAPEDB_DIR=/var/lib/sapedb SAPEDB_ACCOUNT=acme SAPEDB_DB=main
    sapedb apply schema.json
    sapedbd

SAPEDB_ACCOUNT and SAPEDB_DB each become one path component under SAPEDB_DIR,
and together they derive the encryption key when SAPEDB_ENCRYPT is set — so
each must be 1 to 64 characters of letters, digits, dot, dash or underscore,
and neither `.` nor `..` on its own. A name outside that is refused, not
adjusted or normalized.

The server refuses to start without TLS unless `SAPEDB_INSECURE=1` says you meant
it. The Docker image ships the server binary and nothing else — no shell, no
package manager, no libc.

### Publishing a signed bundle

A bundle is the same two lists `sapedb apply` reads — collections and
operations — with a name, an author and an ed25519 signature on them. There is
no separate declaration language for an author to learn: a draft is a schema
file with three strings in front of it.

    {
      "name": "orders-pack",
      "version": "3.4.0",
      "signer": "Orders Authors",
      "collections": [ ... ],
      "operations":  [ ... ]
    }

`keygen` makes the identity and `seal` signs a draft with it. The private key
is never printed and never an argument: `keygen` writes it into the file you
name, refuses to write over anything already there, creates it mode 0600, and
prints only the public half — one line, 64 lower-case hex characters, which is
exactly what `SAPEDB_TRUST` takes. `seal` reads the private key back on stdin,
so the key can come out of a secret store and never touch disk at all.

    $ sapedb keygen orders.key
    2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64

    $ ls -l orders.key
    -rw-------  1 ngvcanh  wheel  129 Sep 21 07:37 orders.key

    $ sapedb seal orders.draft.json orders.bundle < orders.key
    sealed orders.bundle, signed by 2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64

The hex above is a real public key from a real run of these commands, kept so
the output below is what the commands actually print. It is a throwaway: its
private half was generated in a scratch directory, never existed in this
repository, and has been deleted. A public key is meant to be published — that
is the whole point of the line — and there is no private key anywhere in this
repository, in a test, a fixture or a document.

Send `orders.bundle` and that one line of hex to whoever runs the server. A new
author joins an operator's trust list in one command:

    export SAPEDB_TRUST="orders-authors=$(sapedb keygen orders.key)"

Neither command opens a database, needs `SAPEDB_SECRET`, or touches the
network — an author signing a release is usually not an operator of anything.

`seal` refuses a draft it would have to lie about: one carrying a field this
version does not read, one claiming some other format, and one that already
carries a signature. None of those is re-signed quietly.

There is no key rotation, no revocation, and no registry. Replacing an author's
key means an operator editing `SAPEDB_TRUST` by hand, and bundles already
installed stay installed — what a bundle declared is indistinguishable
afterwards from a declaration somebody wrote by hand, and there is no uninstall.

### Installing a signed bundle

It carries no code, so a verified bundle widens what a database holds, never
what the server is able to do.

    $ export SAPEDB_TRUST="orders-authors=2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64"

    $ sapedb verify orders.bundle
    bundle "orders-pack" version "3.4.0"

      read      yes  sapedb/bundle:v1
      signed    yes  ed25519, key 2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64
      intact    yes  the signature is over exactly these declarations
      trusted   yes  an operator of this server put that key on the list

      says it is "Orders Authors" (the bundle's own claim about its author)
      known as  "orders-authors" (what an operator of this server called that key)

    declares
      collection orders (key id string, ulid)
        index by_customer [customer string missing:skip, placed number desc missing:last]
      operation orders.add insert orders
      operation orders.by_customer scan orders via by_customer limit 25 scopes orders:read

    $ sapedb install orders.bundle
    installing bundle "orders-pack" version "3.4.0", signed by 2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64, trusted here as "orders-authors"
    collection orders
    operation orders.add version 1
    operation orders.by_customer version 1

    $ sapedb ls
    collection orders (key id string, ulid)
      index by_customer [customer string missing:skip, placed number desc missing:last]
    operation orders.add v1 insert orders
    operation orders.by_customer v1 scan orders via by_customer limit 25 scopes orders:read
    log 1..3

Two refusals, both worth seeing, because the reports say which of them
happened. A bundle whose key is not on this server's list:

      signed    yes  ed25519, key 2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64
      intact    yes  the signature is over exactly these declarations
      trusted   no   no operator of this server put that key on the list

      says it is "Orders Authors" (the bundle's own claim about its author)
      known as  nothing on this server

    declares (nothing below is vouched for: the checks above did not pass)

and a bundle somebody edited after it was sealed — here `"limit": 25` rewritten
to `"limit": 900`, which is the declaration of what an operation costs:

      signed    yes  ed25519, key 2017d976ee5786f34d78b4c1eb331cfb55ff7fbb04b5e13afcdbc78cf9171d64
      intact    no   these are not the declarations that key signed
      trusted   -    not reached; the signature did not check out

Both exit 1, and `install` refuses the file before the database is opened at
all.

`SAPEDB_TRUST` is whose bundles this host will look at, written `label=key`
and separated by commas or newlines. Unset is an empty list, and an empty list
refuses every bundle and says so — there is no value that means "trust
anything". The label is local: `verify` prints it beside the name the bundle
claims for its author, and comparing those two is the whole of the name
binding, because there is no PKI here.

`verify` opens no database, needs no secret and touches no network. `install`
declares everything the bundle carries or none of it, and records the bundle
and the key in the change log rather than the account that ran the command.

### Following another server

    export SAPEDB_FOLLOW='sapedb://acme:...@leader:7433/main?sig=...'
    sapedbd

A daemon started like this keeps a copy of that database: it subscribes to its
change log from wherever its own copy has got to, applies every entry, and
**refuses every write of its own** — `read_only`, with the reason in the
message. The same connection string reaches both, because a signature covers
the account, the password and the database name and deliberately not the host.
`SAPEDB_FOLLOW_INSECURE=1` dials the leader without TLS.

A follower keeps no place of its own — no cursor file, nothing beside the data.
Applying an entry writes the log counter in the same transaction as the change
itself, so where to resume is the database's own latest entry, plus one, and
there is no moment at which the two could disagree.

Refusing writes is wider than it sounds and the surprise is worth knowing
before you point a tool at one: reading the catalogue and the operator shell
both record an entry in the change log, so **both are refused on a follower**.
Invoking a declared operation that only reads is not.

Replication is asynchronous, a follower never promotes itself, nothing measures
lag, and a daemon follows one database rather than a list. None of those is
built.

To watch the example end to end:

    SAPEDB_SERVER_BIN=./sapedbd SAPEDB_CLI_BIN=./sapedb node examples/ledger/run.mjs

## How this is tested

Unit tests are the floor, not the ceiling.

**Mutation testing** is the standard: production code is changed in ways
somebody could plausibly write by mistake, and a test must go red. A mutation
that survives means a property nobody is checking — it gets a test, or a written
reason beside the code explaining why it cannot be observed from outside. There
are three such reasons in this repository and each one names itself.

**A simulated disk** that can cut power between any two writes, tear a write,
reorder writes and lie about `fsync`, deterministically from a seed. Durability
is stated as two tests: with honest syncs a committed root always reads back,
and with a lying sync a transaction can be lost and all that survives is that
the file opens and names the damage.

**Real runs across layers.** Binaries, a container, two processes, the
TypeScript client calling the Go server. Three contract mismatches between the
two repositories passed every unit test on both sides, because each side agreed
with itself. Only running them together found any of them.

## The shared fixtures

`fixtures/signing.json` carries connection triples with their expected digests,
and `fixtures/frames.json` carries wire frames with the bytes they encode to.
This repository's tests and `@ecosy/sapedb`'s both read both files, so a
contract change that lands in both copies turns both suites red at once —
instead of arriving as a user who cannot connect, with nothing in a log to say
which side is wrong.

"Shared" is the intent, and the gap between it and the mechanism is worth
stating plainly: these are two files in two repositories, kept identical by
hand. Nothing compares them. A change made to one copy reddens one suite while
the other stays green and disagrees — which has already happened here, for ten
minutes, seen by neither side. Until the fixtures are published as one artifact
both repositories consume, the sentence above describes a workflow, not a
guarantee.

They prove the two implementations of one *function* agree. They do not prove
the two sides *call* that function with the same arguments, which is a lesson
this project paid for once.
