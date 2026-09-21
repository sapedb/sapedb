# The constrained-profile benchmark rig

```sh
./bench/run.sh
```

That is the whole of it. It builds the server image from the tree it is standing
in, runs it under three memory-and-cpu limits in turn, drives one workload at it
over the wire, watches the container's memory from outside while it does, pushes
one profile until it runs out of memory, writes `bench/results/latest.md` and
`bench/results/latest.jsonl`, and takes the stack and its volume down again.

It needs docker and a POSIX shell. Nothing else — no `jq`, no python, no
benchmarking framework, no Go toolchain on the host.

## What this is not

`../docker-compose.yml` is the production example: TLS, encryption at rest, a
secret that does not live in the file, a real volume. **This rig is not that, and
copying from it would be a mistake.** It serves plaintext, with encryption off, to
a throwaway database under a random secret, because every layer left in the way is
a layer in the number. The two files have nothing to say to each other and neither
should be edited to look like the other.

## The profiles

| profile | cpus | memory | swap |
|---|---|---|---|
| tiny | 1 | 512m | none |
| small | 1 | 1g | none |
| medium | 2 | 2g | none |

Three, not one, so a reader sees the shape of the curve rather than a single
point.

`memswap_limit` is set equal to `mem_limit` on every profile. This is the line
that decides whether the whole exercise is worth anything: `memswap_limit` bounds
memory **plus** swap, and leaving it out lets docker allow swap up to twice
`mem_limit`. The container then swaps rather than hitting the wall, every number
comes back looking healthy, and the one machine this rig exists to say something
about — a small VPS that runs out of memory — is the one thing that never gets
measured. `run.sh` reads the applied limits back off the running container with
`docker inspect` and prints them into the report, rather than trusting that the
compose keys did anything.

## The workload

The application this was written for stores a ten-megabyte file as about five
thousand base64 rows of two kilobytes each, under keys shaped `<id>/000000`. One
run here is one such file. The read path is the same one that application uses: a
prefix scan, paginated with an exclusive `from` and a limit declared in the
operation.

- **insert** — `-runs` files of `-rows` rows, one request per row, each file timed
  on its own.
- **scan** — each of those files paged back, `-page` rows at a time, `-repeat`
  times over. Reading is cheap enough here that a single pass is over faster than
  the host can take one memory sample, so each file is read repeatedly and every
  pass is its own timed reading; the first pass over a file is cold and the rest
  are warm, which is why the spread is wide on purpose.
- **delete** — each of those rows removed, one request each. This is the baseline
  that `deleteRange` (SAPE-32) is to be compared against, so it is recorded
  carefully: one declared delete per row, one transaction and one change-log entry
  each.
- **wall** — rows added to a collection, and after every batch a read that asks
  for all of them at once, until the server stops coming back.

Between each phase the rig samples the server container's memory from outside,
about once a second, and records the peak it saw.

## The layer these numbers belong to

A number here may be quoted with these words attached and no others:

- over the wire, through this repository's own Go client, against `sapedbd` in a
  container of its own;
- one connection, one request outstanding at a time. The client has no pipelining
  and no pool and says so; these are sequential round-trip rates, not saturation
  figures;
- no TLS, no encryption at rest. Both cost something, and neither is in these
  numbers;
- the load generator is in its own unconstrained container, so the memory read off
  the server's container is the server's own.

And the one that is not a footnote: on macOS, Docker Desktop is a Linux virtual
machine with a virtualised disk, and **its disk behaviour is not a VPS's**. This
store commits per write, so the write figures are the ones most exposed to that
difference. Read the ratios — between profiles, and between insert and delete — and
do not quote the absolute rows/second at anybody.

## Why this is a `cmd`-shaped tool and not `func Benchmark`

Go's own benchmark runner decides how many iterations to run and reports one
number per benchmark. This rig has to report a range over a fixed number of runs,
pair each phase with a peak memory sampled from outside the container by the host,
and survive the process under test being killed halfway through. None of those
three fit `testing.B`, and two of them fit it badly enough that the result would
have been a benchmark harness wrapped in a shell script wrapped in a benchmark
harness.

`go test ./...` does not run anything in this directory but the unit tests for the
two statistics functions, which take microseconds. There is no `func Benchmark` in
the tree for `go test` to pick up, by design: the load generator is a `main`, and
running it requires a server to point it at.

## Knobs

All optional, all environment variables:

| | |
|---|---|
| `BENCH_PROFILES` | default `tiny small medium` |
| `BENCH_WALL_PROFILE` | default `tiny`; empty to skip the wall |
| `BENCH_RUNS` | files per phase (default 5) |
| `BENCH_ROWS` | rows per file (default 5000, about ten megabytes) |
| `BENCH_PAGE` | declared limit of one page (default 100) |
| `BENCH_REPEAT` | scan passes over each file (default 20) |
| `BENCH_WALL_STEP` | rows added per rung (default 5000) |
| `BENCH_WALL_RUNGS` | most rungs (default 24) |
| `BENCH_WALL_MINUTES` | budget for the wall phase (default 30) |
| `BENCH_OUT` | report path (default `bench/results/latest.md`) |
| `BENCH_KEEP` | `1` leaves the stack up for debugging |

## Results kept in the repository

`results/` holds the first run, as a baseline. It says on its face that it is one
machine on one day. A later run that disagrees with it is not evidence that
anything regressed until it has been run on the same machine.

**One thing in the first baseline should not be read as a result.** Its insert
figures fall as the profiles get bigger — `tiny` is the fastest and `medium` the
slowest — which is the opposite of what more cpu and more memory should buy. The
profiles are run one after another on one host over about forty minutes, and
nothing in the rig holds that host still while they are; `medium` ran last. So
that ordering is far more likely to be the host drifting than the limits
mattering, and until the three profiles have been run in a different order on a
quiet machine it is not evidence of anything. Read the insert figure as *what one
profile cost*, not as a comparison between them. The scan and delete figures,
where the three profiles agree with each other almost exactly, are the ones the
profile comparison is safe on.

The first baseline names its commit as `737e149-dirty`, and the suffix is not a
mistake. `git describe --dirty` says a build was made over uncommitted changes,
and the uncommitted change was this rig: the first run of a benchmark cannot be
made from a tree that already contains its own results. `737e149` is the commit
of everything it measured — the server, the store, the protocol — and nothing
uncommitted at the time was part of the server image, which copies only `go.mod`,
`go.sum`, `internal/` and `cmd/`. Every run after this one describes a committed
tree and says so.
