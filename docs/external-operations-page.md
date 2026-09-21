# Replacement prose for `learn/external-operations.html` (SAPE-12, criterion 6)

This file is copy for somebody else to apply. Nothing under `projects/sapedb-site/` was
edited while writing it — that directory was being uploaded by another agent, and an edit
mid-flight would have published half a page.

The page today has three sections tagged `needs the feature` and zero `<pre>` blocks. This
file carries replacement prose for each of those three, and nothing else: the two sections
tagged `already true, already measured` (**Name collisions**) and `this one landed`
(**Composing operations**) are left exactly as they are.

**Every command below was run.** Every output block is pasted from a real run against a real
`sapedbd` built from this repository at the commit this file lands in, on macOS 15 (darwin
25.3.0, arm64). Nothing is reconstructed, elided or tidied. Where a line holds a key or a
temporary path, it is the real one from that run.

The worked module is `examples/library/module.json` in the `sapedb` repository: a lending
library, with three collections, five indexes, one rollup and nine operations. It is a
module rather than a missing verb, which is what the revised SAPE-12 asks for.

House markup, copied from `learn/declaring-operations.html`:

```html
<div class="code"><div class="code-label"><span>LABEL</span><span class="what">verified</span></div><pre><code data-hl="none">…</code></pre></div>
```

`data-hl` is `none`, `json`, `go` or `javascript` on the finished pages. Entities are escaped
(`&quot;`, `&mdash;`, `&amp;`). Below, the prose is given as HTML ready to paste and the code
blocks are given as plain text with the label to use, so whoever applies this does the
escaping once, mechanically.

---

## One correction to make while applying this

**The second section's heading is now wrong, and should change.** It reads *"Installed by an
administrator before the server starts"*. The "before the server starts" half stopped being
true when SAPE-14 shipped frame 14 (`establish`): a whole module — collections included —
now installs into a running daemon over one connection, with no restart. The part that is
still true, and still the point, is that **an administrator** installs it and no client can.

Suggested replacement heading: **"Installed by an operator, never by a client"**. The prose
below is written for that heading. If the heading has to stay as it is, the second paragraph
of that section contradicts it and should be read as the correction.

---

## Section 1 — `<h2>What an EO is, and what it is not</h2>`

Replaces the `<p class="note">` tagged *needs the feature*.

### Prose

```html
<p>An external operation is a file of declarations that somebody outside this project wrote,
signed with a key they hold, and handed to you. Installing it adds collections and operations
to your database. That is the whole of it.</p>

<p>What it is <em>not</em> is code. There is no plugin, no hook, no trigger, no validator, no
expression language and no WASM. A bundle can only say the things a declaration can say, and
the nine actions in the store are the entire vocabulary &mdash; a bundle that wants a tenth
cannot have one. This is why signing is enough where sandboxing would otherwise be needed: a
verified bundle can widen what your database <em>holds</em>, never what your server is
<em>able to do</em>. The worst a trusted-but-hostile bundle achieves is declarations that cost
more to serve than you expected, which is a capacity problem you can read in advance rather
than a code-execution one you cannot.</p>

<p>Four more things a bundle deliberately cannot express. It carries <strong>no data</strong>
&mdash; it declares shapes and leaves them empty. It carries <strong>no removal</strong>:
installing adds, and there is no uninstall, so a module you stop using leaves its vocabulary
behind. It carries <strong>no dependency</strong> on another bundle. And it says
<strong>nothing about where it may be installed</strong>: the same file is meant to install on
every server whose operator trusts its author.</p>

<p>A worked example ships in the repository at <code>examples/library/module.json</code>: a
lending library, in three collections with five indexes and one rollup, and nine operations
across five of the nine actions. It is deliberately a whole small module rather than one
missing verb, because a module is the hard case &mdash; it has to bring its own storage.</p>

<p>Operations in it are named <code>library:books.get</code>, not <code>books.get</code>. The
part before the colon is a namespace, and a named namespace belongs to whoever declares into
it first &mdash; for a signed bundle, that is the signing key. Use one. It is what stops the
next vendor's module from quietly redeclaring over yours.</p>
```

### Code block 1

Label: `examples/library/module.json` · what: `verified` · `data-hl="json"`

An excerpt, not the whole file — one collection and the two operations that read it. The
point of showing it is the `projection` on the read and the shape of a `key`.

**Keep the four header fields in the block even though they are not what it is illustrating.**
Without them the file is not a bundle, and the refusal a stranger gets names the *bundle's*
name while reading as though it were about a collection:

```
$ sapedb seal excerpt.json out.bundle.json < author.key
sapedb/bundle: that is not a bundle this version can read: name must be 1-128 bytes and hold no control characters, not ""
```

```json
{
  "format": "sapedb/bundle:v1",
  "name": "library",
  "version": "1.0.0",
  "signer": "the sapedb worked example",

  "collections": [
    {
      "name": "library_books",
      "key": { "path": "id", "type": "string" },
      "indexes": [
        {
          "name": "by_author",
          "fields": [
            { "path": "author", "type": "string", "missing": "skip" },
            { "path": "title", "type": "string", "missing": "skip" }
          ]
        }
      ]
    }
  ],
  "operations": [
    {
      "name": "library:books.get",
      "collection": "library_books",
      "action": "get",
      "input": [{ "name": "id", "type": "string", "required": true }],
      "key": { "arg": "id" },
      "projection": ["id", "title", "author", "shelf", "on_loan"]
    },
    {
      "name": "library:books.by_author",
      "collection": "library_books",
      "action": "scan",
      "index": "by_author",
      "limit": 50,
      "input": [{ "name": "author", "type": "string", "required": true }],
      "from": { "terms": [{ "arg": "author" }] },
      "to": { "terms": [{ "arg": "author" }] },
      "projection": ["id", "title", "shelf", "on_loan"]
    }
  ]
}
```

### Prose continuing, on `projection`

```html
<p>Every read in that file declares a <code>projection</code>, and that is not decoration. An
operation does not carry a shape, but it decides which shape a client can derive from it: the
<strong>argument</strong> shape always, because <code>input</code> gives every argument a name,
a type and whether it is required &mdash; and the <strong>row</strong> shape only when the
operation declares a projection. Leave the projection off and a client generating types from
your module has to fall back to <code>Record&lt;string, unknown&gt;</code>, and your operation
is callable but not bindable. A bundle author who copies an example that is silent on
<code>projection</code> publishes that gap without noticing, which is why the worked module is
not silent on it.</p>
```

### Code block 2

Label: `node &mdash; @ecosy/sapedb, types generated from the module` · what: `verified` · `data-hl="none"`

The command, and the two entries from its output that make the point — one read with a
projection, one write. Real output, trimmed to those two entries; say so if the page wants the
whole file.

```
$ node bin/sapedb-types.mjs examples/library/module.json --name Library

  /** `get` on `library_books`. */
  "library:books.get": {
    args: {
      id: string;
    };
    /**
     * The projected fields. Each is optional: the store keeps only those the
     * document had. Each is `unknown`: the schema names the fields, never
     * their types. The key is the declared path verbatim, not a nested path.
     */
    row: {
      id?: unknown;
      title?: unknown;
      author?: unknown;
      shelf?: unknown;
      on_loan?: unknown;
    };
  };

  /** `batch` on `library_loans`. */
  "library:loans.borrow": {
    args: {
      book: string;
      member: string;
      borrowed: number;
      due: number;
    };
    /** A batch sends back no rows; `key`, `changed` and `count` are its answer. */
    row: never;
  };
```

### Prose closing the section — the honest exception

```html
<p>One operation in the module does not get a typed row, and the reason is worth reading
before you write your own. <code>library:loans.per_member</code> reads a rollup, and a rollup
row is not a document: it is a synthetic <code>{count, group}</code> row the store builds from
the rollup's own declaration. A projection is not applied to it, so declaring one would not
help &mdash; the generator emits <code>Record&lt;string, unknown&gt;</code> for every
<code>totals</code>, with or without a projection. The shape <em>is</em> knowable, but from the
collection's <code>rollups</code> rather than from the operation, and no client reads it from
there today. So the rule has a third case the one-sentence version does not cover: the
argument shape always, the row shape when a read declares a projection, and a rollup row not
yet at all.</p>
```

---

## Section 2 — `<h2>Installed by an administrator before the server starts</h2>`

Replaces the `<p class="note">` tagged *needs the feature*. See the correction above: the
heading should become **"Installed by an operator, never by a client"**.

### Prose

```html
<p class="note">No client, in any language, can load an external operation. That is a security
property rather than a limitation to apologise for, and it belongs here rather than at the
bottom of the page: installing a bundle is an operator action, gated on the server's own
secret, exactly as <code>Explore</code> and declaring an operation are.</p>

<p>What has changed since this page was outlined is only <em>when</em>. A module used to need
the daemon stopped, because a collection could only be added by <code>sapedb apply</code>
against the database file. Frame 14 (<code>establish</code>) ended that. A module now installs
into a running daemon over one connection &mdash; its collections first, then its operations
&mdash; with no restart and with other connections still being served throughout.</p>

<p>An author does two things, and neither of them touches a database. This matters: somebody
signing a release is very often not an operator of any server at all.</p>
```

### Code block 3

Label: `sapedb &mdash; the author's half` · what: `verified` · `data-hl="none"`

```
$ sapedb keygen author.key
c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561

$ sapedb seal module.json library.bundle.json < author.key
sealed library.bundle.json, signed by c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561
```

### Prose

```html
<p><code>keygen</code> writes the private half to the file you name and prints only the public
half. It is never printed to the terminal, because a private key in your scrollback is a
private key in your scrollback forever and nothing ever complains about it. <code>seal</code>
reads the private half from stdin for the same reason an argument would be worse: arguments are
visible to anyone who can run <code>ps</code>.</p>

<p>The operator's half begins with a question, not an install. <code>verify</code> opens no
database, takes no lock, needs no secret and touches no network &mdash; you must be able to
ask <em>what would this do to my database</em> without committing to anything.</p>
```

### Code block 4

Label: `sapedb verify` · what: `verified` · `data-hl="none"`

```
$ export SAPEDB_TRUST="worked-example=c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561"
$ sapedb verify library.bundle.json
bundle "library" version "1.0.0"

  read      yes  sapedb/bundle:v1
  signed    yes  ed25519, key c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561
  intact    yes  the signature is over exactly these declarations
  trusted   yes  an operator of this server put that key on the list

  says it is "the sapedb worked example" (the bundle's own claim about its author)
  known as  "worked-example" (what an operator of this server called that key)

declares
  collection library_books (key id string)
    index by_author [author string missing:skip, title string missing:skip]
    index by_shelf [shelf string missing:skip]
  collection library_members (key id string)
    index by_name [name string missing:skip]
  collection library_loans (key id string, ulid)
    index by_member [member string missing:skip, borrowed number missing:last]
    index by_book [book string missing:skip]
    rollup loans_per_member [member string missing:skip] count
  operation library:books.shelve insert library_books
  operation library:members.join insert library_members
  operation library:books.get get library_books
  operation library:books.by_author scan library_books via by_author limit 50
  operation library:books.on_shelf count library_books via by_shelf limit 1000
  operation library:loans.borrow batch library_loans
  operation library:loans.give_back batch library_loans
  operation library:loans.of_member scan library_loans via by_member limit 25
  operation library:loans.per_member totals library_loans limit 100
```

### Prose on trust

```html
<p>Read the four checks in order, because each failure is a different action. <strong>read</strong>
is whether this is a bundle this version understands. <strong>signed</strong> and
<strong>intact</strong> are about the file: whether the author signed it, and whether these are
the declarations they signed. <strong>trusted</strong> is about the author: whether an operator
of this server put that key on the list.</p>

<p>"Trusted" means precisely that an operator pasted 64 hex characters into
<code>SAPEDB_TRUST</code>. There is no PKI, no certificate and no directory. The name the bundle
claims for itself is signed, so nobody can change it in transit, but it is written by whoever
holds the key and can say anything at all &mdash; which is why the report prints the bundle's own
claim and the label <em>you</em> wrote beside that key on separate lines, for a person to
compare.</p>

<p>An empty trust list refuses everything, and says so in those words. There is no spelling of
<code>SAPEDB_TRUST</code> that means "trust anything".</p>
```

### Code block 5

Label: `sapedb verify &mdash; the three refusals` · what: `verified` · `data-hl="none"`

The report is printed even when the bundle is refused, deliberately: a refusal has to be
takeable back to the person who sent you the file. Only the last line of each is shown here;
the full report precedes each one.

```
$ sapedb verify library.bundle.json          # SAPEDB_TRUST unset
  trusted   no   this server's trust list is empty; set SAPEDB_TRUST
library.bundle.json: sapedb/bundle: no signer is trusted on this server, so no bundle can be

$ sed 's/"limit": 50/"limit": 5000/' library.bundle.json > tampered.bundle.json
$ sapedb verify tampered.bundle.json
  intact    no   these are not the declarations that key signed
  trusted   -    not reached; the signature did not check out
tampered.bundle.json: sapedb/bundle: the signature does not match these declarations: it was signed by c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561

$ sapedb verify library.bundle.json          # somebody else on the trust list
  trusted   no   no operator of this server put that key on the list
library.bundle.json: sapedb/bundle: the bundle is signed by a key no operator of this server trusts: it was signed by c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561, and this server's list holds 1 key(s), none of them that one
```

### Prose on installing

```html
<p>Note what the middle one changed: a row ceiling, from 50 to 5000. That is the whole reason
the signature is over the declarations rather than over the bytes of the file. Re-indenting a
bundle keeps it valid; changing a number any declaration is made of does not.</p>

<p>Then install it, into a database with none of its collections in it. The database below did
not exist at all before this command ran.</p>
```

### Code block 6

Label: `sapedb install &mdash; into an empty database` · what: `verified` · `data-hl="none"`

```
$ export SAPEDB_SECRET="…"          # install opens a database, so it needs this
$ sapedb -dir lib -account acme -db main install library.bundle.json
installing bundle "library" version "1.0.0", signed by c97a7bd50680844a7ff86157217b318423d8a4a667e4f93042302ae6155ae561, trusted here as "worked-example"
collection library_books
collection library_members
collection library_loans
operation library:books.shelve version 1
operation library:members.join version 1
operation library:books.get version 1
operation library:books.by_author version 1
operation library:books.on_shelf version 1
operation library:loans.borrow version 1
operation library:loans.give_back version 1
operation library:loans.of_member version 1
operation library:loans.per_member version 1
```

### Prose

```html
<p>Collections first, then operations. That order is not a style: an operation naming a
collection that is not there yet is refused, and getting both halves in, in that order, on one
connection, is the entire difference between a module and a list of verbs.</p>

<p><code>sapedb install</code> works on the database file, so it needs the daemon stopped. The
same install against a <em>running</em> daemon is the same two steps over the wire &mdash;
<code>establish</code> for each collection, <code>declare</code> for each operation, on one
connection that never closes. The repository measures exactly that, and the daemon's pid is
compared before and after rather than inferred from the socket staying up.</p>
```

### Code block 7

Label: `go test &mdash; installed over the wire, then run` · what: `verified end to end against a running sapedbd` · `data-hl="none"`

```
$ go test . -run TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfItsCollections -count=1 -v
=== RUN   TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfItsCollections
    library_live_test.go:81: sapedbd: sapedb dev listening on 127.0.0.1:51778 as sapedb (no TLS), databases in /var/folders/vk/gd9yfkmn74v993prfl3yqcn80000gn/T/TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfIts2851184314/001
    library_live_test.go:150: library:books.get bk-1 -> The Left Hand of Darkness by Le Guin, shelf sf-a, on_loan false
    library_live_test.go:150: library:books.by_author "Le Guin" -> 2 rows, in index order [A Wizard of Earthsea The Left Hand of Darkness]
    library_live_test.go:150: library:books.on_shelf "sf-a" -> count 2, rows 0
    library_live_test.go:150: library:loans.borrow bk-1 -> 3 documents changed in one transaction
    library_live_test.go:150: library:loans.of_member mem-1 -> 1 loan, fields [book borrowed due id returned]
    library_live_test.go:150: library:loans.per_member -> map[count:1 group:[mem-1]]
    library_live_test.go:150: library:loans.give_back -> 2 documents changed; bk-1 on_loan false
--- PASS: TestTheWorkedModuleInstallsOverTheWireIntoADatabaseWithNoneOfItsCollections (0.92s)
PASS
ok  	github.com/sapedb/sapedb	1.572s
```

### Prose

```html
<p>Read the fourth line. <code>library:loans.borrow</code> is one call that changed three
documents in three different collections &mdash; it marks the book as out, stamps the member,
and writes the loan &mdash; as one transaction, with the condition <em>this book is not already
out</em> checked inside it. Borrowing the same book twice is refused by the declaration, not by
anything the caller remembered to check. The sixth line is the rollup, kept by that same
transaction rather than worked out afterwards, which is why it cannot drift.</p>
```

---

## Section 3 — `<h2>Reading an EO's cost envelope before trusting it</h2>`

Replaces the `<p class="note">` tagged *needs the feature* — "which collections, which indexes,
what row ceiling, which fields escape".

### Prose

```html
<p>Installing a module is a consent, and consent to an unreadable thing is not consent. So
every operation carries a <strong>cost envelope</strong>: four facts, derived from the
declaration rather than written by hand beside it, so it cannot disagree with the operation it
describes. Which collections it touches. Which indexes it uses. The most rows it can ever
return. Which fields leave the database in its result.</p>

<p>You read it from the catalogue, so it costs nothing to run and does not run the
operation.</p>
```

### Code block 8

Label: `sapedb shell &mdash; ls` · what: `verified against a running sapedbd` · `data-hl="none"`

Four of the nine, chosen because they are the ones that make the four facts legible. The full
listing has all nine.

```
$ sapedb -account acme -db main shell 127.0.0.1:7455 -insecure
connected to sapedb dev
sapedb main — type help, or exit when you are done
main> ls
  operation library:books.by_author v1 scan library_books
    limit 50, collections [library_books], indexes [by_author]
    projection [id, on_loan, shelf, title] escapes
  operation library:books.on_shelf v1 count library_books
    limit 1000, collections [library_books], indexes [by_shelf]
    nothing escapes
  operation library:loans.borrow v1 batch library_loans
    limit 3, collections [library_books, library_loans, library_members], indexes []
    nothing escapes
  operation library:loans.give_back v1 batch library_loans
    limit 2, collections [library_books, library_loans], indexes []
    nothing escapes
```

### Prose

```html
<p>Compare the last two lines of that listing, because they are the whole argument for the
envelope being derived rather than written. <code>borrow</code> lists three collections;
<code>give_back</code> lists two of the same three. Neither author wrote those lists. They are
read off the steps, and <code>library_members</code> appears in one and not the other because
borrowing stamps a member and returning does not. The list is exact in both directions: every
collection an operation reads is there, and one it does not read is not.</p>

<p>The row ceiling is the same number the engine already proves for itself, surfaced rather
than recomputed &mdash; a second implementation of "how far does this reach" would be a second
place for the two to disagree. For a batch it is the number of steps, which is why
<code>borrow</code> reads 3 and <code>give_back</code> reads 2.</p>

<p><code>projection … escapes</code> is the fourth fact, and it is the one worth checking against
a real answer rather than trusting. <code>library:books.by_author</code> promises four fields.
The documents it reads have five &mdash; they also carry <code>author</code>, which the index is
built on. Run it, and <code>author</code> is not in the answer.</p>
```

### Code block 9

Label: `go test &mdash; the envelope, checked against what the operation returns` · what: `verified &mdash; the guard, deliberately broken` · `data-hl="none"`

This is the test with the projection removed from the declaration, to show the check is real.
The declaration was restored immediately afterwards.

```
$ python3 -u mutate.py no-projection && go test . -run TestTheWorkedModule -count=1
mutated: no-projection
--- FAIL: TestTheWorkedModulesCostEnvelopesSayWhatItActuallyDoes (0.85s)
    library_live_test.go:390: library:books.by_author lets fields [id on_loan shelf title] escape, and its envelope says []
    library_live_test.go:434: a row carries fields [author id on_loan shelf title]; its envelope promised [id on_loan shelf title]
FAIL
```

### Prose closing

```html
<p>The second of those two lines is the one that matters. The first says the envelope changed;
the second says a document came out of the database carrying a field the envelope had not
promised. An envelope checked only against the declaration it was derived from would have
stayed green.</p>

<p>One caveat, stated plainly because it is the gap between what this page shows and what an
operator wants. <code>sapedb verify</code> prints what a bundle <em>declares</em> &mdash; the
collections, the indexes, the limits &mdash; but it does not print the derived envelope, because
the envelope is computed by the store and a bundle you have not installed is not in one. The
way to read a module's envelope before installing it somewhere that matters is to install it
into a scratch database first, read the catalogue, and then decide. That is cheap and reversible
&mdash; installing is additive, and a scratch database is a file you delete &mdash; but it is a
step, and a <code>verify</code> that printed the envelope directly would remove it.</p>
```

---

## Two more paragraphs the page needs, found by following it from cold

These are not replacements for a tagged section. They are things a stranger walks into that
the page as outlined does not mention anywhere, each one measured by doing it.

### Add to section 2, after the install block: there is no CLI for running one

```html
<p>One thing to know before you install a module: the shipped command line can
<em>install</em> an operation and cannot <em>call</em> one. The operator shell reads the
catalogue and does ad-hoc <code>get</code>, <code>scan</code> and <code>count</code> against
collections directly &mdash; it has no <code>invoke</code>. Running a declared operation means
a client: the Go package, or one of the three drivers. This is consistent with the rest of the
design rather than an omission, since an ad-hoc read by an operator and a declared operation
run by an application are deliberately different things, but it surprises everybody once.</p>
```

Measured:

```
$ sapedb -account a -db m shell 127.0.0.1:7466 -insecure
m> help
  ls                                 what this database holds
  get <collection> <key>             one document by its key
  scan <collection> [index] [...]    a stretch of an index
  count <collection> [index] [...]   how many are in that stretch
  declare [name]                     the operation that would do the last thing
  help                               this
  exit                               leave
m> invoke x:b.tot
  there is no "invoke" here; type help
```

### Add to section 2, after the verify block: verify does not validate declarations

```html
<p><code>verify</code> answers <em>who signed this and is it intact</em>. It does not answer
<em>will this install</em>, and the two are genuinely separate questions &mdash; the second one
belongs to the store, and the store runs later. So a bundle can be signed, published, and pass
<code>verify</code> with every check green, and still be refused by <code>install</code>. If
you are the author, install your own bundle into a scratch database before you publish it;
sealing it is not a check that it works.</p>
```

Measured — the same bundle, twice. An index field with no `missing`:

```
$ sapedb verify out2.bundle.json
  trusted   yes  an operator of this server put that key on the list
declares
  collection b (key id string)
    index by_a [a string missing:]

$ sapedb -dir d2 -account a -db m install out2.bundle.json
bundle "x": collection "b": sapedb/store: the declaration does not make sense: index "by_a" must say what happens to documents without "a" (skip, first or last)
```

---

## What is not in this file, and why

- **No gallery.** SAPE-12 is explicit that one example that demonstrably works is the
  deliverable, not a collection.
- **No scaffolding tool.** Also explicit: if the module needs a generator to be writable, the
  interface is what needs fixing.
- **Nothing about listing installed modules.** That is SAPE-11, and it has its own page.
- **The `<h2>Name collisions</h2>` and `<h2>Composing operations</h2>` sections are untouched.**
  Both are already written and neither was tagged `needs the feature`. Note only that the
  namespace paragraph in section 1 above and the existing **Name collisions** section now have
  to be read together: collisions are still silent **in the unnamed namespace**, and refused in
  a named one. If the existing section says there is no namespace, it is out of date since
  SAPE-9 and should be re-read while applying this.
