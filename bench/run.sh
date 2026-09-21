#!/usr/bin/env sh
#
# One command: ./bench/run.sh
#
# Builds the server image from this tree, runs it under each constrained
# profile in turn, drives one workload at it, samples the container's memory
# from outside while it does, pushes one profile until it runs out of memory,
# and writes a Markdown report. Then it takes everything down again, including
# the volume.
#
# It needs docker and a POSIX shell. No jq, no python, no benchmarking
# framework: every JSON line this script produces it produces with printf, and
# every JSON line it consumes is read by the Go program in this directory,
# which is where the one JSON parser in the rig lives.
#
# Knobs, all optional:
#   BENCH_PROFILES       default "tiny small medium"
#   BENCH_WALL_PROFILE   default "tiny"; "" to skip the wall
#   BENCH_RUNS           files per phase           (default 5)
#   BENCH_ROWS           rows per file             (default 5000, ~10 MB)
#   BENCH_PAGE           declared limit of a page  (default 100)
#   BENCH_REPEAT         scan passes over each file (default 20)
#   BENCH_WALL_STEP      rows added per rung       (default 5000)
#   BENCH_WALL_RUNGS     most rungs               (default 24)
#   BENCH_WALL_MINUTES   budget for the wall      (default 30)
#   BENCH_OUT            report path (default bench/results/latest.md)
#   BENCH_KEEP           set to 1 to leave the stack up for debugging

set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(dirname "$here")
cd "$root"

compose="docker compose -f bench/compose.yml"

PROFILES=${BENCH_PROFILES:-"tiny small medium"}
WALL_PROFILE=${BENCH_WALL_PROFILE-tiny}
RUNS=${BENCH_RUNS:-5}
ROWS=${BENCH_ROWS:-5000}
PAGE=${BENCH_PAGE:-100}
REPEAT=${BENCH_REPEAT:-20}
WALL_STEP=${BENCH_WALL_STEP:-5000}
WALL_RUNGS=${BENCH_WALL_RUNGS:-24}
WALL_MINUTES=${BENCH_WALL_MINUTES:-30}
OUT=${BENCH_OUT:-bench/results/latest.md}
# Absolute from here on: the report is rendered inside a container with the
# report's own directory bind-mounted, and a relative path would be resolved
# against two different working directories.
case "$OUT" in
/*) ;;
*) OUT="$root/$OUT" ;;
esac

# The limits, in one place. Three profiles, so a reader sees a curve.
limits_for() {
	case "$1" in
	tiny) echo "1 512m" ;;
	small) echo "1 1g" ;;
	medium) echo "2 2g" ;;
	*)
		echo "unknown profile $1" >&2
		exit 2
		;;
	esac
}

# The commit the image is built from. --dirty, because a number measured over
# uncommitted changes must not claim the commit it was standing next to.
BENCH_COMMIT=$(git describe --tags --always --dirty 2>/dev/null || echo unknown)
# Docker tags are narrower than git's names, and this string becomes one.
BENCH_COMMIT=$(printf '%s' "$BENCH_COMMIT" | tr -c 'A-Za-z0-9_.-' '-')
# A throwaway secret for a throwaway database. Never read from the
# environment, never written to a file, and gone with the process: this rig
# must not be a reason a real SAPEDB_SECRET is sitting in anyone's shell
# history.
BENCH_SECRET=$(od -An -tx1 -N 32 /dev/urandom | tr -d ' \n')
# Placeholders so that `docker compose build`, which parses the whole file,
# has something for the two required limit variables. Every actual run
# overwrites both in up_profile.
BENCH_CPUS=1
BENCH_MEMORY=512m
export BENCH_COMMIT BENCH_SECRET BENCH_CPUS BENCH_MEMORY

raw=$(mktemp "${TMPDIR:-/tmp}/sapedb-bench.XXXXXX")
scratch=$(mktemp -d "${TMPDIR:-/tmp}/sapedb-bench-dir.XXXXXX")

cleanup() {
	status=$?
	if [ -n "${SAMPLER_PID:-}" ]; then kill "$SAMPLER_PID" 2>/dev/null || true; fi
	if [ "${BENCH_KEEP:-0}" = "1" ]; then
		echo "==> BENCH_KEEP=1, leaving the stack up. Raw records: $raw" >&2
	else
		$compose down -v --remove-orphans >/dev/null 2>&1 || true
		rm -f "$raw"
		rm -rf "$scratch"
	fi
	exit $status
}
trap cleanup EXIT INT TERM

say() { echo "==> $*" >&2; }

# JSON this script writes. Values are sanitised rather than escaped: none of
# them should ever contain a quote or a backslash, and a report is not worth
# an escaping routine in shell.
clean() { printf '%s' "$1" | tr -d '"\\' | tr '\n' ' '; }

# ---------------------------------------------------------------- host facts

docker_version=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)
docker_os=$(docker info --format '{{.OperatingSystem}}' 2>/dev/null || echo unknown)
docker_arch=$(docker info --format '{{.Architecture}}' 2>/dev/null || echo unknown)
docker_cpus=$(docker info --format '{{.NCPU}}' 2>/dev/null || echo unknown)
docker_mem=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo unknown)
host_kernel=$(uname -srm 2>/dev/null || echo unknown)
host_name=$(uname -n 2>/dev/null || echo unknown)
if [ "$(uname -s)" = "Darwin" ]; then
	host_name="macOS $(sw_vers -productVersion 2>/dev/null || echo '?') on $(uname -m)"
fi

printf '{"kind":"host","date":"%s","host":"%s","kernel":"%s","docker":"%s","dockerOs":"%s","arch":"%s","vmCpus":"%s","vmMemoryBytes":"%s","commit":"%s"}\n' \
	"$(date -u '+%Y-%m-%d %H:%M UTC')" \
	"$(clean "$host_name")" "$(clean "$host_kernel")" \
	"$(clean "$docker_version")" "$(clean "$docker_os")" "$(clean "$docker_arch")" \
	"$(clean "$docker_cpus")" "$(clean "$docker_mem")" "$(clean "$BENCH_COMMIT")" >>"$raw"

# --------------------------------------------------------------- the sampler

# Peak memory is read from outside the container, about once a second, and
# only for the duration of one phase. `docker stats --no-stream` costs the
# best part of a second itself, so the interval is roughly one second whatever
# the sleep says — which makes every peak below a lower bound. That is stated
# in the report rather than hidden.
sampler_start() {
	: >"$scratch/samples"
	(
		while :; do
			docker stats --no-stream --format '{{.MemUsage}}' sapedb-bench-server 2>/dev/null >>"$scratch/samples" || exit 0
			sleep 0.2
		done
	) &
	SAMPLER_PID=$!
}

sampler_stop_peak() {
	if [ -n "${SAMPLER_PID:-}" ]; then
		kill "$SAMPLER_PID" 2>/dev/null || true
		wait "$SAMPLER_PID" 2>/dev/null || true
		SAMPLER_PID=
	fi
	awk '
		{
			value = $1
			unit = value
			gsub(/[0-9.]/, "", unit)
			gsub(/[^0-9.]/, "", value)
			mult = 1
			if (unit == "KiB" || unit == "kB") mult = 1024
			else if (unit == "MiB" || unit == "MB") mult = 1048576
			else if (unit == "GiB" || unit == "GB") mult = 1073741824
			bytes = value * mult
			if (bytes > peak) peak = bytes
			seen++
		}
		END { printf "%d %d", peak, seen }
	' "$scratch/samples"
}

# ------------------------------------------------------------------ the rig

up_profile() {
	profile=$1
	set -- $(limits_for "$profile")
	BENCH_CPUS=$1
	BENCH_MEMORY=$2
	export BENCH_CPUS BENCH_MEMORY

	say "$profile: ${BENCH_CPUS} cpu, ${BENCH_MEMORY}, swap disabled"
	$compose down -v --remove-orphans >/dev/null 2>&1 || true
	$compose up -d server >/dev/null

	waited=0
	while :; do
		if $compose logs server 2>/dev/null | grep -q "listening on"; then break; fi
		waited=$((waited + 1))
		if [ "$waited" -gt 120 ]; then
			$compose logs server >&2 || true
			echo "the server never said it was listening" >&2
			exit 1
		fi
		sleep 0.5
	done

	# The limits as the daemon actually got them, read back rather than
	# assumed. A compose key that silently did nothing is the failure mode
	# this whole exercise is most exposed to.
	applied=$(docker inspect sapedb-bench-server \
		--format '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.NanoCpus}}')
	set -- $applied
	printf '{"kind":"limits","profile":"%s","askedCpus":"%s","askedMemory":"%s","memoryBytes":%s,"memorySwapBytes":%s,"nanoCpus":%s}\n' \
		"$profile" "$BENCH_CPUS" "$BENCH_MEMORY" "$1" "$2" "$3" >>"$raw"
	say "$profile: docker applied memory=$1 memory+swap=$2 nanocpus=$3"
	if [ "$1" != "$2" ]; then
		say "WARNING: memory and memory+swap differ, so this container can swap and its numbers are not trustworthy"
	fi
}

# phase <profile> <phase> [extra args...]
phase() {
	profile=$1
	name=$2
	shift 2

	sampler_start
	captured="$scratch/$profile.$name.out"
	set +e
	$compose run --rm -T bench \
		-phase "$name" -profile "$profile" -commit "$BENCH_COMMIT" \
		-runs "$RUNS" -rows "$ROWS" -page "$PAGE" -repeat "$REPEAT" "$@" >"$captured"
	code=$?
	set -e
	peak=$(sampler_stop_peak)
	set -- $peak

	if [ "$name" != "setup" ]; then
		printf '{"kind":"peak","profile":"%s","phase":"%s","peakBytes":%s,"peakSamples":%s}\n' \
			"$profile" "$name" "$1" "$2" >>"$raw"
	fi

	grep '^BENCHJSON ' "$captured" | sed 's/^BENCHJSON //' >>"$raw" || true

	if [ "$code" -ne 0 ]; then
		say "$profile/$name exited $code (see above); carrying on so the rest of the run is still recorded"
	fi
	return 0
}

# ---------------------------------------------------------------- the phases

$compose build >/dev/null
say "built from $BENCH_COMMIT"

for profile in $PROFILES; do
	up_profile "$profile"
	phase "$profile" setup
	phase "$profile" insert
	phase "$profile" scan
	phase "$profile" delete
done

if [ -n "$WALL_PROFILE" ]; then
	say "the wall, on $WALL_PROFILE"
	up_profile "$WALL_PROFILE"
	phase "$WALL_PROFILE" setup
	phase "$WALL_PROFILE" wall \
		-wallStep "$WALL_STEP" -wallRungs "$WALL_RUNGS" -wallMinutes "$WALL_MINUTES"

	# What docker says about that container now. This is the answer to "what
	# does the process do at the wall" that does not depend on the rig's own
	# interpretation of a broken socket.
	# RestartCount is here because the rig's own verdict depends on it. The
	# wall phase reconnects after a lost connection and reads "the server
	# answered again" as "it only hung up" — which would be a lie if something
	# had restarted the process in between. `restart: "no"` in the compose file
	# is the reason that cannot happen; this is the measurement that it did
	# not.
	state=$(docker inspect sapedb-bench-server \
		--format '{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}} {{.RestartCount}}' 2>/dev/null || echo "gone - - -")
	set -- $state
	printf '{"kind":"death","profile":"%s","status":"%s","exitCode":"%s","oomKilled":"%s","restarts":"%s"}\n' \
		"$WALL_PROFILE" "$1" "$2" "$3" "$4" >>"$raw"
	say "after the wall: status=$1 exit=$2 oomKilled=$3 restarts=$4"
fi

# ---------------------------------------------------------------- the report

mkdir -p "$(dirname "$OUT")"
cp "$raw" "${OUT%.md}.jsonl"
docker run --rm --user "$(id -u):$(id -g)" \
	-v "$(dirname "$OUT")":/out \
	"sapedb-bench-load:$BENCH_COMMIT" \
	-phase report -in "/out/$(basename "${OUT%.md}").jsonl" -out "/out/$(basename "$OUT")"

say "report: $OUT"
say "raw records: ${OUT%.md}.jsonl"
