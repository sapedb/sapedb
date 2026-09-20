// Package sapedb is the public surface of a client for this repository's
// server, in Go.
//
// Every type here is either an alias for a type internal/store or
// internal/connection already declares — so a value built here is the exact
// type the engine uses, nothing is converted at the boundary, and nothing can
// drift between two declarations of the same shape — or a thin wrapper around
// internal/wire's Client, kept as a wrapper (not an alias) so that a reader of
// this package's documentation never sees a type named after a path they
// cannot open.
//
// What this package does not do, said out loud so nobody plans around a
// silence: there is no subscribe here, no pipelining, no connection pool and
// no retry. One request is outstanding at a time. Those are not oversights
// kept for later — they are things nobody has needed yet, and adding one is a
// decision, not a fix.
package sapedb

import (
	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/store"
	"github.com/sapedb/sapedb/internal/wire"
)

// The value types a caller sends and receives. They are aliases, not copies:
// a value built here is the same type the engine uses, so nothing has to be
// converted at the boundary and nothing can drift between two declarations.
//
// Every field of every one of these carries a json tag, and that tag is the
// wire contract: dropping an exported field, renaming its tag, or changing
// its type is a breaking change from the day this package is first tagged.
// Adding a field with `omitempty` is not.
type (
	Access     = store.Access
	Bound      = store.Bound
	Catalogue  = store.Catalogue
	Condition  = store.Condition
	Endpoint   = store.Endpoint
	Field      = store.Field
	Index      = store.Index
	Key        = store.Key
	Operation  = store.Operation
	Parameter  = store.Parameter
	Partition  = store.Partition
	Result     = store.Result
	Rollup     = store.Rollup
	Spec       = store.Spec
	Step       = store.Step
	Term       = store.Term
	Connection = connection.Connection
	Refused    = wire.ErrRefused
	Welcome    = wire.Welcome
	Options    = wire.Options
)

// Parse takes a connection string apart, naming whichever field is at fault.
//
// sapedb:// is a public contract, not a detail of the tools that ship with
// the server: any caller that holds a string may parse it, without opening a
// connection first.
func Parse(text string) (Connection, error) {
	return connection.Parse(text)
}

// Dial opens a connection string and does the handshake.
func Dial(where Connection, options Options) (*Client, error) {
	inner, err := wire.Dial(where, options)
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner}, nil
}

// Client is one connection to one server.
//
// It wraps internal/wire's Client rather than aliasing it, on purpose: an
// alias could not change what its methods' signatures say, and those
// signatures would then read store.Catalogue and store.Operation — a path
// pkg.go.dev cannot link and nobody outside this repository can open. Six
// thin methods pay for signatures that read Catalogue and Operation instead,
// which are names this package owns.
type Client struct {
	inner *wire.Client
}

// Welcome is what the server said at the handshake.
func (c *Client) Welcome() Welcome {
	return c.inner.Welcome()
}

// Operate proves this connection may explore, by answering the challenge the
// server issued with the server's own secret.
func (c *Client) Operate(secret string) error {
	return c.inner.Operate(secret)
}

// Explore runs a typed access.
func (c *Client) Explore(access Access) (Explored, error) {
	explored, err := c.inner.Explore(access)
	if err != nil {
		return Explored{}, err
	}
	return Explored{Result: explored.Result, Draft: explored.Draft, Here: explored.Here}, nil
}

// WhatIsHere asks what the database holds.
func (c *Client) WhatIsHere() (Catalogue, error) {
	return c.inner.WhatIsHere()
}

// Declare stores an operation on this connection's database, without the
// server being stopped for it.
//
// Only an operator may: call Operate first, as for Explore. A name that is
// already declared gets a new version and the older ones stay readable, so the
// Operation handed back is worth keeping — its Version names this exact
// declaration after somebody declares over the top of it — see InvokeVersion.
func (c *Client) Declare(operation Operation) (Operation, error) {
	return c.inner.Declare(operation)
}

// Invoke runs a declared operation, at whichever version is newest.
func (c *Client) Invoke(name string, arguments map[string]any) (Result, error) {
	return c.inner.Invoke(name, arguments)
}

// InvokeVersion runs one particular version of a declared operation. Zero
// means the newest, which is what Invoke asks for.
//
// Declaring over a name keeps every older version — so after a redeclaration
// this is how a caller keeps running the one it was built against, until it
// has been changed to suit the new one. It is a separate method rather than a
// parameter on Invoke because Invoke's signature is already published.
func (c *Client) InvokeVersion(name string, version int, arguments map[string]any) (Result, error) {
	return c.inner.InvokeVersion(name, version, arguments)
}

// Close says goodbye and hangs up.
func (c *Client) Close() error {
	return c.inner.Close()
}

// Explored is the answer to an access somebody typed: what it found, and the
// operation it would have to be declared as to run again.
//
// Declared again here, by this package's own names, rather than aliased to
// wire.Explored: wire.Explored's own fields are typed store.Result,
// store.Operation and *store.Catalogue, which would carry the same unlinkable
// path into this package's documentation that Client's wrapper exists to
// avoid.
type Explored struct {
	Result Result     `json:"result"`
	Draft  Operation  `json:"draft"`
	Here   *Catalogue `json:"here,omitempty"`
}
