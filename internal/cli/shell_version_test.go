package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// serverVersionForTest is what the server below is told to call itself:
// written out here by hand, and not a value anything else in this tree
// produces. If this were build.Version, the session could print the version
// of the tool and pass.
const serverVersionForTest = "7.7.7-the-server-not-the-shell"

// TestASessionSaysWhichBuildAnsweredOrSaysItCouldNotTell covers both answers
// the line can give, including the one no server in this repository can
// produce: a welcome with no product version, which is what every build from
// before this field sends.
//
// Measured live as well, against a daemon built from the commit before this
// change, which printed exactly the second line below. This is the part of
// that measurement a test can keep.
func TestASessionSaysWhichBuildAnsweredOrSaysItCouldNotTell(t *testing.T) {
	if got, want := connectedLine("1.2.3"), "connected to sapedb 1.2.3"; got != want {
		t.Errorf("a server that named its build gets %q, want %q", got, want)
	}

	silent := connectedLine("")
	if strings.Contains(silent, "connected to sapedb ") {
		t.Errorf("a server that named no build gets %q, which reads as a version it did not send", silent)
	}
	if !strings.Contains(silent, "does not say which build") {
		t.Errorf("a server that named no build gets %q, which does not say so", silent)
	}
}

// TestTheShellWritesDownWhichBuildAnsweredIt is the operator-facing half of
// the welcome carrying a product version: a connected client must not only be
// able to read it, it must actually put it somewhere a person keeps.
//
// The shell is where that lands, because the shell is the session an operator
// pastes into a bug report. Before this, such a report said which database
// was open and nothing whatsoever about what was serving it.
//
// It goes through shell(), not through Shell(), on purpose: Shell() is the
// loop and never sees a welcome. The line being measured is the one that
// exists precisely because the connection knows something the loop does not.
func TestTheShellWritesDownWhichBuildAnsweredIt(t *testing.T) {
	secret := "the-secret-this-server-was-started-with"

	live, err := server.New(server.Options{
		Dir: t.TempDir(), Secret: secret, ProductVersion: serverVersionForTest,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })

	// Something for the shell's opening catalogue request to find. Not what
	// is being measured, but a shell connecting to an empty database is a
	// less honest rehearsal of the thing than one connecting to a real one.
	db, release, err := live.Store("acme", "books")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "books",
		Key:  store.Key{Path: "id", Type: store.TypeString},
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
	go func() { _ = live.Serve(listener) }()

	out := &strings.Builder{}
	opts := options{secret: secret, account: "acme", db: "books"}
	if err := shell(opts, []string{listener.Addr().String(), "-insecure"}, strings.NewReader("exit\n"), out); err != nil {
		t.Fatalf("the shell session failed: %v", err)
	}

	printed := out.String()

	// The control: the session really ran and really reached the database.
	// Without it, a shell that failed silently before printing anything would
	// look the same as one that printed the wrong version, and both would
	// fail the assertion below for entirely different reasons.
	if !strings.Contains(printed, "books>") && !strings.Contains(printed, "sapedb books") {
		t.Fatalf("this is not a session that opened books, so nothing here is measuring what it wrote down:\n%s", printed)
	}

	if !strings.Contains(printed, "connected to sapedb "+serverVersionForTest) {
		t.Errorf("the session does not say which build answered it:\n%s", printed)
	}
}
