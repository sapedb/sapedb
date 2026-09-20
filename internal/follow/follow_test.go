package follow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
)

const secret = "the secret only the control plane has"

// leader is a connection string that parses. Nothing here dials it.
func leader(t *testing.T) connection.Connection {
	t.Helper()

	where, err := connection.Parse("sapedb://acme:a-password-of-the-right-shape@127.0.0.1:1/main?sig=abc")
	if err != nil {
		t.Fatal(err)
	}
	return where
}

// TestFollowingIntoAServerThatStillTakesWritesIsRefused is the guard on the
// one configuration that goes wrong silently.
//
// A follower applies entries that were numbered by the leader. A server that
// also takes writes of its own hands out numbers of its own, and from the
// first local write the two sides' numbers mean different things — the next
// entry off the feed is refused as out of order, and what an operator sees is
// a follower that stopped for no visible reason, long after the write that did
// it. Refusing at the top of Run is the difference between finding out now and
// finding out then.
func TestFollowingIntoAServerThatStillTakesWritesIsRefused(t *testing.T) {
	writable, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writable.Close() }()

	// A deadline that would expire, so that a Run which did not refuse would
	// fail here rather than return the same nil a clean stop returns.
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()

	err = Run(ctx, Options{Leader: leader(t), Into: writable, Insecure: true, Retry: time.Millisecond})
	if err == nil {
		t.Fatal("following into a server that takes writes was allowed")
	}
	if !strings.Contains(err.Error(), "still takes writes") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if ctx.Err() != nil {
		t.Error("the refusal took as long as the deadline, so it was the deadline and not the refusal")
	}

	// The control: the same call against a read-only server does not refuse
	// for this reason. It cannot succeed — nothing is listening on port 1 —
	// so what is measured is that it keeps trying until the context stops it
	// and then returns nil, which is a different answer from the one above.
	readOnly, err := server.New(server.Options{Dir: t.TempDir(), Secret: secret, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnly.Close() }()

	short, give := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer give()

	var said []string
	if err := Run(short, Options{
		Leader: leader(t), Into: readOnly, Insecure: true, Retry: time.Millisecond,
		Notice: func(line string) { said = append(said, line) },
	}); err != nil {
		t.Fatalf("following a leader that is not there should be waited out, not returned: %v", err)
	}
	if len(said) == 0 {
		t.Error("a follower that could not reach its leader said nothing about it")
	}
	// What it said must not be the secret half of the connection string.
	for _, line := range said {
		if strings.Contains(line, "a-password-of-the-right-shape") || strings.Contains(line, "sig=abc") {
			t.Errorf("a follower printed the password or the signature: %q", line)
		}
	}
}
