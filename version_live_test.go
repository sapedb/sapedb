package sapedb

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sapedb/sapedb/internal/build"
)

// The two versions below are written out by hand and are not derived from
// anything — not from build.Version, not from each other, not from a tag.
// That is the whole discipline of this file: a build identity is worth
// exactly as much as the number of different answers it can give, and the
// only way to find that out is to produce two builds and compare what they
// say. A test that read the version out of the same variable it stamped in
// would agree with itself for any implementation, including one that prints a
// constant.
const (
	firstStamp  = "1.4.0-first-of-two"
	secondStamp = "2.9.1-second-of-two"
)

// stamp builds one of this module's commands with a product version, and
// returns the path to the binary.
//
// The -X path comes from build.Path rather than being spelled here, so that
// this test stamps through exactly the mechanism the Makefile and Dockerfile
// stamp through. A rename of that package changes the constant and leaves
// their hard-coded copies behind; the binary then quietly falls back to the
// default, which is what assertions on firstStamp and secondStamp refuse.
func stamp(t *testing.T, command, version string) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), filepath.Base(command))
	building := exec.Command(goTool(t), "build",
		"-ldflags", "-X "+build.Path+"="+version,
		"-o", binary, command)
	if out, err := building.CombinedOutput(); err != nil {
		t.Fatalf("building %s at %s: %v\n%s", command, version, err, out)
	}
	return binary
}

// TestAStampedBuildSaysWhatItWasStampedWith is the acceptance measurement for
// the command, done the only way that settles it: build twice, at two
// versions, and compare the two answers.
//
// Not by reading the line that formats the string. A `sapedb version` that
// printed a hard-coded release would pass any reading of that code and fail
// here on the second build, which is the whole reason this test builds
// anything at all.
func TestAStampedBuildSaysWhatItWasStampedWith(t *testing.T) {
	first := say(t, stamp(t, "./cmd/sapedb", firstStamp))
	second := say(t, stamp(t, "./cmd/sapedb", secondStamp))

	if first == second {
		t.Fatalf("two builds at two versions introduce themselves identically: both say %q", first)
	}
	if first != "sapedb "+firstStamp {
		t.Errorf("the build stamped %q says %q", firstStamp, first)
	}
	if second != "sapedb "+secondStamp {
		t.Errorf("the build stamped %q says %q", secondStamp, second)
	}
}

// TestAnUnstampedBuildDoesNotClaimAVersion is the other end of the same
// measurement, and it is what keeps the test above from passing on a program
// that simply echoes whatever it is given without the stamping being real.
// A build nobody stamped must say the default and not invent a number.
func TestAnUnstampedBuildDoesNotClaimAVersion(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sapedb")
	building := exec.Command(goTool(t), "build", "-o", binary, "./cmd/sapedb")
	if out, err := building.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/sapedb: %v\n%s", err, out)
	}

	if said := say(t, binary); said != "sapedb dev" {
		t.Errorf("a build nobody stamped says %q, want \"sapedb dev\"", said)
	}
}

// say runs `<binary> version` and returns the one line it printed.
//
// No SAPEDB_SECRET, no SAPEDB_ACCOUNT, no SAPEDB_DB — an empty environment on
// purpose, because a version command that needs a database named before it
// will answer is one nobody can run at the moment they need it. env is set to
// an empty slice rather than left nil: nil inherits this test process's
// environment, which in a developer's shell may well have all three set, and
// the check would then pass for a reason that has nothing to do with the
// command.
func say(t *testing.T, binary string) string {
	t.Helper()

	asking := exec.Command(binary, "version")
	asking.Env = []string{}
	out, err := asking.CombinedOutput()
	if err != nil {
		t.Fatalf("%s version: %v\n%s", binary, err, out)
	}

	printed := strings.TrimRight(string(out), "\n")
	if strings.Contains(printed, "\n") {
		t.Fatalf("version printed more than one line: %q", out)
	}
	return printed
}

// TestAConnectedClientReadsTheDaemonsProductVersion is the acceptance
// measurement for the wire half: a real sapedbd process, stamped with a
// version written out above, and a real client of this package that connects
// to it over a socket and comes away knowing which build answered.
//
// The daemon is a separate process built by this test, not a server started
// in-process, because that is the only arrangement in which the version the
// client reads could have come from somewhere other than this test binary's
// own linker flags.
func TestAConnectedClientReadsTheDaemonsProductVersion(t *testing.T) {
	dir := t.TempDir()
	signature := seedTheCollection(t, dir)

	_, address := startStampedDaemon(t, dir, firstStamp)

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

	welcome := client.Welcome()

	// The control that makes the rest legible: this is a real welcome from a
	// real handshake, not a zero value that happens to have an empty version
	// in it for reasons of its own.
	if welcome.Account != "acme" || welcome.DBName != "main" {
		t.Fatalf("the welcome is not this connection's: %+v", welcome)
	}

	if welcome.ProductVersion != firstStamp {
		t.Fatalf("a daemon stamped %q introduced itself as %q", firstStamp, welcome.ProductVersion)
	}
	if welcome.ProductVersion == build.Version {
		t.Errorf("the client read %q, which is this test binary's own build.Version — the value did not come over the wire", welcome.ProductVersion)
	}

	// Read is half of it; the acceptance criterion says a client must be able
	// to write it down. Nothing about that is clever — it is a string on a
	// struct a caller already has — and this is the line that says it stays
	// that way, in the public surface, without a second round trip.
	recorded := "talked to sapedb " + welcome.ProductVersion
	if !strings.Contains(recorded, firstStamp) {
		t.Errorf("a caller recording what it talked to wrote %q", recorded)
	}
}

// startStampedDaemon is startDaemon with a product version linked into the
// binary it builds.
func startStampedDaemon(t *testing.T, dir, version string) (*exec.Cmd, string) {
	t.Helper()

	daemon := exec.Command(stamp(t, "./cmd/sapedbd", version))
	daemon.Env = append(os.Environ(),
		"SAPEDB_SECRET="+liveSecret,
		"SAPEDB_DIR="+dir,
		"SAPEDB_ADDR=127.0.0.1:0",
		"SAPEDB_INSECURE=1",
	)
	stdout, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})

	address := ""
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		line := lines.Text()
		t.Logf("sapedbd: %s", line)
		_, after, found := strings.Cut(line, "listening on ")
		if !found {
			continue
		}
		address, _, _ = strings.Cut(after, " ")
		break
	}
	if address == "" {
		t.Fatal("sapedbd never announced an address, so nothing below could have connected to it")
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	return daemon, address
}
