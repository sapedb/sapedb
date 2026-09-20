// Command sapedbd serves databases over the sapedb protocol.
//
// Everything it needs comes from the environment, so the same image runs in a
// container, under systemd, or on a laptop without a different invocation:
//
//	SAPEDB_SECRET     what connection strings are signed with (required)
//	SAPEDB_ADDR       where to listen                       (default :7433)
//	SAPEDB_DIR        where databases live                  (default /var/lib/sapedb)
//	SAPEDB_TLS_CERT   certificate, with SAPEDB_TLS_KEY
//	SAPEDB_TLS_KEY    its key
//	SAPEDB_INSECURE   1 to serve without TLS, said out loud
//	SAPEDB_ENCRYPT    1 to encrypt every database at rest
//	SAPEDB_LABEL      signing label, if not the default
//	SAPEDB_SHUTDOWN   how long to let connections finish    (default 20s)
//
//	SAPEDB_FOLLOW            a connection string to keep a copy of
//	SAPEDB_FOLLOW_INSECURE   1 to dial that leader without TLS
//
// SAPEDB_FOLLOW makes this daemon a follower: it subscribes to that database's
// change log from wherever its own copy has got to, applies what arrives, and
// refuses every write of its own — including reading the catalogue and the
// operator shell, both of which record an entry. See internal/follow.
//
// SIGHUP reloads: it re-reads the environment above and applies whatever of
// it can change without a restart — currently SAPEDB_SHUTDOWN and a rotated
// SAPEDB_TLS_CERT/SAPEDB_TLS_KEY pair. Everything else it names is refused by
// name rather than silently kept, on stdout, same as every other line this
// daemon logs. It is one signal, sent on purpose; nothing here watches a file
// or retries on its own.
//
// There is no logic here on purpose. What this command decides is decided in
// internal/service, where it can be tested without starting a process.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sapedb/sapedb/internal/service"
)

func main() {
	// SIGTERM is how a container is asked to stop, and SIGINT how a person
	// does. Both mean the same thing here.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// SIGHUP is the explicit, one-at-a-time way to ask a running daemon to
	// pick up a changed environment — never automatic, never on a timer.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	if err := service.RunWithReload(ctx, service.Env, os.Stdout, reload); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
