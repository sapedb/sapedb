// Package wire is a client, in Go, for the server in this repository.
//
// The driver applications use is the TypeScript one; this is what the tools
// that ship with the server need — the shell today, and whatever follows it.
// It is deliberately small: a connection, the frames, and the handful of
// requests the tools send. Everything it does not do is a thing nobody has
// needed yet.
//
// One request at a time, on purpose. A tool that a person is typing at has one
// question outstanding, and the pipelining that a driver needs is machinery
// that would have to be tested for a use that does not exist.
package wire

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
)

// ErrRefused is the server saying no. It carries the code, because a tool that
// tells failures apart by matching prose breaks when the server's wording
// improves.
type ErrRefused struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ErrRefused) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (" + e.Code + ")"
}

// Client is one connection to one server.
type Client struct {
	conn    net.Conn
	reader  *protocol.Reader
	next    uint32
	welcome Welcome

	// requestTimeout bounds every request this connection sends after the
	// TCP/TLS dial, including the handshake itself. See Options.RequestTimeout.
	requestTimeout time.Duration
}

// Welcome is what the server said when the connection opened.
type Welcome struct {
	Version   uint8  `json:"version"`
	Account   string `json:"account"`
	DBName    string `json:"dbname,omitempty"`
	Mode      string `json:"mode"`
	LSN       uint64 `json:"lsn,omitempty"`
	Encrypted bool   `json:"encrypted"`
	Challenge string `json:"challenge,omitempty"`
}

// Options are what a connection needs beyond the string.
type Options struct {
	// Insecure connects without TLS. Said out loud, as on the server: a
	// connection that quietly falls back to plaintext is worse than one that
	// refuses.
	Insecure bool

	// Timeout bounds the TCP/TLS dial only. Zero means 10 seconds — Dial
	// picks a default rather than letting a stalled connect hang forever,
	// because the alternative there (an address nothing answers) is a mistake
	// worth failing fast on.
	Timeout time.Duration

	// RequestTimeout bounds each request this connection sends AFTER the
	// dial — the handshake included, since that is itself a request/response
	// pair over the socket Dial just opened. Timeout has no say over it: once
	// TCP/TLS finishes connecting, nothing before this field ever called
	// SetReadDeadline or used a context, so a peer that accepts a connection
	// and then never answers — accidentally (a stuck server) or on purpose —
	// hung every caller forever, and because a session on the server side
	// holds the database's lock for the duration of a call (see
	// internal/server's per-database mutex), one such client could pin a
	// whole database.
	//
	// Zero leaves that hang exactly as it was: no deadline is set, matching
	// what every Options{} zero value has done until now. That is a
	// deliberate choice, not an oversight matching Timeout's own default-to-
	// 10s habit — a caller doing a legitimate long-running request through
	// this client (sapedb dump/restore's bulk read, a large batch, an
	// operator shell command with no natural bound) must not start failing
	// the moment this field exists, just because it was never given a value.
	// The trap this closes is opt-in: a caller that wants liveness against a
	// stuck or hostile peer sets RequestTimeout; one that does not is exactly
	// as exposed as before, no better and no worse. See CHANGELOG.md.
	//
	// Applied with net.Conn.SetDeadline before every request (wire.go's
	// request(), which is Send+Read of one frame), not only SetReadDeadline:
	// a peer that stops draining what this client writes — a large batch
	// payload against a socket nobody is reading from the other end — can
	// wedge a Write the same way a silent peer wedges a Read, and both ends
	// of one request are one round trip a caller waits on as a single unit.
	// A context.Context was the other option considered: it would let a
	// caller cancel a request already in flight from outside, which
	// SetDeadline cannot. Nothing in this package's callers needs that today
	// — the CLI and shell issue one blocking request at a time and have
	// nothing else running that would cancel it — so the deadline is the
	// smaller change for the problem actually measured (task 0070 §2). If a
	// caller ever needs mid-flight cancellation, that is the point to
	// revisit this, not a reason to add unused surface now.
	RequestTimeout time.Duration
}

// Dial opens a connection string and does the handshake.
func Dial(where connection.Connection, options Options) (*Client, error) {
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	address := net.JoinHostPort(where.Host, fmt.Sprint(where.Port))

	var conn net.Conn
	var err error
	if options.Insecure {
		conn, err = net.DialTimeout("tcp", address, timeout)
	} else {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", address, &tls.Config{ServerName: where.Host})
	}
	if err != nil {
		return nil, err
	}

	client := &Client{
		conn: conn, reader: protocol.NewReader(conn).Accept(protocol.Version),
		requestTimeout: options.RequestTimeout,
	}

	frame, err := client.request(protocol.Hello, map[string]any{
		"account":  where.Account,
		"password": where.Password,
		"dbname":   where.DBName,
		"sig":      where.Signature,
		"mode":     "bound",
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if frame.Type != protocol.Welcome {
		_ = conn.Close()
		return nil, fmt.Errorf("sapedb/wire: the server opened with a %s", frame.Type)
	}
	if err := json.Unmarshal(frame.Payload, &client.welcome); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// Welcome is what the server said at the handshake.
func (c *Client) Welcome() Welcome { return c.welcome }

// Close says goodbye and hangs up. The goodbye is best effort: a connection
// being closed is not a place to fail.
func (c *Client) Close() error {
	_, _ = c.send(protocol.Goodbye, nil)
	return c.conn.Close()
}

// Operate proves this connection may explore, by answering the challenge the
// server issued with the server's own secret.
func (c *Client) Operate(secret string) error {
	if c.welcome.Challenge == "" {
		return errors.New("sapedb/wire: this server issued no challenge, so it cannot be operated")
	}
	challenge, err := hex.DecodeString(c.welcome.Challenge)
	if err != nil {
		return fmt.Errorf("sapedb/wire: the challenge is not hex: %w", err)
	}
	proof, err := signing.Operating(secret, challenge)
	if err != nil {
		return err
	}
	_, err = c.ask(protocol.Elevate, map[string]any{"proof": proof})
	return err
}

// Explored is the answer to an access somebody typed.
type Explored struct {
	Result store.Result     `json:"result"`
	Draft  store.Operation  `json:"draft"`
	Here   *store.Catalogue `json:"here,omitempty"`
}

// Explore runs a typed access.
func (c *Client) Explore(access store.Access) (Explored, error) {
	payload, err := c.ask(protocol.Explore, map[string]any{"access": access})
	if err != nil {
		return Explored{}, err
	}
	answer := Explored{}
	return answer, json.Unmarshal(payload, &answer)
}

// WhatIsHere asks what the database holds.
func (c *Client) WhatIsHere() (store.Catalogue, error) {
	payload, err := c.ask(protocol.Explore, map[string]any{"catalogue": true})
	if err != nil {
		return store.Catalogue{}, err
	}
	answer := Explored{}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return store.Catalogue{}, err
	}
	if answer.Here == nil {
		return store.Catalogue{}, errors.New("sapedb/wire: the server sent no catalogue")
	}
	return *answer.Here, nil
}

// Declare stores an operation on the database this connection is for, and
// hands it back with the version it was given.
//
// Only an operator may: call Operate first, exactly as for Explore. Declaring
// a name that is already declared writes a NEW version and leaves the older
// ones readable — it does not replace anything — which is why the returned
// Operation is worth reading rather than discarding: its Version is what
// InvokeVersion needs to keep running this exact declaration once somebody
// has declared over the top of it.
//
// The body key is "operation", which is what the server's own `declaring`
// struct decodes (internal/server/declare.go).
func (c *Client) Declare(operation store.Operation) (store.Operation, error) {
	payload, err := c.ask(protocol.Declare, map[string]any{"operation": operation})
	if err != nil {
		return store.Operation{}, err
	}
	answer := struct {
		Operation store.Operation `json:"operation"`
	}{}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return store.Operation{}, err
	}
	return answer.Operation, nil
}

// Invoke runs a declared operation.
//
// The body key is "args", not "arguments": that is what the server's own
// `call` struct decodes (internal/server/server.go) and what the TypeScript
// client sends. Before task 0068 §1, nothing in this repository or its
// TypeScript sibling ever called this method against a real server — cli's
// shell only ever calls Explore/WhatIsHere — so a body sent under the wrong
// key had nothing to catch it: the server silently saw no arguments at all
// on every call. Caught by sapedb_test.go's end-to-end wrapper test, the
// first thing to invoke a declared operation through this client.
func (c *Client) Invoke(name string, arguments map[string]any) (store.Result, error) {
	return c.InvokeVersion(name, 0, arguments)
}

// InvokeVersion runs one particular version of a declared operation. Version
// zero is the latest, which is what Invoke asks for.
//
// The wire has carried a version since before this method existed — the
// server's `call` struct decodes it and store.Store.Invoke takes it — and
// nothing in this client could set it. So a redeclaration (Declare, or
// `sapedb apply`) left every older version stored, readable, runnable by the
// engine, and unreachable through this package: the data was there, the
// declaration was there, and there was no way to ask for it.
//
// It is a second method rather than a third parameter on Invoke, because
// Invoke's signature is part of a surface this repository has already
// published and taking it back would break every caller for a field almost
// none of them pass. The two alternatives were both worse: a variadic
// `version ...int` compiles for `Invoke(name, args, 1, 2, 3)` and reads in
// the documentation as something it is not, and an options struct means
// either a second method anyway or the same breaking change with more
// ceremony. One name on the surface is the honest price.
func (c *Client) InvokeVersion(name string, version int, arguments map[string]any) (store.Result, error) {
	body := map[string]any{"command": name, "args": arguments}
	// Omitted rather than sent as zero, because the field is `omitempty` on
	// the server's own struct and on the TypeScript client's request body:
	// sending an explicit 0 where every other client sends nothing is a
	// difference with no meaning that somebody would one day have to explain.
	if version > 0 {
		body["version"] = version
	}
	payload, err := c.ask(protocol.Invoke, body)
	if err != nil {
		return store.Result{}, err
	}
	result := store.Result{}
	if err := json.Unmarshal(payload, &result); err != nil {
		return store.Result{}, err
	}
	return result, nil
}

// ask sends a request and returns the payload of a successful answer, turning
// a refusal into an error that carries the code.
func (c *Client) ask(kind protocol.Type, body any) ([]byte, error) {
	frame, err := c.request(kind, body)
	if err != nil {
		return nil, err
	}
	if frame.Type == protocol.Failure {
		refused := &ErrRefused{}
		if err := json.Unmarshal(frame.Payload, refused); err != nil {
			return nil, fmt.Errorf("sapedb/wire: the server refused it and the reason does not read: %w", err)
		}
		return nil, refused
	}
	return frame.Payload, nil
}

// request sends one frame and reads the one that answers it.
//
// The deadline covers both: a peer that stops reading can wedge the write
// half of this round trip exactly as a peer that stops answering wedges the
// read half, and a caller waiting on request() is waiting on the pair of
// them as one unit. Set fresh on every call (not once, in Dial) so that one
// slow request does not leave a deadline in the past for every request after
// it — and cleared (a zero time.Time) when RequestTimeout is 0, in case this
// connection ever reuses a net.Conn that already has one set.
func (c *Client) request(kind protocol.Type, body any) (protocol.Frame, error) {
	if c.requestTimeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.requestTimeout)); err != nil {
			return protocol.Frame{}, err
		}
	} else if err := c.conn.SetDeadline(time.Time{}); err != nil {
		return protocol.Frame{}, err
	}

	id, err := c.send(kind, body)
	if err != nil {
		return protocol.Frame{}, err
	}
	frame, err := c.reader.Read()
	if err != nil {
		return protocol.Frame{}, err
	}
	// One request at a time, so an answer to something else is a server that
	// is not the one this speaks to.
	if frame.ID != id {
		return protocol.Frame{}, fmt.Errorf("sapedb/wire: asked %d and was answered %d", id, frame.ID)
	}
	return frame, nil
}

func (c *Client) send(kind protocol.Type, body any) (uint32, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = encoded
	}

	c.next++
	frame, err := protocol.Encode(protocol.Frame{Version: protocol.Version, Type: kind, ID: c.next, Payload: payload})
	if err != nil {
		return 0, err
	}
	if _, err := c.conn.Write(frame); err != nil {
		return 0, err
	}
	return c.next, nil
}
