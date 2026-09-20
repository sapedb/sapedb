package sapedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/vfs"
)

// A change that is produced and never consumed is a log nobody has proved is
// a log. The engine has been able to apply one for some time — store.Apply,
// and internal/store's own replica tests — and the server has carried
// Subscribe and Event frames for as long, with a handler and tests of its
// own. What had never happened anywhere is the join: an Event frame coming
// off a socket, turning back into the change the server wrote, and being
// applied. No client in this repository could read an Event at all.
//
// So what is measured here is the whole path, on a real sapedbd process: a
// database is written to over one connection, followed over another, and the
// changes that arrive are applied into a blank engine in this process until
// the two databases are the same one — compared document by document and
// declaration by declaration, never by counting.

// disagreements is everything two databases do not agree on: which
// collections exist, how each is declared, every document field by field, and
// every version of every declared operation.
//
// It returns what it found rather than failing, which is what lets a test
// point it at two databases that really do differ and check that it says so.
// Counting is the measurement this exists to avoid: a document that arrived
// missing a field is still one document on each side, and a count of
// documents agrees while the data does not.
func disagreements(t *testing.T, first, second *store.Store) []string {
	t.Helper()

	var out []string

	here, there := declarationsOf(t, first), declarationsOf(t, second)
	for name, declaration := range here {
		switch other, ok := there[name]; {
		case !ok:
			out = append(out, fmt.Sprintf("collection %q is declared here and not there", name))
		case other != declaration:
			out = append(out, fmt.Sprintf("collection %q is declared %s here and %s there", name, declaration, other))
		}
	}
	for name := range there {
		if _, ok := here[name]; !ok {
			out = append(out, fmt.Sprintf("collection %q is declared there and not here", name))
		}
	}

	ours, theirs := documentsOf(t, first), documentsOf(t, second)
	for at, document := range ours {
		mirror, ok := theirs[at]
		if !ok {
			out = append(out, fmt.Sprintf("%s is here and not there", at))
			continue
		}
		for field, value := range document {
			held, carried := mirror[field]
			switch {
			case !carried:
				out = append(out, fmt.Sprintf("%s carries %q here and not there", at, field))
			case fmt.Sprint(held) != fmt.Sprint(value):
				out = append(out, fmt.Sprintf("%s %q is %v here and %v there", at, field, value, held))
			}
		}
		for field := range mirror {
			if _, carried := document[field]; !carried {
				out = append(out, fmt.Sprintf("%s carries %q there and not here", at, field))
			}
		}
	}
	for at := range theirs {
		if _, ok := ours[at]; !ok {
			out = append(out, fmt.Sprintf("%s is there and not here", at))
		}
	}

	mine, yours := operationsOf(t, first), operationsOf(t, second)
	for at, declaration := range mine {
		switch other, ok := yours[at]; {
		case !ok:
			out = append(out, fmt.Sprintf("operation %s is declared here and not there", at))
		case other != declaration:
			out = append(out, fmt.Sprintf("operation %s is declared %s here and %s there", at, declaration, other))
		}
	}
	for at := range yours {
		if _, ok := mine[at]; !ok {
			out = append(out, fmt.Sprintf("operation %s is declared there and not here", at))
		}
	}

	sort.Strings(out)
	return out
}

// declarationsOf is every collection of a database as it is declared, ids and
// counters included: a follower that installed the declaration and numbered it
// differently is a follower whose documents are somewhere else.
func declarationsOf(t *testing.T, db *store.Store) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, name := range db.Collections() {
		collection, err := db.Collection(name)
		if err != nil {
			t.Fatalf("collection %q: %v", name, err)
		}
		encoded, err := json.Marshal(collection.Spec())
		if err != nil {
			t.Fatalf("declaration of %q: %v", name, err)
		}
		out[name] = string(encoded)
	}
	return out
}

// documentsOf is every document, addressed by collection and key, kept as a
// map so the comparison can go field by field.
func documentsOf(t *testing.T, db *store.Store) map[string]map[string]any {
	t.Helper()

	out := map[string]map[string]any{}
	for _, name := range db.Collections() {
		collection, err := db.Collection(name)
		if err != nil {
			t.Fatalf("collection %q: %v", name, err)
		}
		if err := collection.Walk(func(key any, document map[string]any) bool {
			out[fmt.Sprintf("%s/%v", name, key)] = document
			return true
		}); err != nil {
			t.Fatalf("walk %q: %v", name, err)
		}
	}
	return out
}

// operationsOf is every version of every declared operation — every version,
// because a caller is built against one and a follower missing an older one
// refuses calls the database it followed answers.
//
// A version that cannot be read back is left out rather than failing here, so
// that it shows as a missing declaration in the comparison instead of as a
// dead test.
func operationsOf(t *testing.T, db *store.Store) map[string]string {
	t.Helper()

	newest, err := db.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}

	out := map[string]string{}
	for _, operation := range newest {
		for version := 1; version <= operation.Version; version++ {
			one, found, err := db.Operation(operation.Name, version)
			if err != nil || !found {
				continue
			}
			encoded, err := json.Marshal(one)
			if err != nil {
				t.Fatalf("operation %q version %d: %v", operation.Name, version, err)
			}
			out[fmt.Sprintf("%s@%d", operation.Name, version)] = string(encoded)
		}
	}
	return out
}

// TestTheComparisonSeesADocumentThatLostAField is the control the measurement
// below rests on.
//
// Two databases hold one document each, under the same key, and one of them is
// missing a field. Counting says they match. If the comparison says they match
// too, then every green run of the test below means nothing, and the failure
// it exists to catch — a change that arrived and left a field behind — would
// pass unseen.
func TestTheComparisonSeesADocumentThatLostAField(t *testing.T) {
	first, second := blank(t, 1), blank(t, 2)
	for _, db := range []*store.Store{first, second} {
		if _, err := db.Declare(store.Caller{}, store.Spec{
			Name: "notes", Key: store.Key{Path: "id", Type: store.TypeString},
		}); err != nil {
			t.Fatal(err)
		}
	}

	here, err := first.Collection("notes")
	if err != nil {
		t.Fatal(err)
	}
	there, err := second.Collection("notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := here.Put(map[string]any{"id": "n1", "body": "whole", "author": "ann"}); err != nil {
		t.Fatal(err)
	}
	if _, err := there.Put(map[string]any{"id": "n1", "body": "whole"}); err != nil {
		t.Fatal(err)
	}

	if counted, mirrored := len(documentsOf(t, first)), len(documentsOf(t, second)); counted != mirrored {
		t.Fatalf("this control is about a difference counting cannot see, and counting saw %d against %d", counted, mirrored)
	}

	said := disagreements(t, first, second)
	if len(said) == 0 {
		t.Fatal("the comparison passed two databases whose one document differs by a field")
	}
	if !strings.Contains(strings.Join(said, "\n"), `"author"`) {
		t.Errorf("the comparison did not name the missing field:\n%s", strings.Join(said, "\n"))
	}
}

// blank is an empty database in this process, to apply somebody else's changes
// into.
func blank(t *testing.T, seed int64) *store.Store {
	t.Helper()

	pages, err := pager.Create(vfs.NewSim(seed, vfs.Faults{}), 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	db, err := store.Open(pages)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

// dialDaemon opens this package's own client against a running daemon.
func dialDaemon(t *testing.T, address, signature string) *Client {
	t.Helper()

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
	return client
}

// streaming reads a subscription in the background, so a test that is wrong
// fails on a deadline instead of hanging forever.
type streaming struct {
	changes chan Change
	failed  chan error
	stop    chan struct{}
}

func stream(t *testing.T, client *Client) *streaming {
	t.Helper()

	live := &streaming{
		changes: make(chan Change, 4096),
		failed:  make(chan error, 1),
		stop:    make(chan struct{}),
	}
	t.Cleanup(func() { close(live.stop) })

	go func() {
		for {
			change, err := client.NextChange()
			if err != nil {
				select {
				case live.failed <- err:
				case <-live.stop:
				}
				return
			}
			select {
			case live.changes <- change:
			case <-live.stop:
				return
			}
		}
	}()
	return live
}

// next is the next change, or a failure of the test rather than a hang.
func (s *streaming) next(t *testing.T, within time.Duration) Change {
	t.Helper()

	select {
	case change := <-s.changes:
		return change
	case err := <-s.failed:
		t.Fatalf("the subscription ended: %v", err)
	case <-time.After(within):
		t.Fatal("no change arrived")
	}
	return Change{}
}

// TestAChangeStreamedFromARunningDaemonRebuildsTheDatabase is the join this
// task is about: a real sapedbd process as the source, this package's client
// as the reader, and store.Apply as the consumer.
//
// The daemon is the subject, not the instrument. Nothing here is measured
// through a command that ships with the product: the first collection is
// seeded through internal/server before the daemon starts, everything after
// that goes over a socket, and the comparison at the end opens the daemon's
// own file once the daemon is gone.
//
// The stream deliberately carries all three kinds of change that a Change can
// hold, because Change carries Spec and Operation as well as Document and a
// follower that only handles documents is one that is wrong about exactly the
// half that decides what the documents mean:
//
//   - establish: a collection declared over the wire, mid-subscription;
//   - declare: operations declared over the wire, two of them, one twice so
//     that a version that is not the newest has to travel;
//   - insert, update and delete of documents through those operations.
func TestAChangeStreamedFromARunningDaemonRebuildsTheDatabase(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)

	daemon, address := startDaemon(t, dir)
	pid := daemon.Process.Pid

	writer := dialDaemon(t, address, signature)
	if err := writer.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}

	// The control that makes every failure below legible: this connection can
	// already talk to this database.
	if _, err := writer.WhatIsHere(); err != nil {
		t.Fatalf("the connection cannot even read the catalogue: %v", err)
	}

	// Something to catch up on, written before anybody is following.
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

	// A second connection, because one carrying a subscription serves no
	// requests. It never calls Operate: following a database is not an
	// operator's privilege on this server, and this says so out loud rather
	// than leaving it to be discovered.
	reader := dialDaemon(t, address, signature)
	following, err := reader.Subscribe(1)
	if err != nil {
		t.Fatalf("subscribing from the first entry: %v", err)
	}
	if following.From != 1 || following.Oldest != 1 {
		t.Errorf("the subscription reports %+v, want it starting at the first entry the log still keeps", following)
	}
	if following.Latest < 4 {
		t.Fatalf("the log is at %d, which is fewer entries than were written before subscribing", following.Latest)
	}
	caughtUpTo := following.Latest

	live := stream(t, reader)
	replica := blank(t, 300)

	// The catch-up half: everything written before the subscription existed.
	for applied := uint64(0); applied < caughtUpTo; applied++ {
		change := live.next(t, 15*time.Second)
		if err := replica.Apply(change); err != nil {
			t.Fatalf("applying %d (%s): %v", change.LSN, change.Kind, err)
		}
	}
	if at, err := replica.LatestLSN(); err != nil {
		t.Fatal(err)
	} else if at != caughtUpTo {
		t.Fatalf("the catch-up left the replica at %d, and the log was at %d", at, caughtUpTo)
	}

	// The keeping-up half: a collection, two operations and five documents,
	// all declared and written while somebody is following.
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
	// Declared twice, so that version 1 is an older version the follower has
	// to carry rather than the only one there is.
	if _, err := writer.Declare(Operation{
		Name: "articles.remove", Collection: "articles", Action: store.ActionDelete,
		Input: []Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	}); err != nil {
		t.Fatalf("declaring articles.remove: %v", err)
	}
	second, err := writer.Declare(Operation{
		Name: "articles.remove", Collection: "articles", Action: store.ActionDelete,
		Input: []Parameter{{Name: "id", Type: store.TypeString, Required: true}},
		Key:   &Term{Arg: "id"},
	})
	if err != nil {
		t.Fatalf("redeclaring articles.remove: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("the redeclaration came back at version %d, so there is no older version to carry", second.Version)
	}

	var keys []any
	for i := 0; i < 3; i++ {
		written, err := writer.Invoke("articles.add", map[string]any{
			"title": fmt.Sprintf("Article %d", i), "author": "bob",
		})
		if err != nil {
			t.Fatalf("inserting article %d: %v", i, err)
		}
		if written.Key == nil {
			t.Fatalf("inserting article %d produced no key", i)
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

	// How far to read to. Asked on a connection of its own, because the one
	// that is following cannot be asked anything and the one that writes would
	// record a read of its own if it explored.
	ruler := dialDaemon(t, address, signature)
	ends, err := ruler.Subscribe(1)
	if err != nil {
		t.Fatalf("asking how far the log has got: %v", err)
	}
	if err := ruler.Close(); err != nil {
		t.Fatalf("closing the connection that only asked: %v", err)
	}
	if ends.Latest <= caughtUpTo {
		t.Fatalf("the log was at %d before the live half and is at %d after it, so nothing was written live",
			caughtUpTo, ends.Latest)
	}

	for applied := caughtUpTo; applied < ends.Latest; applied++ {
		change := live.next(t, 15*time.Second)
		if err := replica.Apply(change); err != nil {
			t.Fatalf("applying %d (%s): %v", change.LSN, change.Kind, err)
		}
	}

	// Every kind that travelled, counted before anything is concluded from
	// it: a stream of nothing but documents would satisfy a comparison of
	// documents, and say nothing about the half of Change that carries a Spec
	// or an Operation.
	kinds := map[string]int{}
	if err := replica.Changes(0, func(change store.Change) bool {
		kinds[change.Kind]++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{store.ChangePut, store.ChangeDelete, store.ChangeDeclare, store.ChangeOperation} {
		if kinds[kind] == 0 {
			t.Fatalf("no %q entry came down the subscription, so applying one is not what this measured: %v", kind, kinds)
		}
	}

	// The daemon is stopped before its file is opened, because it holds the
	// lock while it runs. The pid is the one started at the top: nothing here
	// restarted it to make a declaration take effect, which is the whole
	// point of declaring over the wire.
	if daemon.Process.Pid != pid {
		t.Fatalf("the daemon serving at the end is pid %d and the one started was %d", daemon.Process.Pid, pid)
	}
	if err := daemon.Process.Kill(); err != nil {
		t.Fatalf("stopping the daemon: %v", err)
	}
	_, _ = daemon.Process.Wait()

	srv, err := server.New(server.Options{Dir: dir, Secret: liveSecret})
	if err != nil {
		t.Fatalf("opening the daemon's own database: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	source, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if said := disagreements(t, source, replica); len(said) > 0 {
		t.Fatalf("the database rebuilt from the subscription is not the one it followed:\n%s", strings.Join(said, "\n"))
	}

	// The comparison above would pass two empty databases, so this says there
	// was something to compare.
	if held := len(documentsOf(t, replica)); held < 5 {
		t.Fatalf("the replica holds %d documents, which is not enough to have measured anything", held)
	}
	if declared := len(declarationsOf(t, replica)); declared < 2 {
		t.Fatalf("the replica holds %d collections, and two were declared", declared)
	}

	at, err := replica.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	there, err := source.LatestLSN()
	if err != nil {
		t.Fatal(err)
	}
	if at != there {
		t.Errorf("the daemon's log is at %d and the replica's at %d", there, at)
	}
}

// TestASubscriptionFromAnEntryTheLogDoesNotHaveIsRefused is the negative, and
// the positive control comes first in the same test on the same daemon: a
// subscription that the server accepts, then one it must not.
//
// Entry 0 is the case a caller reaches by assuming logs are counted from zero.
// The server refuses it as too_far_behind rather than reading it as "the
// beginning", which is the answer that matters: a follower quietly started
// somewhere other than where it asked is one that believes it has seen changes
// it has not.
func TestASubscriptionFromAnEntryTheLogDoesNotHaveIsRefused(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)
	_, address := startDaemon(t, dir)

	// The control. Without it, a refusal below could be a connection that was
	// never able to subscribe at all.
	control := dialDaemon(t, address, signature)
	following, err := control.Subscribe(1)
	if err != nil {
		t.Fatalf("subscribing from the first entry was refused, so a refusal proves nothing: %v", err)
	}
	if following.Latest == 0 {
		t.Fatal("the daemon's log is empty, so there is nothing for a subscription to be behind")
	}

	behind := dialDaemon(t, address, signature)
	_, err = behind.Subscribe(0)
	if err == nil {
		t.Fatal("subscribing from entry 0 was accepted; a follower would be started somewhere it did not ask for")
	}
	refused := &Refused{}
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal does not read as one the client can tell apart: %v (%T)", err, err)
	}
	if refused.Code != "too_far_behind" {
		t.Errorf("the refusal came back as %q, and a client switches on that code: %+v", refused.Code, refused)
	}
}

// TestASubscriptionFromAnEntryThatWasTrimmedIsRefused is the case the retention
// cap exists to make possible and the one it makes dangerous: a follower that
// has been away longer than the log is kept.
//
// The server here is in this process rather than a separate one, for a reason
// that is itself worth writing down: nothing in the product sets a retention
// cap. store.Retain exists, no server option reaches it, and so a daemon's log
// is never trimmed. That means this refusal cannot be produced against
// sapedbd at all today — it is reachable only by an embedder who sets the cap,
// which is what this test does. See the note in CHANGELOG.
func TestASubscriptionFromAnEntryThatWasTrimmedIsRefused(t *testing.T) {
	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: wrapperSecret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = srv.Serve(listener) }()

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Two entries kept, so that everything written after them ages the first
	// ones out from under a follower.
	db.Retain(2)
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "notes", Key: store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input:    []store.Parameter{{Name: "body", Type: store.TypeString, Required: true}},
		Document: map[string]store.Term{"body": {Arg: "body"}},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	signature, err := srv.Sign("acme", wrapperPassword, "main")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	where := Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}

	writer, err := Dial(where, Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	// The control, before anything has aged out: entry 1 is still there and a
	// subscription from it is accepted.
	control, err := Dial(where, Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	if _, err := control.Subscribe(1); err != nil {
		t.Fatalf("subscribing from entry 1 while entry 1 is still kept: %v", err)
	}

	for i := 0; i < 6; i++ {
		if _, err := writer.Invoke("notes.add", map[string]any{"body": fmt.Sprintf("note %d", i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// The cap has to have actually bitten, or the refusal below would be the
	// refusal entry 0 gets on any database and this test would be measuring
	// nothing. Asked from an entry far ahead of the log, which is accepted
	// (a consumer that has seen everything is not behind), purely to read back
	// where the log now starts.
	probe, err := Dial(where, Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	where1000, err := probe.Subscribe(1000)
	if err != nil {
		t.Fatalf("asking where the log now starts: %v", err)
	}
	if where1000.Oldest <= 1 {
		t.Fatalf("the log still starts at %d, so nothing was trimmed and the refusal below would be about entry 0, not about falling behind", where1000.Oldest)
	}

	// And the negative: the very entry the control subscribed from a moment
	// ago, now that it has aged out.
	behind, err := Dial(where, Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = behind.Close() })

	following, err := behind.Subscribe(1)
	if err == nil {
		t.Fatalf("a subscription from a trimmed entry was accepted: %+v", following)
	}
	refused := &Refused{}
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal does not read as one the client can tell apart: %v (%T)", err, err)
	}
	if refused.Code != "too_far_behind" {
		t.Errorf("the refusal came back as %q: %+v", refused.Code, refused)
	}
	if !strings.Contains(refused.Message, "start again from a dump") {
		t.Errorf("the refusal does not say what to do about it: %q", refused.Message)
	}
}

// TestAConnectionCarryingASubscriptionServesNothingElse is the rule the
// client's own shape depends on.
//
// Everything else in this client is one request and one answer read straight
// off the socket. A subscription puts frames on that socket which nothing
// asked for, so a request issued alongside one reads an event where it expects
// its answer. Refusing by name is the difference between a caller being told
// and a caller being handed somebody else's payload.
func TestAConnectionCarryingASubscriptionServesNothingElse(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)
	_, address := startDaemon(t, dir)

	client := dialDaemon(t, address, signature)

	// The control: this connection answers requests before it subscribes.
	if err := client.Operate(liveSecret); err != nil {
		t.Fatalf("proving the server secret: %v", err)
	}
	if _, err := client.WhatIsHere(); err != nil {
		t.Fatalf("the connection cannot read the catalogue before subscribing: %v", err)
	}

	if _, err := client.Subscribe(1); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	_, err := client.WhatIsHere()
	if err == nil {
		t.Fatal("a request on a connection carrying a subscription was accepted")
	}
	if !strings.Contains(err.Error(), "subscription") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
