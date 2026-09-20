package wire

import (
	"net"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
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
