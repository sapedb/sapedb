// Package follow keeps one database a copy of another one.
//
// It is the daemon half of replication, and it is deliberately the smaller
// half: the protocol was already there. internal/wire can subscribe to a
// leader's change log and read entries off it, internal/store can apply an
// entry keeping its own number, and internal/server can refuse every write
// while that is happening. What was missing was the loop that joins them and
// survives being stopped in the middle.
//
// Where the follower keeps its place is the decision worth reading twice,
// because it is the one that decides whether a restart loses a change or
// repeats one.
//
// It keeps no place. There is no cursor file, no marker key of this package's
// own, nothing beside the data — because two things that must agree and are
// written separately are two things that will one day disagree. Applying an
// entry already writes the log counter (store.Apply calls setLSN, into the
// same tree, inside the same transaction as the document it changed), and one
// commit makes the change and the number durable together or neither of them.
// So the place a follower resumes from is the database's own LatestLSN, plus
// one — the same number the leader would give the next entry, read out of the
// same file the changes went into.
//
// That removes both halves of the trap rather than balancing them. Crash after
// applying and before saving the position: impossible, they are one write.
// Crash after saving and before applying: impossible, same reason. Reconnect
// and be sent entries already held: store.Apply ignores an entry at or below
// what is there, so they cost nothing and change nothing.
//
// What this package does NOT do, said out loud so nobody plans around a
// silence. Replication is asynchronous: the leader does not wait for a
// follower and does not know whether one is keeping up. A follower never
// promotes itself — becoming a leader is an operator's decision and there is
// nothing here that could make it. Nothing merges: a follower takes no writes,
// so there is no conflict to resolve. Nothing measures lag. Several followers
// of one leader do not know about each other. Each of those is a thing nobody
// has needed yet, not an oversight.
package follow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/wire"
)

// DefaultRetry is how long to wait before dialling a leader again.
//
// A follower that reconnects as fast as it can is a follower that turns a
// leader being restarted into a flood, and it buys nothing: replication is
// asynchronous, so being a second behind is already the normal state.
const DefaultRetry = time.Second

// Options are what a follower needs.
type Options struct {
	// Leader is the connection string of the database to follow, taken apart.
	// The account and database name in it are the ones written to locally as
	// well: a copy of acme/main is acme/main.
	Leader connection.Connection

	// Into is the server holding the copy. It should have been opened with
	// ReadOnly set — see Run, which refuses one that was not.
	Into *server.Server

	// Insecure dials the leader without TLS. Separate from the local server's
	// own SAPEDB_INSECURE, because how this daemon is reached and how it
	// reaches another one are two decisions and an operator may want them to
	// differ.
	Insecure bool

	// Retry is the wait between attempts. Zero means DefaultRetry.
	Retry time.Duration

	// Notice is where a follower says what happened to it — connected, fell
	// over, resumed from here. Nil says it nowhere, which is a follower nobody
	// can tell is stuck.
	Notice func(string)
}

// Run follows the leader until ctx is done.
//
// It returns nil on ctx being done and an error only for something no amount
// of retrying fixes: a server that was not opened read-only, or a leader that
// says this follower has fallen further behind than the log is kept. Anything
// else — a leader that is not up yet, a connection that dropped, a leader
// restarting — is waited out and tried again, because a follower that exits on
// the first network error is a follower that needs a supervisor to do the job
// it was written to do.
func Run(ctx context.Context, options Options) error {
	if options.Into == nil {
		return errors.New("sapedb/follow: no server to apply changes into")
	}
	// Checked rather than assumed. A follower applying into a server that
	// still takes writes is the one configuration in which the entry numbers
	// on the two sides can come to mean different things, and it would look
	// perfectly healthy until the first local write.
	if !options.Into.IsReadOnly() {
		return errors.New("sapedb/follow: the server holding the copy still takes writes, so following it would not keep a copy")
	}

	retry := options.Retry
	if retry == 0 {
		retry = DefaultRetry
	}

	for {
		err := once(ctx, options)
		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil:
			// The leader hung up without an error of its own. Nothing to say
			// that the reconnection below will not say.
		case tooFarBehind(err):
			// The one failure retrying cannot fix: the entries this copy needs
			// are gone from the leader's log. Reconnecting would ask for them
			// again, be refused again, and fill a log with it.
			return fmt.Errorf("sapedb/follow: this copy is further behind %s than its log is kept; rebuild it from a dump: %w",
				options.Leader.Redact(), err)
		default:
			say(options.Notice, fmt.Sprintf("following %s stopped: %v", options.Leader.Redact(), err))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// once dials the leader, catches up, and keeps up until something ends it.
func once(ctx context.Context, options Options) error {
	account, name := options.Leader.Account, options.Leader.DBName

	// Asked of the local copy every time round, not remembered from last time.
	// What this follower holds is the only thing that decides where it resumes
	// — and after a crash mid-batch it is the only thing that knows.
	at, err := options.Into.FollowingAt(account, name)
	if err != nil {
		return err
	}
	from := at + 1

	client, err := wire.Dial(options.Leader, wire.Options{
		Insecure: options.Insecure,
		// No RequestTimeout: the one request that matters here is the
		// subscription, and after it every read is NextChange, which has no
		// deadline on purpose.
	})
	if err != nil {
		return err
	}
	// Closing from here is also how the read below is interrupted: NextChange
	// blocks with no deadline, and closing the connection ends it. There is no
	// other way out of it.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-stopped:
			_ = client.Close()
		}
	}()

	following, err := client.Subscribe(from)
	if err != nil {
		return err
	}
	say(options.Notice, fmt.Sprintf("following %s from entry %d; the leader is at %d and keeps from %d",
		options.Leader.Redact(), from, following.Latest, following.Oldest))

	for {
		change, err := client.NextChange()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := options.Into.Apply(account, name, change); err != nil {
			return fmt.Errorf("applying entry %d (%s): %w", change.LSN, change.Kind, err)
		}
	}
}

// tooFarBehind says whether the leader refused because the entries wanted are
// no longer kept. The code is what is matched, not the words: a server that
// improves its wording must not turn this into an endless reconnection.
func tooFarBehind(err error) bool {
	refused := &wire.ErrRefused{}
	return errors.As(err, &refused) && refused.Code == "too_far_behind"
}

func say(notice func(string), line string) {
	if notice != nil {
		notice(line)
	}
}
