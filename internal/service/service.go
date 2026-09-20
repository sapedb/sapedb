// Package service turns environment into a running server.
//
// It is a separate package from the command so that the decisions worth
// arguing about — whether to serve without TLS, what happens on a shutdown
// signal, where databases live — are testable without building a binary and
// starting a process. A `main` that holds logic is a `main` nobody tests.
package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sapedb/sapedb/internal/build"
	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/follow"
	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/signing"
)

// DefaultAddress is the port a connection string assumes when it names none.
const DefaultAddress = ":7433"

// DefaultDir is where a packaged server keeps databases.
const DefaultDir = "/var/lib/sapedb"

var (
	ErrNoSecret    = errors.New("sapedb: SAPEDB_SECRET is not set, so no connection could be verified")
	ErrNoTLS       = errors.New("sapedb: refusing to serve without TLS")
	ErrHalfTLS     = errors.New("sapedb: a TLS certificate needs its key, and a key needs its certificate")
	ErrNotDuration = errors.New("sapedb: that is not a length of time")
)

// Config is everything the server is told before it starts.
type Config struct {
	Address string
	Dir     string
	Secret  string
	Label   string

	CertFile string
	KeyFile  string
	// Insecure serves without TLS. It has to be said out loud: a server that
	// quietly falls back to plaintext when a certificate is missing is a server
	// that will one day be in production without one, and nothing about it
	// looks different.
	Insecure bool

	// Encrypt stores every database encrypted under a key derived from the
	// secret.
	Encrypt bool
	// Shutdown is how long to let connections finish after a signal.
	Shutdown time.Duration

	// Follow is the connection string of a database this daemon keeps a copy
	// of. Set, this daemon is a follower: it subscribes to that database's
	// change log from wherever its own copy has got to, applies what arrives,
	// and refuses every write of its own. The account and database name come
	// out of the string — a copy of acme/main is served here as acme/main.
	//
	// It is one string rather than a list because following one database is
	// what has been measured. A daemon that followed several would need to
	// decide what it does when one of them is unreachable and the others are
	// not, and that is a decision, not a loop.
	Follow connection.Connection
	// Following says whether Follow was set, because a zero Connection is a
	// valid-looking struct and "is this a follower" must not be a guess about
	// empty strings.
	Following bool
	// FollowInsecure dials the leader without TLS. Separate from Insecure,
	// which is about how this daemon is reached: a daemon can be served over
	// TLS and follow a leader on a private network, or the other way round.
	FollowInsecure bool
}

// FromEnv reads the configuration. `lookup` is os.LookupEnv in a real process.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	get := func(name, fallback string) string {
		if value, found := lookup(name); found && value != "" {
			return value
		}
		return fallback
	}
	flag := func(name string) bool {
		value := strings.ToLower(get(name, ""))
		return value == "1" || value == "true" || value == "yes"
	}

	label := get("SAPEDB_LABEL", "")
	if strings.EqualFold(label, "direct") {
		// The plainer form, which has to be asked for by name: its sentinel is
		// deliberately not something a blank field can produce.
		label = signing.Direct
	}

	config := Config{
		Address:  get("SAPEDB_ADDR", DefaultAddress),
		Dir:      get("SAPEDB_DIR", DefaultDir),
		Secret:   get("SAPEDB_SECRET", ""),
		Label:    label,
		CertFile: get("SAPEDB_TLS_CERT", ""),
		KeyFile:  get("SAPEDB_TLS_KEY", ""),
		Insecure: flag("SAPEDB_INSECURE"),
		Encrypt:  flag("SAPEDB_ENCRYPT"),
		Shutdown: 20 * time.Second,
	}

	if wait := get("SAPEDB_SHUTDOWN", ""); wait != "" {
		parsed, err := time.ParseDuration(wait)
		if err != nil {
			return Config{}, fmt.Errorf("%w: SAPEDB_SHUTDOWN=%q", ErrNotDuration, wait)
		}
		config.Shutdown = parsed
	}

	if leader := get("SAPEDB_FOLLOW", ""); leader != "" {
		// Parsed here rather than when the follower starts, so that a string
		// with a typo in it stops the daemon at startup — where somebody is
		// watching — instead of at the first reconnection attempt, in a log.
		// connection.Parse names the field at fault, which matters for a value
		// nobody can print: it is a secret.
		parsed, err := connection.Parse(leader)
		if err != nil {
			return Config{}, fmt.Errorf("SAPEDB_FOLLOW: %w", err)
		}
		config.Follow, config.Following = parsed, true
		config.FollowInsecure = flag("SAPEDB_FOLLOW_INSECURE")
	}

	return config, config.check()
}

func (c Config) check() error {
	if c.Secret == "" {
		return ErrNoSecret
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return ErrHalfTLS
	}
	if c.CertFile == "" && !c.Insecure {
		return fmt.Errorf("%w: set SAPEDB_TLS_CERT and SAPEDB_TLS_KEY, or SAPEDB_INSECURE=1 to say you meant it", ErrNoTLS)
	}
	return nil
}

// Service is a server and the listener it is answering on.
type Service struct {
	// mutex guards config. Everything else about a Service is fixed at
	// Start: the directory, the secret, the address, whether it follows —
	// none of that can change without a new listener or a new server, so
	// only the handful of fields Reload actually touches need protecting.
	mutex  sync.Mutex
	config Config

	server   *server.Server
	listener net.Listener

	// cert is the certificate a TLS listener presents, read fresh on every
	// handshake instead of baked into the listener's tls.Config. That
	// indirection is what lets Reload swap it: two connections dialled a
	// second apart can see two different certificates without the listener
	// itself ever being torn down. Nil when this Service is not serving TLS.
	cert atomic.Pointer[tls.Certificate]
}

// Start opens the listener and the server, but serves nothing yet.
//
// Opening the listener here rather than inside Serve is what lets a caller —
// a test, or a supervisor — know the port is taken before anything is
// announced as ready.
func Start(config Config) (*Service, error) {
	if err := config.check(); err != nil {
		return nil, err
	}

	made, err := server.New(server.Options{
		Dir: config.Dir, Secret: config.Secret, Label: config.Label, Encrypt: config.Encrypt,
		// A follower refuses writes, and that is the same decision as being a
		// follower rather than a second switch beside it. Two switches would
		// mean a configuration in which this daemon applies somebody else's
		// log and takes writes of its own, which is the one arrangement whose
		// entry numbers stop meaning the same thing on the two sides.
		ReadOnly: config.Following,
	})
	if err != nil {
		return nil, err
	}

	raw, err := net.Listen("tcp", config.Address)
	if err != nil {
		made.Close()
		return nil, err
	}

	service := &Service{config: config, server: made, listener: raw}

	if config.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			raw.Close()
			made.Close()
			return nil, fmt.Errorf("sapedb: reading the certificate: %w", err)
		}
		service.cert.Store(&certificate)
		service.listener = tls.NewListener(raw, &tls.Config{
			// A function rather than a fixed Certificates slice, so a
			// certificate rotated in by Reload reaches the very next
			// handshake without the listener changing at all.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return service.cert.Load(), nil
			},
			// Nothing below this is worth serving a database over.
			MinVersion: tls.VersionTLS12,
		})
	}

	return service, nil
}

// Address is where it is listening, which is worth asking when the port was
// zero.
func (s *Service) Address() string { return s.listener.Addr().String() }

// Server is the server underneath, for a caller that needs to declare
// something before anybody connects.
func (s *Service) Server() *server.Server { return s.server }

// Serve answers connections until the context is done or the listener fails.
//
// A signal closes the listener, so no new connection is accepted, and then the
// databases are closed. Connections in flight finish what they were doing:
// every write has already been committed before its answer went back, so
// nothing is lost either way — but cutting a client off mid-answer for no
// reason is rudeness with no benefit.
func (s *Service) Serve(ctx context.Context, announce io.Writer) error {
	// Taken once, under the lock, because CertFile's presence, Dir, and
	// following-ness never change after Start — Reload refuses every one of
	// them by name — so nothing past this point needs to re-read s.config for
	// them. Shutdown is the one field Reload does touch, so it is re-read
	// fresh at the point it is used, below.
	start := s.snapshot()

	// Databases are opened on demand, so what a database has to say about how
	// it was last left is said while the server is running, not before.
	if announce != nil {
		s.server.Say(func(line string) { fmt.Fprintf(announce, "sapedb: %s\n", line) })
	}

	if announce != nil {
		scheme := "sapedb+tls"
		if start.CertFile == "" {
			scheme = "sapedb (no TLS)"
		}
		// The build comes first, before anything about this particular run.
		// A daemon has no `version` subcommand to ask — its only argument
		// surface is the environment, and it is usually a container nobody
		// has a shell into — so the first line of its log is where the
		// question "which build is this" has to be answerable from. It is
		// also the one line that is kept when a log is pasted into a report.
		fmt.Fprintf(announce, "sapedb %s listening on %s as %s, databases in %s\n",
			build.Version, s.Address(), scheme, start.Dir)
		if start.Following {
			// Said on the line after the address, because "which of these is
			// the leader" is the first question anybody debugging two daemons
			// asks, and the answer must not be something they have to infer
			// from a write being refused. Redacted: the string holds a
			// password and a signature.
			fmt.Fprintf(announce, "sapedb following %s, and taking no writes of its own\n",
				start.Follow.Redact())
		}
	}

	// Started before Serve, so that a follower catches up whether or not
	// anybody connects to it. Stopped by the same context that stops serving.
	if start.Following {
		following, stopFollowing := context.WithCancel(ctx)
		defer stopFollowing()
		go func() {
			err := follow.Run(following, follow.Options{
				Leader: start.Follow, Into: s.server, Insecure: start.FollowInsecure,
				Notice: func(line string) {
					if announce != nil {
						fmt.Fprintf(announce, "sapedb: %s\n", line)
					}
				},
			})
			// Only what retrying cannot fix reaches here. It is said and the
			// daemon keeps serving what it already has: a follower that has
			// fallen off the end of the leader's log still holds a database
			// somebody may be reading, and killing the process would take that
			// away as well.
			if err != nil && announce != nil {
				fmt.Fprintf(announce, "sapedb: %v\n", err)
			}
		}()
	}

	done := make(chan error, 1)
	go func() { done <- s.server.Serve(s.listener) }()

	select {
	case err := <-done:
		s.server.Close()
		// A listener closed on purpose is not a failure to report.
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err

	case <-ctx.Done():
		// Read fresh rather than taken from start: a reload that changed how
		// long a shutdown waits should govern the shutdown that actually
		// happens, even one that was reloaded in after Serve began.
		wait := s.snapshot().Shutdown
		if announce != nil {
			fmt.Fprintf(announce, "sapedb stopping, %s for connections to finish\n", wait)
		}
		s.listener.Close()

		select {
		case <-done:
		case <-time.After(wait):
			if announce != nil {
				fmt.Fprintln(announce, "sapedb: connections did not finish in time; closing anyway")
			}
		}
		return s.server.Close()
	}
}

// Close stops serving without waiting for anything.
func (s *Service) Close() error {
	s.listener.Close()
	return s.server.Close()
}

// snapshot copies the config under the lock, so a caller can read several
// fields together without one of them changing halfway through.
func (s *Service) snapshot() Config {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.config
}

// Config is the configuration this Service is currently running with,
// including anything a prior Reload applied.
func (s *Service) Config() Config { return s.snapshot() }

// ReloadField names one setting a reload considered, by the environment
// variable(s) that set it — the same name an operator would grep a log for.
type ReloadField string

const (
	FieldShutdown ReloadField = "SAPEDB_SHUTDOWN"
	FieldTLSCert  ReloadField = "SAPEDB_TLS_CERT / SAPEDB_TLS_KEY"
	FieldAddress  ReloadField = "SAPEDB_ADDR"
	FieldDir      ReloadField = "SAPEDB_DIR"
	FieldSecret   ReloadField = "SAPEDB_SECRET"
	FieldLabel    ReloadField = "SAPEDB_LABEL"
	FieldInsecure ReloadField = "SAPEDB_INSECURE"
	FieldEncrypt  ReloadField = "SAPEDB_ENCRYPT"
	FieldFollow   ReloadField = "SAPEDB_FOLLOW / SAPEDB_FOLLOW_INSECURE"
	// FieldEnvironment stands in for a reload whose environment could not
	// even be read into a Config — the same checks FromEnv runs at startup,
	// run again, naming the same field FromEnv would have named.
	FieldEnvironment ReloadField = "environment"
)

// Why an unreloadable field is refused, said once here rather than at each
// call site, so the reason a setting is on this list can't drift from the
// reason given for it.
const (
	reasonRestart  = "changing this while serving needs a new listener; restart to change it"
	reasonDataDir  = "changing the data directory while databases are open would split state across two directories; restart to change it"
	reasonSecret   = "every open connection and every grant is checked against the running secret on every request, and it also derives the key databases are encrypted under; changing it live would invalidate all of them at once and could lock the server out of its own encrypted databases"
	reasonLabel    = "part of the same signing scheme as the secret; changing it alone breaks verification the same way changing the secret would"
	reasonTLSOnOff = "turning TLS on or off changes what the listener accepts, the same kind of change as the address; restart to change it"
	reasonEncrypt  = "decided once, when a database file is created or first opened; toggling it does not touch files already on disk and would leave a mix of encrypted and plain databases reported as one setting"
	reasonFollow   = "switches whether this server takes writes of its own or refuses them, which changes what its own entry numbers mean; that is a restart decision, not a reload"
)

// ReloadNote is one setting a reload did not apply, and why.
type ReloadNote struct {
	Field  ReloadField
	Reason string
}

// ReloadReport is exactly what a reload attempt changed, what it left alone,
// and why — an operator reads this instead of finding out later, from
// something still behaving the old way, that part of a reload never landed.
//
// Rejected set means Applied is empty. A reload either takes effect in full
// or leaves the running configuration exactly as it was; there is no partly
// applied state for this to describe, because none is ever produced.
type ReloadReport struct {
	// Applied are the reloadable settings that changed and now hold their new
	// value.
	Applied []ReloadField
	// Ignored are settings this build never reloads, reported only when the
	// requested configuration actually asked for a different value — nothing
	// is said about a setting nobody tried to change.
	Ignored []ReloadNote
	// Rejected is set when the whole reload was refused: one reloadable
	// setting failed validation, so none of them were applied.
	Rejected *ReloadNote
}

// Failed reports whether the reload was refused outright.
func (r ReloadReport) Failed() bool { return r.Rejected != nil }

// String is the line (or lines) an operator sees after a reload.
func (r ReloadReport) String() string {
	var out strings.Builder
	switch {
	case r.Rejected != nil:
		fmt.Fprintf(&out, "sapedb: reload refused, nothing applied — %s: %s\n", r.Rejected.Field, r.Rejected.Reason)
	case len(r.Applied) == 0:
		fmt.Fprintln(&out, "sapedb: reload: nothing to apply")
	default:
		for _, field := range r.Applied {
			fmt.Fprintf(&out, "sapedb: reload: applied %s\n", field)
		}
	}
	for _, note := range r.Ignored {
		fmt.Fprintf(&out, "sapedb: reload: ignored %s — %s\n", note.Field, note.Reason)
	}
	return strings.TrimRight(out.String(), "\n")
}

// ReloadFromEnv re-reads the environment and reloads whatever of it can be
// reloaded. A configuration the environment can no longer even produce — a
// duration that does not parse, a follower string with a typo — is reported
// the same way an invalid reloadable field is: refused, nothing applied.
func (s *Service) ReloadFromEnv(lookup func(string) (string, bool)) ReloadReport {
	next, err := FromEnv(lookup)
	if err != nil {
		return ReloadReport{Rejected: &ReloadNote{Field: FieldEnvironment, Reason: err.Error()}}
	}
	return s.Reload(next)
}

// Reload applies whatever of next can be applied to a running Service.
//
// It validates every reloadable setting before it touches any of them, and
// applies all of them or none: a configuration whose one invalid entry is
// applied last, after four valid ones already took effect, is exactly the
// half-migrated state this exists to prevent. Everything else next asks to
// change is refused by name — reported once, in Ignored — because this
// server has no safe way to change it without a restart.
func (s *Service) Reload(next Config) ReloadReport {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	current := s.config
	var report ReloadReport

	structural := [...]struct {
		field   ReloadField
		changed bool
		reason  string
	}{
		{FieldAddress, next.Address != current.Address, reasonRestart},
		{FieldDir, next.Dir != current.Dir, reasonDataDir},
		{FieldSecret, next.Secret != current.Secret, reasonSecret},
		{FieldLabel, next.Label != current.Label, reasonLabel},
		{FieldEncrypt, next.Encrypt != current.Encrypt, reasonEncrypt},
		{
			FieldFollow,
			next.Following != current.Following || next.Follow != current.Follow || next.FollowInsecure != current.FollowInsecure,
			reasonFollow,
		},
	}
	for _, field := range structural {
		if field.changed {
			report.Ignored = append(report.Ignored, ReloadNote{Field: field.field, Reason: field.reason})
		}
	}

	// Whether TLS is served at all is a listener-level decision like the
	// address — Insecure and an empty CertFile are the same fact seen from
	// two fields — so turning it on or turning it off is refused the same
	// way, even though rotating an already-configured certificate, below, is
	// not.
	togglesTLS := (current.CertFile == "") != (next.CertFile == "")
	if togglesTLS || next.Insecure != current.Insecure {
		report.Ignored = append(report.Ignored, ReloadNote{Field: FieldInsecure, Reason: reasonTLSOnOff})
	}

	// Reloadable settings are validated before anything is touched — the
	// all-or-nothing guarantee this whole method exists for. Nothing below
	// this point may mutate s.config or s.cert until every one of them has
	// been checked.
	shutdownChanged := next.Shutdown != current.Shutdown
	if shutdownChanged && next.Shutdown <= 0 {
		report.Rejected = &ReloadNote{Field: FieldShutdown, Reason: "SAPEDB_SHUTDOWN must be a positive duration"}
		return report
	}

	// A certificate rotation only, not a TLS on/off toggle (handled above):
	// both the old and the new configuration already serve TLS, and the pair
	// named is a different one.
	rotatesTLS := !togglesTLS && current.CertFile != "" &&
		(next.CertFile != current.CertFile || next.KeyFile != current.KeyFile)
	var rotated *tls.Certificate
	if rotatesTLS {
		loaded, err := tls.LoadX509KeyPair(next.CertFile, next.KeyFile)
		if err != nil {
			report.Rejected = &ReloadNote{Field: FieldTLSCert, Reason: err.Error()}
			return report
		}
		rotated = &loaded
	}

	// Every reloadable setting that changed is valid. Apply all of them.
	if shutdownChanged {
		s.config.Shutdown = next.Shutdown
		report.Applied = append(report.Applied, FieldShutdown)
	}
	if rotatesTLS {
		s.cert.Store(rotated)
		s.config.CertFile, s.config.KeyFile = next.CertFile, next.KeyFile
		report.Applied = append(report.Applied, FieldTLSCert)
	}

	return report
}

// Run is the whole of a `main`: read the environment, start, serve until a
// signal.
func Run(ctx context.Context, lookup func(string) (string, bool), announce io.Writer) error {
	return RunWithReload(ctx, lookup, announce, nil)
}

// RunWithReload is Run, plus an explicit reload trigger: each signal received
// on reload re-reads lookup and applies whatever of it can be applied, all at
// once or not at all, and writes the resulting ReloadReport to announce. A nil
// reload channel disables the path entirely — this is exactly Run.
func RunWithReload(ctx context.Context, lookup func(string) (string, bool), announce io.Writer, reload <-chan os.Signal) error {
	config, err := FromEnv(lookup)
	if err != nil {
		return err
	}

	service, err := Start(config)
	if err != nil {
		return err
	}

	if reload != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-reload:
					report := service.ReloadFromEnv(lookup)
					if announce != nil {
						fmt.Fprintln(announce, report.String())
					}
				}
			}
		}()
	}

	return service.Serve(ctx, announce)
}

// Env is os.LookupEnv, named so that Run reads as what it does.
var Env = os.LookupEnv
