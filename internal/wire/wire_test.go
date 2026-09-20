package wire

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestRequestTimeoutBoundsAHandshakeAgainstASilentPeer is the positive proof
// task 0070 §2 asked for before anything about the timeout could be believed:
// a listener that accepts a connection and then never says a word gives the
// handshake nothing to read, which — before RequestTimeout — was an
// unbounded hang. Options.Timeout does not help here: the TCP connect itself
// succeeds the moment the listener accepts, so Dial has already moved past
// the only thing Timeout ever bounded.
func TestRequestTimeoutBoundsAHandshakeAgainstASilentPeer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Accepted, and now silent on purpose: no frame is ever written back,
		// and the connection is held open (not closed) until the test cleans
		// up, so the client's read blocks on a live socket rather than
		// failing fast on one the peer already closed.
		<-stop
		_ = conn.Close()
	}()

	address := listener.Addr().(*net.TCPAddr)
	const bound = 500 * time.Millisecond

	started := time.Now()
	_, err = Dial(connection.Connection{
		Account: "acme", Password: "x", Host: address.IP.String(), Port: address.Port,
		DBName: "main", Signature: "whatever-a-silent-server-never-checks",
	}, Options{Insecure: true, RequestTimeout: bound})
	took := time.Since(started)

	<-accepted // make sure the goroutine really did accept before asserting

	if err == nil {
		t.Fatal("Dial against a peer that never answers the handshake returned no error at all")
	}
	// The generous half of the margin is for a busy CI box; the point of the
	// assertion is "closer to `bound` than to unbounded", not a tight bound.
	if took > 3*bound {
		t.Fatalf("Dial took %v against a %v RequestTimeout: it is still hanging, not timing out", took, bound)
	}
	t.Logf("Dial against a silent peer returned after %v (RequestTimeout %v)", took, bound)
}

// TestRequestTimeoutStillSucceedsAgainstAnOrdinaryServer is the control case
// the task's own measurement traps warn to run BEFORE trusting the timeout
// above: the same RequestTimeout, on the same request path, against a server
// that actually answers. If this did not pass, the test above would not be
// proving what it claims — a Dial that fails against everything proves
// nothing about silence in particular.
func TestRequestTimeoutStillSucceedsAgainstAnOrdinaryServer(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.New(server.Options{Dir: dir, Secret: "the secret only the control plane has"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = srv.Serve(listener) }()

	const password = "a-password-of-sixteen-characters-or-more"
	signature, err := srv.Sign("acme", password, "main")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().(*net.TCPAddr)
	client, err := Dial(connection.Connection{
		Account: "acme", Password: password, Host: address.IP.String(), Port: address.Port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Dial against a real, answering server failed: %v", err)
	}
	defer client.Close()

	if client.Welcome().Account != "acme" {
		t.Fatalf("welcome names account %q, not acme", client.Welcome().Account)
	}
}

// TestPresentingAGrantReachesAnOperationThatDeclaresAScope is this client's
// half of the scope mechanism, against a real server over a real socket.
//
// internal/server's own tests drive a hand-rolled client that builds the
// request struct directly, which measures the server and not this package. The
// field name, its shape, and the decision to omit it entirely when nothing was
// presented all live here, and none of them is checked by a test that never
// sends these bytes.
//
// Both halves are on one connection, and the order matters: the operation is
// refused before Present and answered after it, so what changed is the grant
// and not anything about the connection, the declaration or the data.
func TestPresentingAGrantReachesAnOperationThatDeclaresAScope(t *testing.T) {
	const secret = "the secret only the control plane has"
	const password = "a-password-of-sixteen-characters-or-more"

	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// A collection is not declarable over the wire, so it is seeded in this
	// process before anything is measured.
	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "notes",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = srv.Serve(listener) }()

	signature, err := srv.Sign("acme", password, "main")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	client, err := Dial(connection.Connection{
		Account: "acme", Password: password, Host: address.IP.String(), Port: address.Port,
		DBName: "main", Signature: signature,
	}, Options{Insecure: true, RequestTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Operate(secret); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Declare(store.Operation{
		Name: "notes.add", Collection: "notes", Action: store.ActionInsert,
		Input: []store.Parameter{
			{Name: "body", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]store.Term{"body": {Arg: "body"}, "author": {Arg: "author"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Declare(store.Operation{
		Name: "notes.guarded", Collection: "notes", Action: store.ActionScan,
		Index: "by_author", Scopes: []string{"notes:read"},
		Input: []store.Parameter{{Name: "author", Type: store.TypeString, Required: true}},
		From:  &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
		To:    &store.Endpoint{Terms: []store.Term{{Arg: "author"}}},
		Limit: 10,
	}); err != nil {
		t.Fatal(err)
	}

	// The control: a connection presenting nothing can still do everything it
	// could before grants existed. If this fails, nothing below is about
	// scopes.
	if _, err := client.Invoke("notes.add", map[string]any{"body": "a note", "author": "ann"}); err != nil {
		t.Fatalf("an unscoped operation failed on a connection presenting nothing: %v", err)
	}

	// The negative, before Present.
	refused := &ErrRefused{}
	if _, err := client.Invoke("notes.guarded", map[string]any{"author": "ann"}); err == nil {
		t.Fatal("a scoped operation ran for a client presenting no grant")
	} else if !errors.As(err, &refused) || refused.Code != "not_allowed" {
		t.Fatalf("refused with %v, want the not_allowed code", err)
	}

	// The positive, on the same connection, after presenting a grant minted by
	// whoever holds the secret.
	expires := time.Now().Add(time.Hour)
	signature, grantErr := srv.Grant("acme", "main", []string{"notes:read"}, expires, "wire-0001")
	if err := grantErr; err != nil {
		t.Fatal(err)
	}
	held := Grant{Scopes: []string{"notes:read"}, Expires: expires, Serial: "wire-0001", Signature: signature}
	client.Present(held)

	result, err := client.Invoke("notes.guarded", map[string]any{"author": "ann"})
	if err != nil {
		t.Fatalf("a scoped operation was refused to a client presenting a real grant: %v", err)
	}
	if len(result.Rows) == 0 {
		t.Fatal("the scoped operation was allowed to run and came back with no rows — an empty answer cannot be told from a refusal nobody reported")
	}

	// A list this client edits after the fact is a list the signature no
	// longer covers, and the server says so with the grant code rather than
	// letting the call go on to fail for some other true-but-wrong reason.
	edited := held
	edited.Scopes = []string{"notes:read", "notes:write"}
	client.Present(edited)
	if _, err := client.Invoke("notes.guarded", map[string]any{"author": "ann"}); err == nil {
		t.Fatal("a client added a scope to a real grant and the call ran")
	} else if !errors.As(err, &refused) || refused.Code != "grant" {
		t.Fatalf("refused with %v, want the grant code", err)
	}

	// Clearing it puts the connection back where it started, which is what
	// says Present is state on this client and not a one-way door.
	client.Present(Grant{})
	if _, err := client.Invoke("notes.guarded", map[string]any{"author": "ann"}); err == nil {
		t.Fatal("clearing the grant left the scope in place")
	} else if !errors.As(err, &refused) || refused.Code != "not_allowed" {
		t.Fatalf("refused with %v, want the not_allowed code", err)
	}
}
