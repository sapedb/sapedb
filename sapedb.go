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
// silence: there is no pipelining, no connection pool and no retry. One
// request is outstanding at a time. Those are not oversights kept for later —
// they are things nobody has needed yet, and adding one is a decision, not a
// fix.
//
// There is a subscribe, and there was not until task SAPE-26. The server has
// carried Subscribe and Event frames, with a handler and tests, since before
// this package existed; no client in this repository could read them, so
// nothing had ever turned an Event back into a change and applied it. A
// connection that subscribes carries the subscription and serves no other
// request — see Client.Subscribe.
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
	Access      = store.Access
	Attribution = store.Attribution
	Bound       = store.Bound
	Catalogue   = store.Catalogue
	Change      = store.Change
	Condition   = store.Condition
	Endpoint    = store.Endpoint
	Field       = store.Field
	Index       = store.Index
	Key         = store.Key
	Operation   = store.Operation
	Parameter   = store.Parameter
	Partition   = store.Partition
	Result      = store.Result
	Rollup      = store.Rollup
	Spec        = store.Spec
	Step        = store.Step
	Term        = store.Term
	Connection  = connection.Connection
	Following   = wire.Following
	Refused     = wire.ErrRefused
	Welcome     = wire.Welcome
	Options     = wire.Options
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

// Establish declares a collection on this connection's database, without the
// server being stopped for it.
//
// Only an operator may: call Operate first, as for Declare. It is the other
// half of Declare and it arrived late, which is why the two do not match: an
// operation declared over a name that exists gets a new version and the old
// ones stay runnable, while a collection has no version to give — it is where
// the documents physically are. So establishing a name that already exists
// brings that collection up to date in place: indexes and rollups it names are
// added (built over the documents already stored) or kept, ones it leaves out
// are dropped with their entries, and what cannot be changed in place — the
// primary key, how the collection is divided, an index that keeps its name and
// changes its shape — is refused rather than done quietly.
//
// The Spec handed back is what actually took effect, read off the collection
// rather than echoed, carrying the ids the store assigned.
func (c *Client) Establish(spec Spec) (Spec, error) {
	return c.inner.Establish(spec)
}

// Present attaches a scope grant to this connection, sent with every Invoke
// from here on.
//
// An operation may declare scopes, and running it means presenting them. This
// is not a client asking for permissions: the grant is an HMAC made with the
// server's own secret, over the account, the database and this exact set of
// scopes, minted by whoever issues your connection string and handed to you
// with it. Edit the list and the signature stops matching it; the signature
// alone is no use with any other list, and neither can be made without the
// secret.
//
// A connection that never calls this presents nothing and holds nothing, which
// is what every caller did before grants existed. Calling it again replaces
// what was presented; calling it with an empty grant clears it.
func (c *Client) Present(scopes []string, grant string) {
	c.inner.Present(scopes, grant)
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

// Subscribe asks for this database's change log from an entry onwards. From
// then on the connection carries the subscription and serves no other
// request: call Subscribe on a connection of its own.
//
// Each change read with NextChange is the entry the server wrote, its number
// and its attribution included, which is what lets a caller holding an engine
// apply it and end up with the same log rather than one of its own.
//
// from is the first entry wanted, and entries are numbered from 1. A database
// that has never been written to is at 0, so 1 asks for everything there has
// ever been; 0 asks for an entry that cannot exist and is refused as
// too_far_behind, the same answer as for an entry that has aged out. Being
// told is the point: a follower quietly started somewhere other than where it
// asked is one that believes it has seen changes it has not.
//
// The returned Following says where the feed begins, how far the log has got,
// and the earliest entry the server still keeps.
func (c *Client) Subscribe(from uint64) (Following, error) {
	return c.inner.Subscribe(from)
}

// NextChange waits for the next change of this connection's subscription.
//
// It blocks with no deadline, whatever Options.RequestTimeout says: a
// subscription that has caught up is waiting for somebody to write, and on a
// quiet database that is not a fault. A caller that wants out closes the
// connection from another goroutine.
func (c *Client) NextChange() (Change, error) {
	return c.inner.NextChange()
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
