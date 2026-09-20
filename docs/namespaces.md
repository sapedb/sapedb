# Do namespaces exist? (SAPE-9)

**The question:** two vendors both want the operation name `orders.recent` in one customer's database, and
today the second one to declare it silently becomes the one every unversioned call reaches — so does 1.0.0
ship flat names, or does a name get a namespace?

**The recommendation:** ship flat names, and spend the decision instead on the one thing that cannot be
added later — record the signing identity of whoever declared each operation, as an optional field on the
declaration, before the tag.

Everything below is the argument for that, the measurements it rests on, and the strongest case against it.

---

## 1. What happens today, measured

Five characterisation tests in `internal/store/collision_test.go` hold every claim in this section. They
pass against the code as it stands; each one says in its own comment which claim here it keeps honest. Run
them with:

```
go test ./internal/store/ -run 'TestASecondDeclaration|TestWhatIsHereShowsOnly|TestNothingOnAStored|TestTheLogEntryNaming|TestAnOperationNameMayHold' -count=1 -v
```

### Two vendors, one name

The test declares `orders.recent` twice. The first declaration is a `get` — a read of one document by its
primary key. The second, from a different actor, is a `delete` with the same argument name. Output:

```
=== RUN   TestASecondDeclarationOfANameTakesOverEveryUnversionedCall
    collision_test.go:82: declare "orders.recent" by vendor-a: version 1, action "get"
    collision_test.go:83: declare "orders.recent" by vendor-b: version 2, action "delete", refused: false, warned: false
    collision_test.go:103: Invoke("orders.recent", version 0, id=a1) by the first vendor's app: version 2, rows 0, changed 1, document a1 still present: false
    collision_test.go:120: InvokeVersion("orders.recent", 1, id=a0): version 1, rows 1, changed 0
--- PASS: TestASecondDeclarationOfANameTakesOverEveryUnversionedCall (0.00s)
```

Read the third line slowly. The first vendor's application did not change. It called the name it was given,
with the argument it was given, and deleted a document. Nothing about a redeclaration has to resemble what
it declares over: not the action, not the collection, not the declared scopes.

The mechanism is four lines of `DeclareOperation` (`internal/store/ops.go`):

```go
latest, found, err := s.Operation(operation.Name, 0)
...
operation.Version = 1
if found {
	operation.Version = latest.Version + 1
}
```

There is no branch for "this name belongs to somebody else", because there is nothing to branch on.

### What `WhatIsHere` reports

```
=== RUN   TestWhatIsHereShowsOnlyTheNewestVersionOfAName
    collision_test.go:173: WhatIsHere lists: name "orders.one" version 1 action "get"
    collision_test.go:173: WhatIsHere lists: name "orders.recent" version 2 action "delete"
--- PASS: TestWhatIsHereShowsOnlyTheNewestVersionOfAName (0.00s)
```

One entry per name, and it is the newest version. `Catalogue` is `{Collections []Spec, Operations
[]Operation}` and `Operations()` keeps the last version it walks past for each name. The database holds two
declarations of `orders.recent`; the only discovery surface in the product reports one.

### What `InvokeVersion` resolves

Exactly what it promises, and that is the whole escape hatch: version 1 is still stored, still runnable, and
returns its row unchanged (fourth line of the first test output). The limit is not resolution, it is
discovery. A caller reaches an older version only by passing the number, and the catalogue never hands that
number out. `Client.Declare` returns the `Operation` carrying its version, so a caller that recorded it at
declare time is fine; a caller that installed a vendor's bundle and did not write the number down has
nowhere to read it from.

So: **a later declaration cannot make an earlier one unreachable by number, and does make it unreachable by
discovery.** For the rest of this document, "unreachable" means the second one.

### Does anything record who declared each version

The stored declaration does not:

```
=== RUN   TestNothingOnAStoredDeclarationSaysWhoDeclaredIt
    collision_test.go:237: stored declaration: {"name":"orders.recent","collection":"articles","action":"get","input":[{"name":"id","type":"string","required":true}],"key":{"arg":"id"},"projection":["id","title"],"version":1}
    collision_test.go:238: change log says version 1 was declared by "vendor-a"
--- PASS: TestNothingOnAStoredDeclarationSaysWhoDeclaredIt (0.00s)
```

`store.Operation` has seventeen JSON fields and none of them names a declarer. The change log does: every
`ChangeOperation` entry carries `By: Attribution{Operation: "declare", Actor: caller.Actor}`.

That actor is always an account, never a vendor. Every construction of `store.Caller` in the tree:

```
$ grep -rn "store.Caller{" --include="*.go" . | grep -v _test
internal/server/server.go:815:  caller := store.Caller{Actor: opened.Account, WriteID: asked.WriteID, Scopes: scopes}
internal/server/establish.go:113:       caller := store.Caller{Actor: live.opening.Account + " (operator)"}
internal/server/explore.go:109: caller := store.Caller{Actor: live.opening.Account + " (operator)"}
internal/server/declare.go:102: caller := store.Caller{Actor: live.opening.Account + " (operator)"}
internal/cli/cli.go:261:                        return apply(db, args, out, store.Caller{Actor: opts.account + " (apply)"})
```

`declare.go` writes the account of the connection that proved it may operate, and its own comment says why:
*"that is the only identity this connection has proved"*. A connection's account is fixed at the handshake
and `Server.reach` verifies every database under it, so **two vendors installing into one database are, on
the wire, the same account twice.** There is no vendor identity in this repository at all yet:

```
$ grep -rn "ed25519" --include="*.go" . | wc -l
       0
$ grep -rln "hmac" --include="*.go" . | wc -l   # positive control: the signing this repo does have
       2
```

### And the log's memory has an expiry

```
=== RUN   TestTheLogEntryNamingADeclarerIsPrunable
    collision_test.go:274: Retain(3) + 12 later entries: the log no longer names who declared version 1
--- PASS: TestTheLogEntryNamingADeclarerIsPrunable (0.00s)
```

`Retain`'s own doc says a cap "is not optional in practice: a log nobody trims is a disk that fills up while
everything looks healthy." Under a cap the entry naming the declarer is dropped like any other entry, while
the declaration it describes stays stored and stays the thing a name resolves to. This asymmetry is the most
load-bearing measurement in the document and section 4 turns on it.

### What a name may be

`usableName` (`internal/store/spec.go`) is the whole of the rule:

```go
func usableName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: it is empty", ErrName)
	case len(name) > 128:
		return fmt.Errorf("%w: %d characters is more than 128", ErrName, len(name))
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: it holds a zero byte", ErrName)
	}
	return nil
}
```

`TestAnOperationNameMayHoldAnySeparatorAName` declares `recent`, `orders.recent`, `orders/recent`,
`acme:orders.recent`, `@acme/orders.recent` and a 128-byte name, and all six are accepted; the empty name, a
129-byte name and a name with a zero byte are refused. Two consequences: any separator a convention might
pick is already legal, and **every one of those names is valid in 1.0**.

### One more thing found on the way, reported not fixed

There is no way to remove an operation. Collections have `Store.Drop`; operations have no equivalent
(`grep -rniE "dropoperation|removeoperation|deleteoperation"` returns 0 hits, against `Store.Drop` at
`internal/store/store.go:367` as the positive control). A name, once declared, is in that database forever,
and an uninstalled bundle leaves its vocabulary behind. SAPE-11's acceptance criterion 2 ("removal confirmed
immediately") has no code path to observe today.

---

## 2. The options, and what each costs

The constraint every option is measured against is `COMPATIBILITY.md` section 3, which 1.0.0 freezes:

> A 1.x release **may** add an action, add an optional field to a declaration, and make a refusal message
> clearer. A 1.x release **may not** change what an existing declaration does when it runs, tighten a rule
> so that a declaration valid in 1.0 is refused in 1.1, or change what a `limit` means.

Adding an optional field is allowed. Refusing a name that used to be accepted is not.

### A. Leave it. Flat names, newest version wins

- **Breaks in existing databases:** nothing.
- **Before the tag?** No. This is the do-nothing option; it is already shipped.
- **`InvokeVersion`:** unchanged.
- **Who gets hurt, and when:** the first vendor's callers, on their next unversioned call, the first time a
  customer installs a second bundle that happens to pick the same name. They get whatever the second vendor
  declared — measured above as a delete where a read used to be. The customer is not told; the first vendor
  is not told; the only artefact is a version number in an answer nobody was watching. The second vendor is
  not malicious in the ordinary case, which is what makes this likely rather than exotic: two vendors both
  calling something `orders.recent` is the first thing that happens, not the worst.

### B. A prefix convention, enforced nowhere

Publish "name your operations `vendor.thing`" in the docs where developers name operations.

- **Breaks in existing databases:** nothing.
- **Before the tag?** No — a convention can be written down in any release, and `usableName` already accepts
  every separator anybody would choose (measured).
- **`InvokeVersion`:** unchanged.
- **Cost:** zero to ship, and it stops nothing. Two vendors who both read the convention and both call
  themselves `orders` collide exactly as before. Worth doing, worth nobody mistaking for a decision.

### C. A namespace as part of the name, validated at declare time

`acme/orders.recent`, with `DeclareOperation` refusing a name that has no namespace, or one whose namespace
is not the declarer's.

- **Breaks in existing databases:** every operation already declared under a bare name, in any database
  anybody has written. There are no released tags yet, so "anybody" is small today — but declarations live
  inside database files, and this is the option that reaches back into them.
- **Before the tag? Yes, or never within 1.x.** This is the measurement that matters most in this section:
  `recent` and `orders.recent` are both valid in 1.0 (measured). A 1.1 that refused either is tightening a
  rule, which section 3 forbids. There is no version of this option that arrives in a minor release.
- **`InvokeVersion`:** a bare name has to keep resolving, or every existing caller breaks — so either the
  store carries an implicit default namespace forever (and collisions inside it continue exactly as today),
  or unqualified resolution is dropped, which is a 2.0.
- **Verdict:** the honest version of this option is "decide it now or accept it is a 2.0 feature". It is
  also the option with the least to show for the disruption: it renames things without giving anybody a
  reason to believe a namespace belongs to them.

### D. A namespace bound to the signing identity of the bundle that declared it

A vendor may only write into its own namespace, because the bundle carries a signature and the namespace is
derived from, or checked against, the signer.

- **What it needs that does not exist:** an identity. Measured above: zero ed25519 in the tree, every
  `Caller.Actor` is an account name, and no bundle path exists in the Go source (`grep -rni bundle
  --include="*.go"` returned 0 hits when this was written, against 14 hits for `hmac` as the control).
  SAPE-10 is what brings ed25519 keys and a `trusted_signers` list with identifying names, and SAPE-10 is
  in 1.0.0's scope and in flight.
- **Breaks in existing databases:** nothing, if it applies only to the bundle install path. No bundle has
  ever been installed anywhere, so there is no installed operation whose ownership is in question.
- **Before the tag?** Partly, and the split is the whole of section 3 below. The *rule* ("a signer may not
  declare over a name another signer owns") can be a refusal in the install path, which is new surface in
  1.0.0 and not one of the four frozen ones — a tool's refusal is neither declaration semantics, nor an
  existing frame, nor a `package sapedb` export. The **record** the rule reads — which signer owns which
  name — cannot wait, because it can only be written at declare time and the only thing that knows it today
  is a prunable log entry.
- **`InvokeVersion`:** unchanged, and this is the option's strongest property. Names stay flat, resolution
  stays exactly what SAPE-23 shipped, and the ownership check happens before a declaration is ever stored.
  Nothing a caller holds means anything different.
- **Cost:** `store.Operation` grows one optional field (allowed by section 3 in so many words), and the
  install path grows a lookup. SAPE-11 needs that same field, and says so: it exists to list operations with
  their signing identity, and it notes that `store.Operation` records no signing identity today, which is
  why it depends on foundational work. So the field is not a cost this decision invents; it is a cost
  SAPE-11 already pays and this decision would make load-bearing.

---

## 3. Recommendation

**Ship flat names. Do not add a namespace syntax in 1.0.0. Do add, before the tag, an optional field on
`store.Operation` recording the signing identity that declared it — written by the bundle install path
(SAPE-10) and left empty for an operator typing at a shell.** Document the collision rule as deliberate
where developers name operations, and publish the prefix convention (option B) as advice.

The reasoning is a split, not a compromise:

1. **Namespace syntax buys little and costs the most.** Option C reaches into declarations inside database
   files, forces a decision about what a bare name means forever, and still leaves the default namespace
   behaving exactly as today. It has to happen before the tag or not at all in 1.x, and "has to be decided
   now" is not the same as "is worth deciding now".

2. **The rule that actually protects a vendor is option D's, and it needs no syntax.** A name is owned by
   the first signer to declare it; a second signer is refused. That is enforceable with flat names, changes
   nothing about resolution, and turns the failure from *silent takeover on the next call* into *the install
   refuses, and the customer reads a sentence naming both vendors*.

3. **The rule can wait; the record cannot.** A refusal in the install path is a tool's refusal and can be
   added in 1.1 without breaking a promise. The field that refusal reads cannot be backfilled: it is written
   at declare time, by the only code that knows the signer, and the one existing record of that fact —
   the change-log entry — is prunable under a `Retain` cap that the code's own documentation calls
   non-optional. Ship 1.0.0 with bundles installing operations and no owner recorded, and a 1.1 ownership
   rule inherits a population of unowned names that anyone may claim by redeclaring. That is the only
   irreversible thing in this entire ticket.

So the deliverable of SAPE-9 is smaller than the ticket's title suggests: not "do namespaces exist" but
"does a declaration remember who made it". Answer that before the tag and every namespace design stays open
for 1.1, 1.2 or 2.0. Leave it and the door closes on all of them at once.

### The strongest case against

*Every argument here rests on a bundle path that does not exist yet.*

SAPE-10 is in flight, not landed. Its design may not produce a stable per-operation signer at all — a bundle
is signed as a unit, and what a database would record is the bundle's signer, not the operation's author;
those differ the moment anybody repackages. Recording a field whose meaning SAPE-10 has not settled is
worse than recording nothing, because section 3 freezes the field's *meaning* the moment it ships. A field
added in 1.1, after SAPE-10 has been used in anger, is a field that means what it says.

And the irreversibility claim is weaker than section 3 makes it look. Nobody has installed a bundle
anywhere, because bundles do not exist. If SAPE-10 lands in 1.0.0 and the ownership record lands in 1.1,
the population of unowned installed operations in the wild is whatever accumulates in one minor release —
which for a product at its first tag is plausibly zero. Buying insurance against that, at the price of
freezing a field ahead of the ticket that defines it, is paying a certain cost for a hypothetical one.

There is also a plainer objection to the whole framing: **SAPE-9's ticket asks for a decision, and this
document recommends deciding something else.** An owner who wants the question closed may reasonably say
that "flat names, documented, and here is a test asserting it is deliberate" is the entire answer, that the
signer field belongs to SAPE-10 or SAPE-11 where it is already scoped, and that SAPE-9 should not be the
ticket that quietly adds a field to `store.Operation` on its way past.

If that argument wins, the decision is option A plus option B, the characterisation tests in
`internal/store/collision_test.go` are the "test asserting the current behaviour is deliberate" the ticket
asks for, and this section is the record of what was traded away: the ability to add an enforceable
ownership rule in 1.x, if bundles turn out to install faster than anyone expected.

---

## 4. About the tests

`internal/store/collision_test.go` holds characterisation tests. They do not assert that today's behaviour
is right — that is what SAPE-9 decides and what this document records. They assert that today's behaviour is
what this document says it is. They pass against the code as it stands, and they go red if name resolution,
the catalogue's one-entry-per-name shape, the absence of a declarer on a stored declaration, the prunability
of the log entry that knows, or the `usableName` rules change.

If the product decides to make collisions refusable, `TestASecondDeclarationOfANameTakesOverEveryUnversionedCall`
goes red and says so in its own failure message. That is the intended way to find out which claims here the
decision overturned.
