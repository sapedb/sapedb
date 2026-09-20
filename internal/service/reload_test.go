package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/protocol"
)

// mustFollow parses a connection string for a test that only cares that
// Follow changed to something, not to what.
func mustFollow(t *testing.T, raw string) connection.Connection {
	t.Helper()
	parsed, err := connection.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// syncBuilder is a strings.Builder an announce goroutine and a test can both
// touch without racing.
type syncBuilder struct {
	mutex sync.Mutex
	inner strings.Builder
}

func (b *syncBuilder) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.inner.Write(p)
}

func (b *syncBuilder) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.inner.String()
}

// syncEnv is a lookup func's backing map, safe for a test goroutine to
// mutate while a Service goroutine reads it through lookup — a real
// os.LookupEnv has no such race because the real environment does not change
// underneath a running process except through this same package's Reload.
type syncEnv struct {
	mutex  sync.Mutex
	values map[string]string
}

func (e *syncEnv) lookup(name string) (string, bool) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	v, ok := e.values[name]
	return v, ok
}

func (e *syncEnv) set(name, value string) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.values[name] = value
}

// A configuration with a third of five settings invalid applies none of the
// five (SAPE-7's own acceptance criterion, restated over this server's real
// settings rather than five abstract ones). This is the headline test: the
// bug this ticket exists to close is a reload that applies what it can and
// reports success, leaving a server that matches neither the old
// configuration nor the new one.
func TestAPartialReloadAppliesNoneOfItWhenOneEntryIsInvalid(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	otherCert, _ := selfSigned(t) // a certificate with no matching key on disk

	original := Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret,
		CertFile: certFile, KeyFile: keyFile, Shutdown: 5 * time.Second,
	}
	service, err := Start(original)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	leader := "sapedb://acme:a-password-of-the-right-shape@leader.internal:7500/main?sig=abc123"

	// Five settings asked to change at once: two reloadable (Shutdown valid,
	// the certificate pair invalid — cert and key from different pairs) and
	// three structural ones this build never reloads (Dir, Label, Follow).
	attempt := original
	attempt.Shutdown = 10 * time.Second    // reloadable, valid
	attempt.CertFile = otherCert           // reloadable, invalid: no key for it
	attempt.KeyFile = keyFile              // (mismatched with CertFile)
	attempt.Dir = t.TempDir()              // structural, ignored
	attempt.Label = "a-different-label"    // structural, ignored
	attempt.Follow = mustFollow(t, leader) // structural, ignored
	attempt.Following = true

	report := service.Reload(attempt)

	if !report.Failed() {
		t.Fatalf("a reload with an invalid entry reported success: %+v", report)
	}
	if len(report.Applied) != 0 {
		t.Errorf("a rejected reload applied something anyway: %v", report.Applied)
	}
	if report.Rejected.Field != FieldTLSCert {
		t.Errorf("rejected field = %s, want %s", report.Rejected.Field, FieldTLSCert)
	}
	if report.Rejected.Reason == "" || strings.EqualFold(report.Rejected.Reason, "failed") {
		t.Errorf("rejection reason is not a reason: %q", report.Rejected.Reason)
	}

	// Every one of the other four settings read back exactly as it was.
	running := service.Config()
	if running.Shutdown != original.Shutdown {
		t.Errorf("Shutdown changed despite the rejected reload: got %s, want %s", running.Shutdown, original.Shutdown)
	}
	if running.CertFile != original.CertFile || running.KeyFile != original.KeyFile {
		t.Errorf("the certificate pair changed despite the rejected reload: %s / %s", running.CertFile, running.KeyFile)
	}
	if running.Dir != original.Dir {
		t.Errorf("Dir changed despite the rejected reload: %s", running.Dir)
	}
	if running.Label != original.Label {
		t.Errorf("Label changed despite the rejected reload: %s", running.Label)
	}
	if running.Following {
		t.Errorf("Following changed despite the rejected reload")
	}

	// The three structural settings are still named as ignored, even though
	// the reload as a whole was rejected: an operator who asked to change all
	// five learns something about all five, not just the one that failed.
	wantIgnored := map[ReloadField]bool{FieldDir: true, FieldLabel: true, FieldFollow: true}
	for _, note := range report.Ignored {
		delete(wantIgnored, note.Field)
	}
	if len(wantIgnored) != 0 {
		t.Errorf("not reported as ignored: %v (got %+v)", wantIgnored, report.Ignored)
	}
}

// An unreloadable setting that changed is reported as ignored. A silent
// no-op looks identical from the outside to a setting that took effect —
// this is what tells the two apart.
func TestAnUnreloadableSettingIsReportedAsIgnoredNotDropped(t *testing.T) {
	original := Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret, Insecure: true,
		Shutdown: time.Second,
	}
	service, err := Start(original)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	attempt := original
	attempt.Dir = t.TempDir() // the only thing changed, and it cannot be reloaded

	report := service.Reload(attempt)

	if report.Failed() {
		t.Fatalf("changing only an unreloadable setting was rejected outright: %+v", report)
	}
	if len(report.Applied) != 0 {
		t.Errorf("nothing reloadable changed, but something was applied: %v", report.Applied)
	}
	if len(report.Ignored) != 1 || report.Ignored[0].Field != FieldDir {
		t.Fatalf("Dir was not reported as ignored: %+v", report.Ignored)
	}
	if report.Ignored[0].Reason == "" {
		t.Error("ignored with no reason given")
	}
	if !strings.Contains(report.String(), string(FieldDir)) {
		t.Errorf("the report text does not name the ignored field: %q", report.String())
	}

	if service.Config().Dir != original.Dir {
		t.Error("Dir changed even though it is not reloadable")
	}
}

// A reload that changes only what it may is applied in full, and an
// already-open connection survives it: a reload is not a reason to drop
// anyone.
func TestAValidReloadAppliesEverythingReloadableAtOnce(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	newCertFile, newKeyFile := selfSigned(t)

	original := Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret,
		CertFile: certFile, KeyFile: keyFile, Shutdown: time.Second,
	}
	svc, err := Start(original)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- svc.Serve(ctx, nil) }()

	pool := x509.NewCertPool()
	original1, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	pool.AppendCertsFromPEM(original1)
	conn, err := tls.Dial("tcp", svc.Address(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial before reload: %v", err)
	}
	defer conn.Close()

	attempt := original
	attempt.Shutdown = 3 * time.Second
	attempt.CertFile, attempt.KeyFile = newCertFile, newKeyFile

	report := svc.Reload(attempt)
	if report.Failed() {
		t.Fatalf("a valid reload was rejected: %+v", report)
	}
	want := map[ReloadField]bool{FieldShutdown: true, FieldTLSCert: true}
	for _, field := range report.Applied {
		delete(want, field)
	}
	if len(want) != 0 {
		t.Errorf("not reported as applied: %v (got %v)", want, report.Applied)
	}

	if got := svc.Config().Shutdown; got != 3*time.Second {
		t.Errorf("Shutdown = %s, want 3s", got)
	}

	// The connection opened before the reload is still alive: this Service
	// never touched its listener.
	reader := protocol.NewReader(conn).Accept(protocol.Version)
	send := func() {
		frame, err := protocol.Encode(protocol.Frame{Version: protocol.Version, Type: protocol.Ping, ID: 1, Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(frame); err != nil {
			t.Fatalf("the pre-reload connection was dropped: %v", err)
		}
	}
	send()
	if _, err := reader.Read(); err != nil {
		t.Fatalf("the pre-reload connection did not answer after reload: %v", err)
	}

	// And a new connection is handed the rotated certificate, not the old one.
	newPool := x509.NewCertPool()
	updated, err := os.ReadFile(newCertFile)
	if err != nil {
		t.Fatal(err)
	}
	newPool.AppendCertsFromPEM(updated)
	fresh, err := tls.Dial("tcp", svc.Address(), &tls.Config{RootCAs: newPool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial after reload with the new certificate: %v", err)
	}
	fresh.Close()

	// The old certificate is no longer what is presented.
	oldPool := x509.NewCertPool()
	oldPool.AppendCertsFromPEM(original1)
	if stale, err := tls.Dial("tcp", svc.Address(), &tls.Config{RootCAs: oldPool, ServerName: "localhost"}); err == nil {
		stale.Close()
		t.Error("the server still presents the certificate that was rotated away")
	}
}

// A reload cannot even build a Config from a broken environment is refused
// the same way: nothing is applied, and the operator is told why, by the
// same check FromEnv itself would have run.
func TestReloadFromEnvRejectsAnEnvironmentItCouldNotRead(t *testing.T) {
	original := Config{
		Address: "127.0.0.1:0", Dir: t.TempDir(), Secret: secret, Insecure: true,
		Shutdown: time.Second,
	}
	service, err := Start(original)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	report := service.ReloadFromEnv(env(map[string]string{
		"SAPEDB_SECRET": secret, "SAPEDB_INSECURE": "1", "SAPEDB_SHUTDOWN": "soonish",
	}))
	if !report.Failed() {
		t.Fatalf("an unparsable environment reloaded anyway: %+v", report)
	}
	if !strings.Contains(report.Rejected.Reason, "SAPEDB_SHUTDOWN") {
		t.Errorf("the rejection does not name the broken variable: %q", report.Rejected.Reason)
	}
	if service.Config().Shutdown != original.Shutdown {
		t.Error("Shutdown changed even though the environment did not parse")
	}
}

// RunWithReload wires a real SIGHUP-shaped channel to Reload without needing
// a process: a signal on the channel is picked up, applied, and reported.
func TestRunWithReloadAppliesOnSignal(t *testing.T) {
	dir := t.TempDir()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	reload := make(chan os.Signal, 1)
	announced := &syncBuilder{}

	vars := &syncEnv{values: map[string]string{
		"SAPEDB_SECRET": secret, "SAPEDB_ADDR": "127.0.0.1:0", "SAPEDB_DIR": dir,
		"SAPEDB_INSECURE": "1", "SAPEDB_SHUTDOWN": "1s",
	}}

	done := make(chan error, 1)
	go func() { done <- RunWithReload(ctx, vars.lookup, announced, reload) }()

	time.Sleep(200 * time.Millisecond)
	vars.set("SAPEDB_SHUTDOWN", "4s")
	reload <- syscall.SIGHUP

	deadline := time.After(5 * time.Second)
	for !strings.Contains(announced.String(), "applied "+string(FieldShutdown)) {
		select {
		case <-deadline:
			t.Fatalf("reload was never reported: %q", announced.String())
		case <-time.After(20 * time.Millisecond):
		}
	}

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunWithReload: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it did not stop when told")
	}
}
