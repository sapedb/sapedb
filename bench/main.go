// Command sapedb-bench is the load generator of the constrained-profile rig
// in this directory. It is not a general-purpose benchmark of sapedb; it is
// one workload, measured at one layer, under a memory limit.
//
// # The layer
//
// Everything here goes over the wire, through this repository's own Go client
// (package sapedb), against a sapedbd in a container of its own. That is the
// layer a number from this rig may be quoted at, and nothing else:
//
//   - one connection, one request outstanding at a time. The client has no
//     pipelining and no pool, says so in its own package documentation, and
//     this rig does not paper over it. Every rate below is therefore a
//     sequential round-trip rate, not a saturation figure — a server asked by
//     sixteen connections at once would answer differently, and this rig does
//     not know how differently.
//   - no TLS and no encryption at rest. SAPEDB_INSECURE=1, SAPEDB_ENCRYPT
//     unset. Both cost something; neither is in these numbers.
//   - the load generator is in its own container with no limits, so the
//     memory read off the server's container is the server's.
//
// # The workload
//
// The application this was written for stores a ten-megabyte file as about
// five thousand base64 rows of two kilobytes, under keys shaped
// <id>/000000. So one run here is one such file: `-rows` rows of exactly
// `-rowBytes` base64 characters under keys "<prefix>/%06d", and the read path
// is a prefix scan paginated with an exclusive `from` and a declared limit,
// which is what that application actually does.
//
// # The phases
//
// Each runs as a separate process against the same server, so the host script
// can watch the container's memory for one phase at a time:
//
//	setup    establish the collection and declare the operations
//	insert   -runs files of -rows rows each, timed per file
//	scan     page through each of those files -repeat times, each pass timed
//	delete   remove each of those rows, one request each, timed per file
//	wall     add rows until an unbounded read of them kills the server
//	report   render the collected JSON into Markdown (no server needed)
//
// Every phase but report prints exactly one line to stdout, beginning with
// "BENCHJSON ", and everything else to stderr. The host script needs no JSON
// parser to collect results, and this program needs no bind mount to hand
// them over.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sapedb/sapedb"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
)

// The names this rig uses inside the database. They are prefixed so that a
// database somebody points this at by accident is not silently sharing a
// collection name with it.
const (
	collectionName = "bench_chunks"
	opInsert       = "bench:chunk.put"
	opPage         = "bench:chunk.page"
	opDelete       = "bench:chunk.drop"
	opFlood        = "bench:chunk.flood"
)

// floodLimit is the declared limit of the unbounded read the wall phase uses.
//
// A scan must declare how many rows it may return — that declaration is what
// this store charges for a read — so "unbounded" here means "a limit far above
// anything the wall phase will put in the collection", not "no limit". The
// number is written down rather than computed so that the declaration in the
// database says the same thing every run.
const floodLimit = 5_000_000

// keyOf is the shape of a key in the workload being imitated: an id, a slash,
// and a six-digit chunk number.
func keyOf(prefix string, chunk int) string {
	return fmt.Sprintf("%s/%06d", prefix, chunk)
}

// prefixOf names one run's file. Zero-padded, so the prefixes sort in the
// order the runs happened and a scan of one never reaches another's rows.
func prefixOf(run int) string {
	return fmt.Sprintf("f%02d", run)
}

// upperOf is the inclusive high end of one prefix's stretch.
//
// '~' is 0x7E, above every digit and every character this rig puts in a key,
// and below nothing it uses — so "f00/~" sorts after "f00/999999" and before
// "f01/000000". It is a bound on a declared scan, not a key: nothing is ever
// stored under it.
func upperOf(prefix string) string { return prefix + "/~" }

func main() {
	var (
		phase    = flag.String("phase", "", "setup, insert, scan, delete, wall or report")
		profile  = flag.String("profile", "", "the name of the constrained profile this is running against")
		commit   = flag.String("commit", "", "the commit the server image was built from")
		host     = flag.String("host", "server", "where sapedbd is")
		port     = flag.Int("port", 7433, "its port")
		account  = flag.String("account", "bench", "account name")
		database = flag.String("db", "main", "database name")
		password = flag.String("password", "bench-password-not-a-secret", "connection-string password (16-128 of A-Za-z0-9._~-)")

		runs     = flag.Int("runs", 5, "how many files to write, read and delete")
		rows     = flag.Int("rows", 5000, "rows per file — 5000 two-kilobyte rows is about ten megabytes")
		rowBytes = flag.Int("rowBytes", 2048, "base64 characters per row")
		page     = flag.Int("page", 100, "declared limit of one page of the paginated read")
		repeat   = flag.Int("repeat", 20, "how many times the scan phase reads each file; every pass is one timed reading")

		wallStep    = flag.Int("wallStep", 5000, "rows added to the collection between attempts at the wall")
		wallRungs   = flag.Int("wallRungs", 40, "most attempts the wall phase makes before giving up")
		wallMinutes = flag.Int("wallMinutes", 25, "wall-clock budget for the wall phase")

		request = flag.Duration("requestTimeout", 10*time.Minute, "deadline on one request; a stalled server must not hang the rig forever")

		in  = flag.String("in", "", "report: the collected JSON lines")
		out = flag.String("out", "", "report: where to write the Markdown")
	)
	flag.Parse()

	if err := run(*phase, options{
		profile:  *profile,
		commit:   *commit,
		host:     *host,
		port:     *port,
		account:  *account,
		database: *database,
		password: *password,

		runs:     *runs,
		rows:     *rows,
		rowBytes: *rowBytes,
		page:     *page,
		repeat:   *repeat,

		wallStep:    *wallStep,
		wallRungs:   *wallRungs,
		wallMinutes: *wallMinutes,

		request: *request,

		in:  *in,
		out: *out,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sapedb-bench:", err)
		os.Exit(1)
	}
}

type options struct {
	profile  string
	commit   string
	host     string
	port     int
	account  string
	database string
	password string

	runs     int
	rows     int
	rowBytes int
	page     int
	repeat   int

	wallStep    int
	wallRungs   int
	wallMinutes int

	request time.Duration

	in  string
	out string
}

func run(phase string, o options) error {
	switch phase {
	case "report":
		return renderReport(o.in, o.out)
	case "setup":
		return withClient(o, func(c *sapedb.Client) error { return setup(c, o) })
	case "insert":
		return withClient(o, func(c *sapedb.Client) error { return insertPhase(c, o) })
	case "scan":
		return withClient(o, func(c *sapedb.Client) error { return scanPhase(c, o) })
	case "delete":
		return withClient(o, func(c *sapedb.Client) error { return deletePhase(c, o) })
	case "wall":
		return wallPhase(o)
	case "":
		return errors.New("-phase is required")
	}
	return fmt.Errorf("%q is not a phase this rig has", phase)
}

// dial builds the connection string this rig's server will accept and opens
// it.
//
// The signature is minted here rather than passed in because this rig holds
// the server's secret already — it is the thing that set SAPEDB_SECRET on the
// container. Nothing about that is how a real caller gets a connection string;
// a real caller is handed one by whoever holds the secret, and never holds it.
func dial(o options) (*sapedb.Client, error) {
	secret := os.Getenv("SAPEDB_SECRET")
	if secret == "" {
		return nil, errors.New("SAPEDB_SECRET is not set, so no connection string could be signed")
	}
	signature, err := signing.Sign(
		signing.Parts{AccountID: o.account, Password: o.password, DBName: o.database}, secret, "")
	if err != nil {
		return nil, fmt.Errorf("signing a connection string: %w", err)
	}
	where, err := sapedb.Parse(fmt.Sprintf("sapedb://%s:%s@%s:%d/%s?sig=%s",
		o.account, o.password, o.host, o.port, o.database, signature))
	if err != nil {
		return nil, fmt.Errorf("the connection string this rig built does not parse: %w", err)
	}
	return sapedb.Dial(where, sapedb.Options{
		Insecure:       true,
		Timeout:        30 * time.Second,
		RequestTimeout: o.request,
	})
}

func withClient(o options, do func(*sapedb.Client) error) error {
	client, err := dial(o)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return do(client)
}

// setup declares what every other phase invokes.
//
// Establishing a collection and declaring an operation both need an operator,
// which is why this is the one phase that proves the server secret. The
// measured phases invoke and nothing else, so none of them elevates — a
// benchmark that ran as an operator would be measuring a connection no
// application has.
func setup(client *sapedb.Client, o options) error {
	if err := client.Operate(os.Getenv("SAPEDB_SECRET")); err != nil {
		return fmt.Errorf("proving the server secret: %w", err)
	}

	if _, err := client.Establish(store.Spec{
		Name: collectionName,
		// No Auto: the workload supplies its own keys, because the key is
		// where in the file the chunk belongs.
		Key: store.Key{Path: "path", Type: store.TypeString},
		// No indexes on purpose. The read path is the primary key, which is
		// the order the documents are already stored in, and an index nobody
		// reads is a cost on every insert that would show up in the insert
		// number below as if it were the store's.
		Indexes: []store.Index{},
	}); err != nil {
		return fmt.Errorf("establishing %q: %w", collectionName, err)
	}

	declarations := []store.Operation{{
		Name:       opInsert,
		Collection: collectionName,
		Action:     store.ActionInsert,
		Input: []store.Parameter{
			{Name: "path", Type: store.TypeString, Required: true},
			{Name: "body", Type: store.TypeString, Required: true},
		},
		Document: map[string]store.Term{
			"path": {Arg: "path"},
			"body": {Arg: "body"},
		},
	}, {
		Name:       opPage,
		Collection: collectionName,
		Action:     store.ActionScan,
		// The clustered index: documents in primary-key order, which is the
		// order they are stored in, so this read walks pages rather than
		// hopping between an index and the documents it points at.
		Index: store.ClusteredIndex,
		Input: []store.Parameter{
			{Name: "from", Type: store.TypeString, Required: true},
			{Name: "to", Type: store.TypeString, Required: true},
		},
		From:       &store.Endpoint{Terms: []store.Term{{Arg: "from"}}, Exclusive: true},
		To:         &store.Endpoint{Terms: []store.Term{{Arg: "to"}}},
		Projection: []string{"path", "body"},
		Limit:      o.page,
	}, {
		Name:       opDelete,
		Collection: collectionName,
		Action:     store.ActionDelete,
		Input:      []store.Parameter{{Name: "path", Type: store.TypeString, Required: true}},
		Key:        &store.Term{Arg: "path"},
	}, {
		Name:       opFlood,
		Collection: collectionName,
		Action:     store.ActionScan,
		Index:      store.ClusteredIndex,
		Projection: []string{"path", "body"},
		Limit:      floodLimit,
	}}

	for _, declaration := range declarations {
		declared, err := client.Declare(declaration)
		if err != nil {
			return fmt.Errorf("declaring %q: %w", declaration.Name, err)
		}
		fmt.Fprintf(os.Stderr, "declared %s at version %d\n", declared.Name, declared.Version)
	}

	return emit(record{
		Kind:     "setup",
		Profile:  o.profile,
		Phase:    "setup",
		Commit:   o.commit,
		Server:   client.Welcome().ProductVersion,
		PageSize: o.page,
		RowBytes: o.rowBytes,
	})
}

// body is one row's payload: base64, of exactly the declared length.
//
// It comes from crypto/rand rather than from a repeated pattern so that
// nothing downstream — a page, a file, a filesystem — can compress it into
// being cheaper than the workload it stands for. The cost of generating it is
// paid before the clock starts; see insertPhase.
func body(length int) string {
	raw := make([]byte, base64.StdEncoding.DecodedLen(length)+3)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a
		// benchmark that quietly used a weaker payload would be worse than
		// one that stops.
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(raw)[:length]
}

// insertPhase writes -runs files of -rows rows, timing each file separately.
//
// The payloads for a file are built before its clock starts. They are the
// rig's cost, not the store's, and on the smallest profile generating fifty
// megabytes of base64 inside the timed section would have been a visible part
// of the number.
func insertPhase(client *sapedb.Client, o options) error {
	rates := make([]float64, 0, o.runs)
	durations := make([]time.Duration, 0, o.runs*o.rows)

	for r := 0; r < o.runs; r++ {
		prefix := prefixOf(r)
		payloads := make([]string, o.rows)
		for i := range payloads {
			payloads[i] = body(o.rowBytes)
		}

		started := time.Now()
		for i := 0; i < o.rows; i++ {
			at := time.Now()
			if _, err := client.Invoke(opInsert, map[string]any{
				"path": keyOf(prefix, i),
				"body": payloads[i],
			}); err != nil {
				return fmt.Errorf("inserting %s: %w", keyOf(prefix, i), err)
			}
			durations = append(durations, time.Since(at))
		}
		elapsed := time.Since(started)
		rate := float64(o.rows) / elapsed.Seconds()
		rates = append(rates, rate)
		fmt.Fprintf(os.Stderr, "insert %s: %d rows in %s, %.0f rows/s\n", prefix, o.rows, elapsed.Round(time.Millisecond), rate)
	}

	return emit(record{
		Kind:       "phase",
		Profile:    o.profile,
		Phase:      "insert",
		Commit:     o.commit,
		Server:     client.Welcome().ProductVersion,
		Runs:       o.runs,
		RowsPerRun: o.rows,
		RowBytes:   o.rowBytes,
		Rate:       spreadOf(rates),
		Series:     rates,
		Latency:    latencyOf(durations),
		Note:       "one connection, one request at a time; each run is one file of rows written in key order",
	})
}

// scanPhase pages through each file the way the application does: an
// exclusive `from` set to the last key of the previous page, a `to` at the end
// of this file's prefix, and a limit declared in the operation rather than
// passed at the call.
// Reading is so much cheaper than writing here that a single pass over every
// file is over in well under a second — shorter than the interval the host
// samples container memory at, which would leave this phase with no memory
// reading at all. So each file is read -repeat times and every pass is its own
// timed reading. That is not padding: a file served repeatedly is what the
// application does, and the first pass over each file is a cold read while the
// rest are warm, which is a spread worth seeing rather than one worth hiding.
func scanPhase(client *sapedb.Client, o options) error {
	rates := make([]float64, 0, o.runs*o.repeat)
	var durations []time.Duration
	pagesSeen := 0

	for pass := 0; pass < o.repeat; pass++ {
		for r := 0; r < o.runs; r++ {
			prefix := prefixOf(r)
			from := prefix + "/"
			upper := upperOf(prefix)

			seen, pages := 0, 0
			started := time.Now()
			for {
				at := time.Now()
				result, err := client.Invoke(opPage, map[string]any{"from": from, "to": upper})
				if err != nil {
					return fmt.Errorf("paging %s from %q: %w", prefix, from, err)
				}
				durations = append(durations, time.Since(at))
				pages++
				if len(result.Rows) == 0 {
					break
				}
				seen += len(result.Rows)
				last, ok := result.Rows[len(result.Rows)-1]["path"].(string)
				if !ok {
					return fmt.Errorf("paging %s: the last row of a page has no path: %v", prefix, result.Rows[len(result.Rows)-1])
				}
				from = last
				if !result.Truncated {
					break
				}
			}
			elapsed := time.Since(started)
			if seen != o.rows {
				return fmt.Errorf("paging %s read %d rows, and %d were written — a read that misses rows is not a read worth timing", prefix, seen, o.rows)
			}
			rate := float64(seen) / elapsed.Seconds()
			rates = append(rates, rate)
			pagesSeen += pages
			if pass == 0 || pass == o.repeat-1 {
				fmt.Fprintf(os.Stderr, "scan %s pass %d: %d rows over %d pages in %s, %.0f rows/s\n",
					prefix, pass+1, seen, pages, elapsed.Round(time.Millisecond), rate)
			}
		}
	}

	return emit(record{
		Kind:       "phase",
		Profile:    o.profile,
		Phase:      "scan",
		Commit:     o.commit,
		Server:     client.Welcome().ProductVersion,
		Runs:       o.runs,
		RowsPerRun: o.rows,
		RowBytes:   o.rowBytes,
		PageSize:   o.page,
		Pages:      pagesSeen,
		Rate:       spreadOf(rates),
		Latency:    latencyOf(durations),
		Note:       fmt.Sprintf("each file read %d times, every pass timed on its own; the min of the spread is a cold read and the max a warm one. Latency here is one page, not one row, and every row written was read back before a pass counted as complete", o.repeat),
	})
}

// deletePhase removes the rows one request at a time.
//
// This is the baseline SAPE-32's deleteRange will be compared against, so what
// it measures is stated rather than implied: one declared delete per row, each
// its own request, each its own transaction and its own entry in the change
// log. A range delete that did the same work in one transaction is not
// competing with this number on the walk — it is competing on the per-row
// round trip and the per-row commit, which is most of what is being timed
// here.
func deletePhase(client *sapedb.Client, o options) error {
	rates := make([]float64, 0, o.runs)
	durations := make([]time.Duration, 0, o.runs*o.rows)

	for r := 0; r < o.runs; r++ {
		prefix := prefixOf(r)
		started := time.Now()
		for i := 0; i < o.rows; i++ {
			at := time.Now()
			result, err := client.Invoke(opDelete, map[string]any{"path": keyOf(prefix, i)})
			if err != nil {
				return fmt.Errorf("deleting %s: %w", keyOf(prefix, i), err)
			}
			if result.Changed != 1 {
				return fmt.Errorf("deleting %s changed %d documents, want 1", keyOf(prefix, i), result.Changed)
			}
			durations = append(durations, time.Since(at))
		}
		elapsed := time.Since(started)
		rate := float64(o.rows) / elapsed.Seconds()
		rates = append(rates, rate)
		fmt.Fprintf(os.Stderr, "delete %s: %d rows in %s, %.0f rows/s\n", prefix, o.rows, elapsed.Round(time.Millisecond), rate)
	}

	return emit(record{
		Kind:       "phase",
		Profile:    o.profile,
		Phase:      "delete",
		Commit:     o.commit,
		Server:     client.Welcome().ProductVersion,
		Runs:       o.runs,
		RowsPerRun: o.rows,
		RowBytes:   o.rowBytes,
		Rate:       spreadOf(rates),
		Series:     rates,
		Latency:    latencyOf(durations),
		Note:       "one declared delete per row, one request each — the baseline SAPE-32's deleteRange is to be compared against",
	})
}

// Rung is one attempt at the wall: how many rows were in the collection, and
// what the server did when asked for all of them at once.
type Rung struct {
	Rows     int     `json:"rows"`
	Bytes    int64   `json:"approxRowBytes"`
	Outcome  string  `json:"outcome"`
	Returned int     `json:"returned,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	Seconds  float64 `json:"seconds"`
}

// wallPhase finds out what a small machine does when a caller asks for more
// than it has.
//
// The shape of the question matters. A read here declares a limit, and the
// limit is the declaration of what the read costs — so the only way to ask for
// more than the machine has is to declare a limit far above what is stored and
// then keep storing. That is exactly the mistake an application makes: an
// operation declared once, against a collection that was small then.
//
// It climbs rather than guessing: add a file, ask for everything, look at what
// came back. It stops at the first rung the server does not survive, and
// reconnects afterwards to find out whether the server is still there at all —
// "the request failed" and "the process is gone" are different answers and a
// rig that could not tell them apart would have measured nothing.
func wallPhase(o options) error {
	rungs, verdict, detail := climb(o)

	// Whatever happened, find out whether the server is still answering. A
	// fresh connection, because the one the climb held may be a socket the
	// other end has already forgotten.
	alive := "gone"
	if again, err := dial(o); err == nil {
		alive = "answering"
		_ = again.Close()
	} else {
		detail = strings.TrimSpace(detail + " | redial afterwards: " + trim(err.Error()))
	}

	return emit(record{
		Kind:     "wall",
		Profile:  o.profile,
		Phase:    "wall",
		Commit:   o.commit,
		RowBytes: o.rowBytes,
		Rungs:    rungs,
		Verdict:  verdict,
		Alive:    alive,
		Note:     detail,
	})
}

// climb is the ladder itself: a file of rows, then an attempt at all of them,
// until something stops it.
//
// A lost connection is not on its own the wall. The server may have refused by
// hanging up — a frame it could not send is still a frame it could not send —
// and a rig that stopped there would report a connection error as if it were a
// death. So a lost connection is followed by a redial: if the server answers,
// the rung is recorded as a hang-up and the climb continues; if it does not,
// the climb is over and that is the wall.
func climb(o options) (rungs []Rung, verdict, detail string) {
	deadline := time.Now().Add(time.Duration(o.wallMinutes) * time.Minute)
	stored := 0
	verdict, detail = "survived", ""

	client, err := dial(o)
	if err != nil {
		return nil, "unreachable", trim(err.Error())
	}
	defer func() { _ = client.Close() }()

	for rung := 0; rung < o.wallRungs; rung++ {
		if time.Now().After(deadline) {
			return rungs, "budget_spent",
				fmt.Sprintf("the wall was not reached within %d minutes; the collection held %d rows", o.wallMinutes, stored)
		}

		// Another file's worth of rows on top of what is already there. One
		// payload reused across the file: the wall phase is about how much
		// the server holds when it reads them back, and generating a fresh
		// two kilobytes per row would put the rig's own cost in the way of
		// getting there.
		payload := body(o.rowBytes)
		wrote := 0
		for i := 0; i < o.wallStep; i++ {
			if _, err := client.Invoke(opInsert, map[string]any{
				"path": keyOf("wall", stored+i),
				"body": payload,
			}); err != nil {
				rungs = append(rungs, Rung{
					Rows:    stored + wrote,
					Bytes:   int64(stored+wrote) * int64(o.rowBytes),
					Outcome: "died_writing",
					Detail:  trim(err.Error()),
				})
				return rungs, "died_writing",
					fmt.Sprintf("the insert at row %d did not come back: %s", stored+wrote, trim(err.Error()))
			}
			wrote++
		}
		stored += wrote
		fmt.Fprintf(os.Stderr, "wall: %d rows stored, asking for all of them\n", stored)

		at := time.Now()
		result, err := client.Invoke(opFlood, nil)
		this := Rung{
			Rows:    stored,
			Bytes:   int64(stored) * int64(o.rowBytes),
			Seconds: time.Since(at).Seconds(),
		}
		switch {
		case err == nil:
			this.Outcome = "answered"
			this.Returned = len(result.Rows)
		case refused(err):
			// The server said no and is still there to say it. A refusal is
			// not the wall; it is a guard in front of it, and the climb goes
			// on to find out whether the guard actually saves the machine.
			this.Outcome = "refused"
			this.Detail = trim(err.Error())
		default:
			this.Outcome = "lost"
			this.Detail = trim(err.Error())
		}
		rungs = append(rungs, this)
		fmt.Fprintf(os.Stderr, "wall: %d rows -> %s %s\n", stored, this.Outcome, this.Detail)

		if this.Outcome != "lost" {
			continue
		}

		_ = client.Close()
		again, err := dial(o)
		if err != nil {
			rungs[len(rungs)-1].Outcome = "died_reading"
			return rungs, "died_reading",
				fmt.Sprintf("at %d rows the read took the server with it: %s | redial: %s",
					stored, this.Detail, trim(err.Error()))
		}
		rungs[len(rungs)-1].Outcome = "hung_up"
		client = again
	}

	return rungs, verdict,
		fmt.Sprintf("the server answered or refused every one of the %d rungs, holding %d rows at the end",
			len(rungs), stored)
}

// refused says whether the server answered with a refusal rather than going
// away. A refusal is the server's own word about a request; anything else here
// is a socket that stopped working, which is what a container being killed
// looks like from this side.
func refused(err error) bool {
	by := &sapedb.Refused{}
	return errors.As(err, &by)
}

// trim keeps an error short enough to sit in a table cell and on one line.
func trim(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}

// record is one line of the rig's output. Every phase writes exactly one, and
// so does the host script — see report.go for the kinds it writes.
type record struct {
	Kind    string `json:"kind"`
	Profile string `json:"profile,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Server  string `json:"server,omitempty"`

	Runs       int `json:"runsRequested,omitempty"`
	RowsPerRun int `json:"rowsPerRun,omitempty"`
	RowBytes   int `json:"rowBytes,omitempty"`
	PageSize   int `json:"pageSize,omitempty"`
	Pages      int `json:"pages,omitempty"`

	Rate    Spread  `json:"rowsPerSecond,omitempty"`
	Latency Latency `json:"latency,omitempty"`

	// Series is every reading in the order it was taken, for the phases whose
	// readings are few enough to be read one by one. A spread says where the
	// readings landed and nothing about the order they landed in — and a phase
	// whose rate falls run after run looks, in min/median/max alone, exactly
	// like one that was merely noisy. Those are different facts about a store
	// and only one of them is a problem.
	Series []float64 `json:"rowsPerSecondByReading,omitempty"`

	Rungs   []Rung `json:"rungs,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	Alive   string `json:"serverAfterwards,omitempty"`

	Note string `json:"note,omitempty"`

	// Written by the host script, not by any phase above. They are in the
	// same struct because they land in the same file and are rendered by the
	// same reader; a second struct would only mean a second decoder.
	Host          string `json:"host,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	Docker        string `json:"docker,omitempty"`
	DockerOS      string `json:"dockerOs,omitempty"`
	Arch          string `json:"arch,omitempty"`
	VMCPUs        string `json:"vmCpus,omitempty"`
	VMMemoryBytes string `json:"vmMemoryBytes,omitempty"`
	Date          string `json:"date,omitempty"`

	MemoryBytes     int64  `json:"memoryBytes,omitempty"`
	MemorySwapBytes int64  `json:"memorySwapBytes,omitempty"`
	NanoCPUs        int64  `json:"nanoCpus,omitempty"`
	AskedCPUs       string `json:"askedCpus,omitempty"`
	AskedMemory     string `json:"askedMemory,omitempty"`

	PeakBytes    int64 `json:"peakBytes,omitempty"`
	PeakSamples  int   `json:"peakSamples,omitempty"`
	SampleEveryS int   `json:"sampleEverySeconds,omitempty"`

	OOMKilled string `json:"oomKilled,omitempty"`
	ExitCode  string `json:"exitCode,omitempty"`
	Status    string `json:"status,omitempty"`
	Restarts  string `json:"restarts,omitempty"`
}

// emit writes the one line this phase exists to produce.
func emit(r record) error {
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(os.Stdout)
	if _, err := fmt.Fprintf(writer, "BENCHJSON %s\n", encoded); err != nil {
		return err
	}
	return writer.Flush()
}
