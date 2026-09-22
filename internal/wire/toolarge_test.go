package wire

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestAnAnswerThatDoesNotFitReachesThisClientAsACodeAndNotAsAnEOF is ISS-38's
// acceptance measured where the ticket says the defect is: at a client.
//
// What was measured on 737e149 was that asking for more rows than fit in a
// frame and asking for so many that the server was OOM-killed produced the
// same thing here — a bare io.EOF — and the benchmark rig could only tell
// them apart by redialling. So this asserts three things, and the third is
// the one that makes a redial unnecessary: the refusal is an *ErrRefused with
// a code, it is not an EOF, and the connection it arrived on still works.
//
// Nothing here sets a budget. The server sets its own when it opens the file
// (openFile, protocol.MaxPayload), which is the number under test — a test
// that lowered it would measure the mechanism and not the configuration.
func TestAnAnswerThatDoesNotFitReachesThisClientAsACodeAndNotAsAnEOF(t *testing.T) {
	const secret = "the secret only the control plane has"
	const password = "a-password-of-sixteen-characters-or-more"

	// Twenty-four rows of a megabyte apiece: more than one frame may carry,
	// in few enough documents that building the fixture is not what this test
	// spends its time on.
	const rows = 24
	const each = 1 << 20

	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := db.Declare(store.Caller{}, store.Spec{
		Name: "blobs",
		Key:  store.Key{Path: "id", Type: store.TypeString},
	})
	if err != nil {
		release()
		t.Fatal(err)
	}
	body := base64.StdEncoding.EncodeToString(make([]byte, each*3/4))
	for part := 0; part < rows; part++ {
		if _, err := collection.Put(map[string]any{
			"id":   fmt.Sprintf("blobs/%06d", part),
			"body": body,
		}); err != nil {
			release()
			t.Fatal(err)
		}
	}
	// A page that fits in one frame, and the declaration ISS-37 describes: a
	// limit written down once, far above what the collection held at the time.
	for _, operation := range []store.Operation{
		{Name: "blobs.page", Collection: "blobs", Action: store.ActionScan, Limit: 4},
		{Name: "blobs.everything", Collection: "blobs", Action: store.ActionScan, Limit: 1_000_000},
	} {
		if _, err := db.DeclareOperation(store.Caller{}, operation); err != nil {
			release()
			t.Fatal(err)
		}
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
	}, Options{Insecure: true, RequestTimeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// The control first. Four megabytes of rows is a real answer over a real
	// socket, and without it every assertion below is satisfied by a server
	// that refuses everything.
	page, err := client.Invoke("blobs.page", nil)
	if err != nil {
		t.Fatalf("a four row page was refused: %v", err)
	}
	if len(page.Rows) != 4 {
		t.Fatalf("the page came back with %d rows, not 4", len(page.Rows))
	}

	// The read the ticket measured.
	_, refused := client.Invoke("blobs.everything", nil)
	if refused == nil {
		t.Fatal("a twenty-four megabyte answer came back whole through a sixteen megabyte frame")
	}
	if errors.Is(refused, io.EOF) || errors.Is(refused, io.ErrUnexpectedEOF) {
		t.Fatalf("the refusal still arrives as an EOF, which is the whole of ISS-38: %v", refused)
	}

	told := &ErrRefused{}
	if !errors.As(refused, &told) {
		t.Fatalf("the refusal is not an *ErrRefused, so it carries no code at all: %T %v", refused, refused)
	}
	if told.Code != "too_large" {
		t.Fatalf("the refusal carries code %q, want %q", told.Code, "too_large")
	}
	if !strings.Contains(told.Message, "larger than one reply may carry") {
		t.Fatalf("the refusal reads %q, which does not say what happened", told.Message)
	}

	// And it is the BUDGET that refused, not the length check on the finished
	// bytes. Both wear the same code, on purpose — a client does the same
	// thing about either — but they are not the same event and only one of
	// them is ISS-37. A budget that refuses while the rows are still being
	// accumulated knows how many rows it got to; a check on a payload that
	// already exists knows only how many bytes it came to. So the row count
	// in the message is what tells the two apart from out here, and without
	// this assertion a server that had stopped setting a budget at all would
	// still satisfy every line above — measured, by removing the call in
	// openFile and watching this test stay green until this was added.
	if !strings.Contains(told.Message, "rows") {
		t.Fatalf("the refusal reads %q: it came from the length check on a finished payload, which means the server built the answer before refusing it", told.Message)
	}

	// And the sentence that makes the code worth having. A client does not
	// have to redial to find out whether the server is still there, because
	// the connection the refusal arrived on is still the connection.
	again, err := client.Invoke("blobs.page", nil)
	if err != nil {
		t.Fatalf("the connection did not survive the refusal, so a caller still cannot tell a refusal from a dead server: %v", err)
	}
	if len(again.Rows) != 4 {
		t.Fatalf("after the refusal the same page came back with %d rows", len(again.Rows))
	}
}
