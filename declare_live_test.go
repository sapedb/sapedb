package sapedb

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAnOperationIsDeclaredOnARunningDaemonAndCalledOnTheSameConnection is the
// whole reason the Declare frame exists, measured the only way that settles
// it: a real sapedbd process, started once and never stopped.
//
// Before this frame, store.DeclareOperation could only be reached through
// `sapedb apply`, and that command opens the database file directly and takes
// the exclusive lock — internal/cli's own package doc says a command run while
// the server is up is refused. So adding an operation meant stopping the
// daemon, applying, and starting it again: every live connection dropped for a
// change that writes one key. That is what made "many connections alive at
// once" and "add an operation" mutually exclusive, and it is what blocks a
// desktop client that holds a connection open.
//
// So the assertion is not only that Declare works. It is that the pid this
// test started at the top is the same pid still serving at the bottom, that it
// was never signalled, and that ONE connection carried the declaration and the
// call that used it.
//
// The daemon is the subject here, not the instrument. Nothing is measured with
// a command that ships with the product: the collection is seeded through
// internal/server in this process before the daemon starts, and everything
// after the daemon is up goes through this package's own client over a socket.
func TestAnOperationIsDeclaredOnARunningDaemonAndCalledOnTheSameConnection(t *testing.T) {
	dir := t.TempDir()

	// Seeded before the daemon exists, because a collection is not declarable
	// over the wire — only an operation is. This is the state of the world the
	// daemon is started on, not a measurement.
	signature := seedTheCollection(t, dir)

	daemon, address := startDaemon(t, dir)
	pid := daemon.Process.Pid

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	client, err := Dial(Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, Timeout: 10 * time.Second, RequestTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("dialling the daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}

	// The control that makes every failure below legible: this connection can
	// already talk to this database. A Declare that fails after this is a
	// Declare failing, not a connection that was never usable.
	if _, err := client.WhatIsHere(); err != nil {
		t.Fatalf("the connection cannot even read the catalogue: %v", err)
	}

	// 1. Declare an insert, on a daemon that has been running since before
	//    this test knew what it wanted to declare.
	add, err := client.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	})
	if err != nil {
		t.Fatalf("declaring notes.add over the wire: %v", err)
	}
	if add.Version != 1 {
		t.Fatalf("notes.add came back at version %d, want 1", add.Version)
	}

	// 2. Call it. This is the half that says the declaration is real to the
	//    running engine and not merely written down somewhere.
	written, err := client.Invoke("notes.add", map[string]any{"body": "the first note", "author": "ann"})
	if err != nil {
		t.Fatalf("invoking an operation declared a moment ago: %v", err)
	}
	if written.Key == nil {
		t.Fatal("the insert produced no key")
	}

	// 3. Declare a read, and get the document back through it.
	if _, err := client.Declare(Operation{
		Name: "notes.by_author", Collection: "notes", Action: store.ActionScan,
		Index: "by_author",
		Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "author"}}},
		Limit: 10,
	}); err != nil {
		t.Fatalf("declaring notes.by_author over the wire: %v", err)
	}

	read, err := client.Invoke("notes.by_author", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("invoking notes.by_author: %v", err)
	}
	if read.Count != 1 {
		t.Fatalf("the scan returned %d rows, want the 1 document just written: %+v", read.Count, read.Rows)
	}
	if got := read.Rows[0]["body"]; got != "the first note" {
		t.Fatalf("the row came back as %+v", read.Rows[0])
	}
	if got := read.Rows[0]["author"]; got != "ann" {
		t.Fatalf("the row came back as %+v", read.Rows[0])
	}

	// 4. Declare over the top of it. A redeclaration is a new version, so the
	//    unversioned call now runs the new one — live, on the same connection.
	again, err := client.Declare(Operation{
		Name: "notes.by_author", Collection: "notes", Action: store.ActionScan,
		Index: "by_author",
		Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "author"}}},
		Limit: 10, Projection: []string{"body"},
	})
	if err != nil {
		t.Fatalf("redeclaring notes.by_author: %v", err)
	}
	if again.Version != 2 {
		t.Fatalf("a redeclaration came back at version %d, want 2", again.Version)
	}

	projected, err := client.Invoke("notes.by_author", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Rows) != 1 || len(projected.Rows[0]) != 1 || projected.Rows[0]["body"] != "the first note" {
		t.Fatalf("the redeclared version did not take effect: %+v", projected.Rows)
	}

	// 5. And the daemon is the one this test started, still up, never
	//    signalled. Everything above happened while it was serving.
	if daemon.Process.Pid != pid {
		t.Fatalf("the daemon's pid changed from %d to %d", pid, daemon.Process.Pid)
	}
	if daemon.ProcessState != nil {
		t.Fatalf("the daemon exited during the test: %v", daemon.ProcessState)
	}
	if err := daemon.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the daemon is no longer running: %v", err)
	}
}

// liveSecret is the control-plane secret both the seeding server and the
// daemon are given, so a connection string signed by one verifies on the other.
const liveSecret = "the secret only the control plane has"

// seedTheCollection declares the notes collection in dir and hands back the
// signature a connection string for it needs. The server it opens is closed
// before it returns — the daemon takes the directory next.
func seedTheCollection(t *testing.T, dir string) string {
	t.Helper()

	srv, err := server.New(server.Options{Dir: dir, Secret: liveSecret})
	if err != nil {
		t.Fatal(err)
	}

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		_ = srv.Close()
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Spec{
		Name: "notes",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
		// Declared here for the same reason the index is: a collection is not
		// declarable over the wire, and the composed-operation test below
		// needs a rollup to read a real total out of.
		Rollups: []store.Rollup{{
			Name:  "per_author",
			Group: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
			Count: true,
		}},
	}); err != nil {
		release()
		_ = srv.Close()
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		release()
		_ = srv.Close()
		t.Fatal(err)
	}
	release()

	signature, err := srv.Sign("acme", wrapperPassword, "main")
	if err != nil {
		_ = srv.Close()
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("closing the seeding server: %v", err)
	}
	return signature
}

// startDaemon builds cmd/sapedbd and runs it on dir, returning the process and
// the address it announced.
func startDaemon(t *testing.T, dir string) (*exec.Cmd, string) {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "sapedbd")
	build := exec.Command(goTool(t), "build", "-o", binary, "./cmd/sapedbd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/sapedbd: %v\n%s", err, out)
	}

	daemon := exec.Command(binary)
	daemon.Env = append(os.Environ(),
		"SAPEDB_SECRET="+liveSecret,
		"SAPEDB_DIR="+dir,
		"SAPEDB_ADDR=127.0.0.1:0",
		"SAPEDB_INSECURE=1",
	)
	stdout, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})

	// The daemon announces the address it actually took, which is the only way
	// to learn a port that was asked for as zero.
	address := ""
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		line := lines.Text()
		t.Logf("sapedbd: %s", line)
		_, after, found := strings.Cut(line, "listening on ")
		if !found {
			continue
		}
		address, _, _ = strings.Cut(after, " ")
		break
	}
	if address == "" {
		t.Fatal("sapedbd never announced an address, so nothing below could have connected to it")
	}
	// Keep draining, or a chatty daemon blocks on a full pipe mid-test.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	return daemon, address
}

// goTool is the go command to build with: the one this test binary's own
// toolchain came from, or whatever is on PATH. It fails rather than skips —
// a test that quietly does not run is indistinguishable from one that passed.
func goTool(t *testing.T) string {
	t.Helper()

	if root := runtime.GOROOT(); root != "" {
		candidate := filepath.Join(root, "bin", "go")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	found, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command to build cmd/sapedbd with: %v", err)
	}
	return found
}

// TestAnOlderVersionIsStillCallableThroughThePublicPackage is the measurement
// behind InvokeVersion.
//
// The wire has carried a version since before this method existed — the
// server's `call` struct decodes it, store.Store.Invoke takes it — and nothing
// on the public surface could set it. So after a redeclaration the older
// versions were stored, readable and runnable by the engine, and unreachable
// by anybody holding this package: the declaration was there and there was no
// way to ask for it.
//
// Three calls make that concrete on one live daemon: the unversioned call
// (which follows the newest declaration wherever it goes), version 1, and
// version 2, all against the same three documents. If InvokeVersion sent no
// version, or sent the wrong one, all three answers would be the same — so
// the three declarations are deliberately given different limits, and the
// row counts are what tell them apart.
func TestAnOlderVersionIsStillCallableThroughThePublicPackage(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)
	daemon, address := startDaemon(t, dir)

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, Timeout: 10 * time.Second, RequestTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Operate(liveSecret); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two", "three"} {
		if _, err := client.Invoke("notes.add", map[string]any{"body": body, "author": "ann"}); err != nil {
			t.Fatal(err)
		}
	}

	scan := func(limit int) Operation {
		return Operation{
			Name: "notes.by_author", Collection: "notes", Action: store.ActionScan,
			Index: "by_author",
			Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
			From:  &Endpoint{Terms: []Term{{Arg: "author"}}},
			To:    &Endpoint{Terms: []Term{{Arg: "author"}}},
			Limit: limit,
		}
	}
	if got, err := client.Declare(scan(1)); err != nil || got.Version != 1 {
		t.Fatalf("declaring version 1: %+v %v", got, err)
	}
	if got, err := client.Declare(scan(2)); err != nil || got.Version != 2 {
		t.Fatalf("declaring version 2: %+v %v", got, err)
	}
	if got, err := client.Declare(scan(3)); err != nil || got.Version != 3 {
		t.Fatalf("declaring version 3: %+v %v", got, err)
	}

	for _, one := range []struct {
		version int
		rows    int
	}{
		{version: 0, rows: 3}, // the newest, which is what Invoke asks for
		{version: 1, rows: 1},
		{version: 2, rows: 2},
		{version: 3, rows: 3},
	} {
		result, err := client.InvokeVersion("notes.by_author", one.version, map[string]any{"author": "ann"})
		if err != nil {
			t.Fatalf("InvokeVersion(%d): %v", one.version, err)
		}
		if result.Count != one.rows {
			t.Errorf("InvokeVersion(%d) returned %d rows, want %d", one.version, result.Count, one.rows)
		}
		if one.version > 0 && result.Version != one.version {
			t.Errorf("InvokeVersion(%d) answered from version %d", one.version, result.Version)
		}
	}

	// Invoke's own signature is untouched and still means "the newest".
	newest, err := client.Invoke("notes.by_author", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if newest.Count != 3 || newest.Version != 3 {
		t.Fatalf("Invoke answered %d rows from version %d, want 3 rows from version 3", newest.Count, newest.Version)
	}

	// A version nobody declared is refused, not quietly answered from another.
	if result, err := client.InvokeVersion("notes.by_author", 99, map[string]any{"author": "ann"}); err == nil {
		t.Fatalf("version 99 was answered instead of refused: %+v", result)
	}

	if daemon.ProcessState != nil {
		t.Fatalf("the daemon exited during the test: %v", daemon.ProcessState)
	}
}

// TestAComposedOperationIsDeclaredOnARunningDaemonAndAnsweredInOneCall is the
// end-to-end measurement of task 0070's composed operation, and it is
// deliberately the same shape as the Declare test at the top of this file:
// one sapedbd process, started once and never signalled, one connection, and
// every assertion made through this package's own client over a socket.
//
// The subject is the thing composing was built to answer — "you are missing a
// scan-with-total, when do I get one?" A page of a shelf and that shelf's true
// total arrive together, out of two operations declared a moment earlier, over
// a connection that stayed open the whole time. Nothing measured here is
// measured with a command that ships with the product.
//
// What it does not measure is speed, and it says so rather than staying quiet:
// the comparison at the bottom counts CALLS, which is a number read off the
// declarations. Nobody has measured this across a real network, so nothing
// here claims a duration.
func TestAComposedOperationIsDeclaredOnARunningDaemonAndAnsweredInOneCall(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)

	daemon, address := startDaemon(t, dir)
	pid := daemon.Process.Pid

	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	client, err := Dial(Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, Timeout: 10 * time.Second, RequestTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("dialling the daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}

	// The control that makes every failure below legible.
	if _, err := client.WhatIsHere(); err != nil {
		t.Fatalf("the connection cannot even read the catalogue: %v", err)
	}

	// 1. The vocabulary: three flat operations, declared live.
	add, err := client.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	})
	if err != nil || add.Version != 1 {
		t.Fatalf("declaring notes.add: %+v %v", add, err)
	}
	byAuthor, err := client.Declare(Operation{
		Name: "notes.by_author", Collection: "notes", Action: store.ActionScan,
		Index: "by_author",
		Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:  &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:    &Endpoint{Terms: []Term{{Arg: "author"}}},
		Limit: 10,
	})
	if err != nil || byAuthor.Version != 1 {
		t.Fatalf("declaring notes.by_author: %+v %v", byAuthor, err)
	}
	perAuthor, err := client.Declare(Operation{
		Name: "notes.total_by_author", Collection: "notes", Action: store.ActionTotals,
		Rollup: "per_author",
		Input:  []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:   &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:     &Endpoint{Terms: []Term{{Arg: "author"}}},
		Limit:  1,
	})
	if err != nil || perAuthor.Version != 1 {
		t.Fatalf("declaring notes.total_by_author: %+v %v", perAuthor, err)
	}

	for _, body := range []string{"one", "two", "three"} {
		if _, err := client.Invoke("notes.add", map[string]any{"body": body, "author": "ann"}); err != nil {
			t.Fatalf("writing a note: %v", err)
		}
	}
	if _, err := client.Invoke("notes.add", map[string]any{"body": "elsewhere", "author": "bob"}); err != nil {
		t.Fatal(err)
	}

	// 2. The composed operation itself, declared over the same connection,
	//    pinned to the two versions that came back above.
	composed, err := client.Declare(Operation{
		Name: "notes.page", Collection: "notes", Action: store.ActionBatch,
		Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		Limit: 11,
		Steps: []Step{
			{Name: "page", Operation: "notes.by_author", Version: byAuthor.Version,
				With: map[string]Term{"author": {Arg: "author"}}},
			{Name: "total", Operation: "notes.total_by_author", Version: perAuthor.Version,
				With: map[string]Term{"author": {Arg: "author"}}},
		},
	})
	if err != nil {
		t.Fatalf("declaring a composed operation over the wire: %v", err)
	}
	if composed.Version != 1 {
		t.Fatalf("notes.page came back at version %d, want 1", composed.Version)
	}
	// The declaration came back carrying the fields that make it composed,
	// which is what says the wire did not quietly drop them on the way.
	if len(composed.Steps) != 2 ||
		composed.Steps[0].Operation != "notes.by_author" || composed.Steps[0].Version != 1 ||
		composed.Steps[0].With["author"].Arg != "author" {
		t.Fatalf("the stored declaration lost its composed fields: %+v", composed.Steps)
	}

	// 3. Call it. One Invoke, one answer, one state.
	result, err := client.Invoke("notes.page", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("invoking a composed operation declared a moment ago: %v", err)
	}
	if len(result.Rows) != 4 {
		t.Fatalf("got %d rows, want ann's 3 notes and 1 total: %+v", len(result.Rows), result.Rows)
	}
	for i, row := range result.Rows[:3] {
		if row["author"] != "ann" {
			t.Fatalf("row %d is not ann's: %+v", i, row)
		}
	}
	if got := result.Rows[3]["count"]; got != float64(3) {
		t.Fatalf("the total says %v, want 3: %+v", got, result.Rows[3])
	}

	// 4. The same answer out of the flat operations: two calls where the
	//    composed one took one. A count, not a duration.
	page, err := client.Invoke("notes.by_author", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	total, err := client.Invoke("notes.total_by_author", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows)+len(total.Rows) != len(result.Rows) {
		t.Fatalf("one composed call answered %d rows and the two flat calls answered %d + %d",
			len(result.Rows), len(page.Rows), len(total.Rows))
	}

	// 5. And the daemon is the one this test started, still up, never
	//    signalled — so all of the above happened on a running server.
	if daemon.Process.Pid != pid {
		t.Fatalf("the daemon's pid changed from %d to %d", pid, daemon.Process.Pid)
	}
	if daemon.ProcessState != nil {
		t.Fatalf("the daemon exited during the test: %v", daemon.ProcessState)
	}
	if err := daemon.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the daemon is no longer running: %v", err)
	}
}
