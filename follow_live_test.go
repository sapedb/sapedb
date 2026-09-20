package sapedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// Follower mode, measured against two real sapedbd processes.
//
// SAPE-26 joined the pieces in one process: a daemon wrote, this package's
// client read the change feed, and store.Apply rebuilt the database in the
// test's own memory. What this adds is the daemon half — a second sapedbd that
// does the subscribing and the applying itself, keeps its place across being
// killed, and refuses every write while it does.
//
// One connection string reaches both, and that is not a shortcut: the
// signature covers the account, the password and the database name, and
// deliberately not the host (see internal/connection). So the identical Invoke
// can be sent to the leader and to the follower, and the only thing that
// differs between the two answers is which database it reached.

// followerEnvironment is what makes a daemon a follower: where the leader is,
// and that it may be dialled without TLS.
func followerEnvironment(leader Connection) []string {
	return []string{
		"SAPEDB_FOLLOW=" + leader.String(),
		"SAPEDB_FOLLOW_INSECURE=1",
	}
}

// heard collects what a daemon printed, safely enough to be read from the test
// while the daemon is still running and written to after the test is over.
type heard struct {
	mutex sync.Mutex
	lines []string
}

func (h *heard) line(text string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.lines = append(h.lines, text)
}

func (h *heard) all() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return append([]string(nil), h.lines...)
}

// saying is the first line the daemon printed containing `what`, or "".
func (h *heard) saying(what string) string {
	for _, line := range h.all() {
		if strings.Contains(line, what) {
			return line
		}
	}
	return ""
}

// leaderConnection is the connection string for the daemon at `address`.
func leaderConnection(t *testing.T, address, signature string) Connection {
	t.Helper()

	where, err := Parse(fmt.Sprintf("sapedb://acme:%s@%s/main?sig=%s", wrapperPassword, address, signature))
	if err != nil {
		t.Fatalf("building the leader's connection string: %v", err)
	}
	return where
}

// heldAt is how far a running daemon's database has got, read off the welcome
// rather than out of the file — the file is locked while the daemon holds it.
func heldAt(t *testing.T, address, signature string) uint64 {
	t.Helper()

	client := dialDaemon(t, address, signature)
	defer func() { _ = client.Close() }()
	return client.Welcome().LSN
}

// caughtUp waits for a daemon to reach an entry, and fails rather than hangs.
func caughtUp(t *testing.T, address, signature string, to uint64, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	last := uint64(0)
	for time.Now().Before(deadline) {
		last = heldAt(t, address, signature)
		if last >= to {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the follower is at entry %d after %s and the leader is at %d", last, within, to)
}

// logOf is every entry of a database's change log, as the JSON it was stored
// as, keyed by its number.
//
// The whole entry, not a summary of it: comparing kinds or counting entries
// would agree between a follower that carried an operation's declaration and
// one that carried an empty one under the same number.
func logOf(t *testing.T, db *store.Store) map[uint64]string {
	t.Helper()

	out := map[uint64]string{}
	if err := db.Changes(0, func(change store.Change) bool {
		encoded, err := json.Marshal(change)
		if err != nil {
			t.Fatalf("entry %d: %v", change.LSN, err)
		}
		if _, twice := out[change.LSN]; twice {
			t.Fatalf("entry %d is in this log twice", change.LSN)
		}
		out[change.LSN] = string(encoded)
		return true
	}); err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	return out
}

// logDisagreements is everything two change logs do not agree on: an entry one
// has and the other does not, an entry both have that says something
// different, and a hole in the numbering.
//
// It returns what it found rather than failing, which is what lets the control
// below point it at logs that really do differ and check that it says so.
//
// Counting is the measurement this exists to avoid, twice over. A follower
// that missed entry 91 and later applied an entry of its own would hold the
// same NUMBER of entries as the leader while holding a different log; a
// follower that applied entry 91 twice would hold a document with the wrong
// value in it and still count one document.
func logDisagreements(leader, follower map[uint64]string) []string {
	var out []string

	for lsn, entry := range leader {
		switch mirror, ok := follower[lsn]; {
		case !ok:
			out = append(out, fmt.Sprintf("entry %d is on the leader and not on the follower", lsn))
		case mirror != entry:
			out = append(out, fmt.Sprintf("entry %d is %s on the leader and %s on the follower", lsn, entry, mirror))
		}
	}
	for lsn := range follower {
		if _, ok := leader[lsn]; !ok {
			out = append(out, fmt.Sprintf("entry %d is on the follower and not on the leader", lsn))
		}
	}

	// The numbering itself, on the follower's side. A log that is missing an
	// entry in the middle is a log with a hole in it whatever the comparison
	// above says, and this is the shape the store's own comment warns about:
	// "a replica with a gap in it is a replica that is wrong in a way nothing
	// will notice later".
	if len(follower) > 0 {
		highest := uint64(0)
		for lsn := range follower {
			if lsn > highest {
				highest = lsn
			}
		}
		for lsn := uint64(1); lsn <= highest; lsn++ {
			if _, ok := follower[lsn]; !ok {
				out = append(out, fmt.Sprintf("the follower has no entry %d, and it has entry %d", lsn, highest))
			}
		}
	}

	sort.Strings(out)
	return out
}

// openAfter opens a database directory once the daemon that held it is gone.
func openAfter(t *testing.T, dir string) *store.Store {
	t.Helper()

	srv, err := server.New(server.Options{Dir: dir, Secret: liveSecret})
	if err != nil {
		t.Fatalf("opening %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return db
}

// reachedOnDisk is how far a stopped daemon's database got, read out of the
// file and then let go of again.
//
// Let go of on purpose: the directory is locked while it is open, so a test
// that means to start that daemon again cannot leave this hanging around until
// a cleanup runs.
func reachedOnDisk(t *testing.T, dir string) uint64 {
	t.Helper()

	srv, err := server.New(server.Options{Dir: dir, Secret: liveSecret})
	if err != nil {
		t.Fatalf("opening %s: %v", dir, err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Fatalf("closing %s again: %v", dir, err)
		}
	}()

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	at, err := db.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// stop kills a daemon and waits for it, so that the directory lock it holds is
// gone before anything opens the file.
func stop(t *testing.T, daemon *exec.Cmd) {
	t.Helper()

	if err := daemon.Process.Kill(); err != nil {
		t.Fatalf("stopping a daemon: %v", err)
	}
	_, _ = daemon.Process.Wait()
}

// TestTheChangeLogComparisonSeesAGapAnExtraEntryAndAnEntryThatSaysSomethingElse
// is the control the measurement of a killed follower rests on.
//
// Every conclusion below is of the form "logDisagreements found nothing". If
// it would find nothing about a log with a hole in it, or one carrying an
// entry the leader never wrote, or one whose entry 2 is a different change
// under the same number, then a green run says nothing at all — and those
// three are exactly the failures that a follower losing its place produces.
func TestTheChangeLogComparisonSeesAGapAnExtraEntryAndAnEntryThatSaysSomethingElse(t *testing.T) {
	leader := map[uint64]string{1: `{"lsn":1}`, 2: `{"lsn":2}`, 3: `{"lsn":3}`}

	for _, one := range []struct {
		what     string
		follower map[uint64]string
		says     string
	}{
		{
			what:     "a gap in the middle",
			follower: map[uint64]string{1: `{"lsn":1}`, 3: `{"lsn":3}`},
			says:     "no entry 2",
		},
		{
			what:     "an entry the leader never wrote",
			follower: map[uint64]string{1: `{"lsn":1}`, 2: `{"lsn":2}`, 3: `{"lsn":3}`, 4: `{"lsn":4}`},
			says:     "entry 4 is on the follower and not on the leader",
		},
		{
			what:     "an entry that says something else",
			follower: map[uint64]string{1: `{"lsn":1}`, 2: `{"lsn":2,"kind":"put"}`, 3: `{"lsn":3}`},
			says:     "entry 2 is",
		},
	} {
		// Counting agrees in two of the three, which is the point of saying it
		// out loud rather than trusting a length.
		said := strings.Join(logDisagreements(leader, one.follower), "\n")
		if said == "" {
			t.Errorf("the comparison passed two logs differing by %s", one.what)
			continue
		}
		if !strings.Contains(said, one.says) {
			t.Errorf("the comparison saw %s but did not name it:\n%s", one.what, said)
		}
	}

	// And the other half of a control: it must say nothing about two logs that
	// really are the same one, or every measurement below would fail whatever
	// the follower did.
	if said := logDisagreements(leader, map[uint64]string{1: `{"lsn":1}`, 2: `{"lsn":2}`, 3: `{"lsn":3}`}); len(said) > 0 {
		t.Errorf("the comparison disagreed with itself:\n%s", strings.Join(said, "\n"))
	}
}

// TestAFollowerDaemonHoldsTheDatabaseItFollows is the first thing a follower
// has to do: catch up on what was written before it existed, keep up with what
// is written while it runs, and end up holding the same database.
//
// Compared document by document and declaration by declaration, never by
// counting — see disagreements, which has a control of its own in
// subscribe_live_test.go.
//
// The stream deliberately carries every kind a Change can be, because Change
// carries a Spec and an Operation as well as a Document: a follower that only
// handled documents would be wrong about exactly the half that decides what
// the documents mean. The kinds that actually travelled are counted before
// anything is concluded from the comparison.
func TestAFollowerDaemonHoldsTheDatabaseItFollows(t *testing.T) {
	binary := buildDaemon(t)

	leaderDir := t.TempDir()
	signature := seedTheCollection(t, leaderDir)
	leaderDaemon, leaderAt := runDaemon(t, binary, leaderDir, nil, nil)

	writer := dialDaemon(t, leaderAt, signature)
	if err := writer.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}

	// Written before any follower exists, so there is something to catch up on.
	if _, err := writer.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatalf("declaring notes.add: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := writer.Invoke("notes.add", map[string]any{
			"body": fmt.Sprintf("written before anybody followed %d", i), "author": "ann",
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	followerDir := t.TempDir()
	said := &heard{}
	followerDaemon, followerAt := runDaemon(t, binary, followerDir,
		followerEnvironment(leaderConnection(t, leaderAt, signature)), said.line)

	// The keeping-up half, all of it declared and written while somebody is
	// following: a collection over the wire, three operations (one declared
	// twice, so a version that is not the newest has to travel), and inserts,
	// an update and a delete through them.
	if _, err := writer.Establish(Spec{
		Name: "articles",
		Key:  Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []Index{{
			Name:   "by_author",
			Fields: []Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		t.Fatalf("establishing articles over the wire: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "articles.add", Collection: "articles", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "title", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"title": {Arg: "title"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatalf("declaring articles.add: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "articles.retitle", Collection: "articles", Action: store.ActionUpdate,
		Input: []Parameter{
			{Name: "id", Type: store.TypeString, Required: true},
			{Name: "title", Type: store.TypeString, Required: true},
		},
		Key: &Term{Arg: "id"},
		Set: map[string]Term{"title": {Arg: "title"}},
	}); err != nil {
		t.Fatalf("declaring articles.retitle: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "articles.remove", Collection: "articles", Action: store.ActionDelete,
		Input: []Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	}); err != nil {
		t.Fatalf("declaring articles.remove: %v", err)
	}
	again, err := writer.Declare(Operation{
		Name: "articles.remove", Collection: "articles", Action: store.ActionDelete,
		Input: []Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	})
	if err != nil {
		t.Fatalf("redeclaring articles.remove: %v", err)
	}
	if again.Version != 2 {
		t.Fatalf("the redeclaration came back at version %d, so there is no older version to carry", again.Version)
	}

	var keys []any
	for i := 0; i < 3; i++ {
		written, err := writer.Invoke("articles.add", map[string]any{
			"title": fmt.Sprintf("Article %d", i), "author": "bob",
		})
		if err != nil {
			t.Fatalf("inserting article %d: %v", i, err)
		}
		keys = append(keys, written.Key)
	}
	if updated, err := writer.Invoke("articles.retitle", map[string]any{
		"id": keys[0], "title": "Retitled after the fact",
	}); err != nil {
		t.Fatalf("updating an article: %v", err)
	} else if updated.Changed != 1 {
		t.Fatalf("the update changed %d documents", updated.Changed)
	}
	if removed, err := writer.Invoke("articles.remove", map[string]any{"id": keys[2]}); err != nil {
		t.Fatalf("deleting an article: %v", err)
	} else if removed.Changed != 1 {
		t.Fatalf("the delete changed %d documents", removed.Changed)
	}

	// How far the leader got, read off its own welcome.
	reached := heldAt(t, leaderAt, signature)
	if reached < 10 {
		t.Fatalf("the leader's log is at %d, which is too little to have measured anything", reached)
	}
	caughtUp(t, followerAt, signature, reached, 30*time.Second)

	// The daemon said which database it follows, which is the one line anybody
	// debugging a pair of these reads first.
	if line := said.saying("following"); line == "" {
		t.Errorf("the follower never said what it follows:\n%s", strings.Join(said.all(), "\n"))
	} else if strings.Contains(line, wrapperPassword) || strings.Contains(line, signature) {
		t.Errorf("the follower printed the password or the signature: %q", line)
	}

	stop(t, leaderDaemon)
	stop(t, followerDaemon)

	source, copied := openAfter(t, leaderDir), openAfter(t, followerDir)

	// Every kind that actually travelled, counted before anything is concluded
	// from the comparison: a stream of nothing but documents would satisfy a
	// comparison of documents and say nothing about a Spec or an Operation.
	kinds := map[string]int{}
	if err := copied.Changes(0, func(change store.Change) bool {
		kinds[change.Kind]++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{store.ChangePut, store.ChangeDelete, store.ChangeDeclare, store.ChangeOperation} {
		if kinds[kind] == 0 {
			t.Fatalf("no %q entry reached the follower, so applying one is not what this measured: %v", kind, kinds)
		}
	}

	if disagreed := disagreements(t, source, copied); len(disagreed) > 0 {
		t.Fatalf("the follower does not hold the database it follows:\n%s", strings.Join(disagreed, "\n"))
	}

	// The comparison above would pass two empty databases, so this says there
	// was something to compare.
	if held := len(documentsOf(t, copied)); held < 5 {
		t.Fatalf("the follower holds %d documents, which is not enough to have measured anything", held)
	}
	if declared := len(declarationsOf(t, copied)); declared < 2 {
		t.Fatalf("the follower holds %d collections, and two were declared", declared)
	}
}

// TestAFollowerKilledMidStreamComesBackWithoutAGapOrARepeat is the criterion
// this task exists for.
//
// A follower that is stopped between applying a change and writing down where
// it had got to comes back either having lost that change or having applied it
// twice — unless there is no "between", which is the claim being measured here.
// The place a follower resumes from is the database's own log counter, written
// by store.Apply into the same tree as the change and made durable by the same
// commit, so the two cannot be separated by a kill.
//
// Measured entry by entry against the leader's log, not by counting: the log
// store.Apply builds is numbered by the leader, so a follower that skipped one
// and carried on has a hole in its numbering, and one that applied a change
// twice holds a document the leader does not.
func TestAFollowerKilledMidStreamComesBackWithoutAGapOrARepeat(t *testing.T) {
	binary := buildDaemon(t)

	leaderDir := t.TempDir()
	signature := seedTheCollection(t, leaderDir)
	leaderDaemon, leaderAt := runDaemon(t, binary, leaderDir, nil, nil)
	leader := leaderConnection(t, leaderAt, signature)

	writer := dialDaemon(t, leaderAt, signature)
	if err := writer.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatalf("declaring notes.add: %v", err)
	}

	// Enough entries that a follower cannot apply all of them between two
	// polls, which is what makes "killed mid-stream" a measurement rather than
	// a hope. How mid-stream it really was is read off the file afterwards and
	// asserted, not assumed.
	const before = 600
	for i := 0; i < before; i++ {
		if _, err := writer.Invoke("notes.add", map[string]any{
			"body": fmt.Sprintf("before the follower was killed %d", i), "author": "ann",
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	total := heldAt(t, leaderAt, signature)
	if total < before {
		t.Fatalf("the leader's log is at %d after %d writes", total, before)
	}

	followerDir := t.TempDir()
	firstRun := &heard{}
	followerDaemon, followerAt := runDaemon(t, binary, followerDir, followerEnvironment(leader), firstRun.line)

	// Killed once it is a third of the way through and before it is finished,
	// so that the kill lands in the middle of the catch-up rather than at
	// either end of it. Where it actually got to is read off the file
	// afterwards and asserted; this loop only decides when to pull the plug.
	stopAfter := total / 3
	deadline := time.Now().Add(60 * time.Second)
	for {
		at := heldAt(t, followerAt, signature)
		if at >= stopAfter {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the follower is at entry %d and was waited on to reach %d", at, stopAfter)
		}
		time.Sleep(time.Millisecond)
	}
	stop(t, followerDaemon)

	// Where it actually got to, read out of its own file now that nothing
	// holds the lock. This is the measurement that says the kill landed
	// mid-stream: not "it was still running", but a number strictly between
	// nothing and everything.
	stoppedAt := reachedOnDisk(t, followerDir)
	if stoppedAt == 0 || stoppedAt >= total {
		t.Fatalf("the follower was at entry %d and the leader at %d, so it was not killed mid-stream and this test measured nothing",
			stoppedAt, total)
	}
	t.Logf("the follower was killed at entry %d of %d", stoppedAt, total)

	// Nothing beside the data holds the place. A cursor file next to the
	// database would be the thing that can be saved after a crash lost the
	// change, or before one lost it, and this says there is not one.
	held, err := os.ReadDir(filepath.Join(followerDir, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range held {
		if name := entry.Name(); name != "main.sapedb" && name != "main.parts" {
			t.Errorf("the follower keeps %q beside the database, so its place is not the database's own log counter", name)
		}
	}

	// The leader moves on while the follower is down, so coming back is not
	// just reconnecting to a feed that waited.
	const during = 40
	for i := 0; i < during; i++ {
		if _, err := writer.Invoke("notes.add", map[string]any{
			"body": fmt.Sprintf("while the follower was down %d", i), "author": "bea",
		}); err != nil {
			t.Fatalf("write %d while the follower was down: %v", i, err)
		}
	}
	ended := heldAt(t, leaderAt, signature)
	if ended <= total {
		t.Fatalf("the leader was at %d before the follower was down and is at %d after, so nothing was written", total, ended)
	}

	secondRun := &heard{}
	followerDaemon, followerAt = runDaemon(t, binary, followerDir, followerEnvironment(leader), secondRun.line)
	caughtUp(t, followerAt, signature, ended, 60*time.Second)

	// What it asked the leader for when it came back. The number is the one
	// measured off its file above, plus one: a follower that asked for entry 1
	// again would still end up correct here, because applying an entry already
	// held does nothing — so this is the line that tells the two apart.
	resumed := secondRun.saying("from entry")
	if resumed == "" {
		t.Errorf("the follower never said where it resumed from:\n%s", strings.Join(secondRun.all(), "\n"))
	} else if want := fmt.Sprintf("from entry %d;", stoppedAt+1); !strings.Contains(resumed, want) {
		t.Errorf("the follower said %q, and it was killed at entry %d so it had to resume at %d",
			resumed, stoppedAt, stoppedAt+1)
	}

	stop(t, leaderDaemon)
	stop(t, followerDaemon)

	source, copied := openAfter(t, leaderDir), openAfter(t, followerDir)

	leaderLog, followerLog := logOf(t, source), logOf(t, copied)
	if len(leaderLog) < before+during {
		t.Fatalf("the leader's log holds %d entries and %d writes went through it, so this compared the wrong thing",
			len(leaderLog), before+during)
	}
	if said := logDisagreements(leaderLog, followerLog); len(said) > 0 {
		shown := said
		if len(shown) > 10 {
			shown = shown[:10]
		}
		t.Fatalf("the follower's log is not the leader's, in %d ways:\n%s", len(said), strings.Join(shown, "\n"))
	}

	// And the data, field by field, because an entry applied twice is an entry
	// whose effect happened twice and its log would say nothing about it.
	if disagreed := disagreements(t, source, copied); len(disagreed) > 0 {
		t.Fatalf("the follower does not hold the database it follows:\n%s", strings.Join(disagreed, "\n"))
	}
}

// TestAWriteSentToAFollowerIsRefusedAndTheSameWriteOnTheLeaderIsNot is the
// third criterion, with its positive control first and on the same wire.
//
// The connection string is one string: the signature covers the account, the
// password and the database name and deliberately not the host, so the very
// same call is sent to both daemons and nothing about the caller differs
// between the two answers.
//
// The control matters more than usual here. A refusal proves nothing if the
// connection could not have written anyway — which is the trap the SAPE-26
// work fell into once, measuring a refusal that would have happened whatever
// it was testing.
func TestAWriteSentToAFollowerIsRefusedAndTheSameWriteOnTheLeaderIsNot(t *testing.T) {
	binary := buildDaemon(t)

	leaderDir := t.TempDir()
	signature := seedTheCollection(t, leaderDir)
	leaderDaemon, leaderAt := runDaemon(t, binary, leaderDir, nil, nil)

	writer := dialDaemon(t, leaderAt, signature)
	if err := writer.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatalf("declaring notes.add: %v", err)
	}
	// A read as well, so that the follower can be shown to serve rather than
	// merely to refuse. A follower that answered nothing at all would pass a
	// test that only measured refusals.
	if _, err := writer.Declare(Operation{
		Name: "notes.byAuthor", Collection: "notes", Action: store.ActionScan,
		Input: []Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		Index: "by_author", Limit: 10,
		From: &Endpoint{Terms: []Term{{Arg: "author"}}},
		To:   &Endpoint{Terms: []Term{{Arg: "author"}}},
	}); err != nil {
		t.Fatalf("declaring notes.byAuthor: %v", err)
	}

	followerDir := t.TempDir()
	followerDaemon, followerAt := runDaemon(t, binary, followerDir,
		followerEnvironment(leaderConnection(t, leaderAt, signature)), nil)
	t.Cleanup(func() { stop(t, followerDaemon); stop(t, leaderDaemon) })

	// The positive control, first: this exact call, on the leader, works.
	written, err := writer.Invoke("notes.add", map[string]any{
		"body": "a note the leader accepted", "author": "ann",
	})
	if err != nil {
		t.Fatalf("the leader refused the write, so a refusal on the follower proves nothing: %v", err)
	}
	if written.Changed != 1 {
		t.Fatalf("the leader changed %d documents", written.Changed)
	}

	caughtUp(t, followerAt, signature, heldAt(t, leaderAt, signature), 30*time.Second)

	onTheFollower := dialDaemon(t, followerAt, signature)

	// The second control: the follower is reachable and answers a read with
	// what the leader wrote. Without this, everything below could be a daemon
	// that refuses everything.
	found, err := onTheFollower.Invoke("notes.byAuthor", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("the follower refused a read, so it is not serving anything: %v", err)
	}
	if len(found.Rows) != 1 {
		t.Fatalf("the follower answered the read with %d documents, and the leader holds 1", len(found.Rows))
	}
	if body := fmt.Sprint(found.Rows[0]["body"]); !strings.Contains(body, "a note the leader accepted") {
		t.Fatalf("the follower answered with %q", body)
	}

	// And the refusal.
	_, err = onTheFollower.Invoke("notes.add", map[string]any{
		"body": "a note nobody should be able to write here", "author": "ann",
	})
	if err == nil {
		t.Fatal("the follower accepted a write; it would be lost, and its log would stop being the leader's")
	}
	refused := &Refused{}
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal does not read as one a client can tell apart: %v (%T)", err, err)
	}
	if refused.Code != "read_only" {
		t.Errorf("the refusal came back as %q, and a client switches on that code: %+v", refused.Code, refused)
	}
	// Not just "no": a caller told only that it may not write has nowhere to
	// go with that.
	for _, saying := range []string{"follows another one", "leader"} {
		if !strings.Contains(refused.Message, saying) {
			t.Errorf("the refusal does not say %q, so it does not say what to do about it: %q", saying, refused.Message)
		}
	}

	// Declaring is a write too — an entry in the log — and so is establishing.
	// Both are refused the same way, on a connection that has proved the
	// server's own secret, because holding the secret says what you may do and
	// not what this database is.
	operator := dialDaemon(t, followerAt, signature)
	if err := operator.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret to the follower: %v", err)
	}
	for _, one := range []struct {
		what string
		run  func() error
	}{
		{"declaring an operation", func() error {
			_, err := operator.Declare(Operation{
				Name: "notes.wrong", Collection: "notes", Action: store.ActionInsert,
				Input:    []Parameter{{Name: "body", Type: store.TypeString, Required: true}},
				Document: map[string]Term{"body": {Arg: "body"}},
			})
			return err
		}},
		{"establishing a collection", func() error {
			_, err := operator.Establish(Spec{
				Name: "wrong", Key: Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
			})
			return err
		}},
		{"reading the catalogue", func() error {
			_, err := operator.WhatIsHere()
			return err
		}},
	} {
		err := one.run()
		if err == nil {
			t.Errorf("%s on the follower was accepted, and it writes an entry to the log", one.what)
			continue
		}
		refused := &Refused{}
		if !errors.As(err, &refused) {
			t.Errorf("%s was refused in a way a client cannot tell apart: %v (%T)", one.what, err, err)
			continue
		}
		if refused.Code != "read_only" {
			t.Errorf("%s came back as %q: %+v", one.what, refused.Code, refused)
		}
	}

	// The last control, and the one that says the refusals above are about
	// this daemon rather than about these requests: every one of them works on
	// the leader.
	if _, err := writer.WhatIsHere(); err != nil {
		t.Errorf("reading the catalogue on the leader: %v", err)
	}
	if _, err := writer.Establish(Spec{
		Name: "allowed", Key: Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}); err != nil {
		t.Errorf("establishing a collection on the leader: %v", err)
	}
	if _, err := writer.Declare(Operation{
		Name: "notes.also", Collection: "notes", Action: store.ActionInsert,
		Input:    []Parameter{{Name: "body", Type: store.TypeString, Required: true}},
		Document: map[string]Term{"body": {Arg: "body"}},
	}); err != nil {
		t.Errorf("declaring an operation on the leader: %v", err)
	}
}
