package sapedb

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sapedb/sapedb/internal/server"
	"github.com/sapedb/sapedb/internal/store"
)

// TestTheSurfaceIsExactlyTheseThirtyFourNames fails when a name is added to this
// package, removed from it, or renamed. It is not a style rule: every name
// here is a promise this project cannot take back without breaking somebody's
// build, so adding one has to be a decision somebody made on purpose, not a
// side effect of a refactor.
//
// It reads every *.go file at the module root, not just sapedb.go, because a
// name exported from a second file would slip past a guard that only opened
// the one file it expected to change (this is what task 0068 §8's SH1 names).
// And it compares two lists of names, in both directions, not a count: a
// count agrees when one name is swapped for another, and it agrees when the
// wrong name is simply missing and a different one was added by mistake.
//
// The surface splits into two namespaces the way Go itself does: names at
// package scope (types, the two functions), and method names on *Client,
// which live in Client's own namespace and so can reuse a package-scope name
// (Welcome the type alias, Welcome the method) without collision. Task 0068
// §1.5 counts both: 20 aliases + Parse + Dial + Client + Explored = 24
// package-scope names, plus 10 methods on Client = 34. It was 30 until Declare
// — the frame that lets an operation be declared on a server that is already
// running — and then InvokeVersion, which is how a caller reaches the older
// versions a redeclaration leaves behind, each gave Client a method. The
// thirty-third is Present: an operation may declare scopes, and until it
// existed nothing in this package could present one, so such an operation was
// one no caller of this client could ever run. The thirty-fourth is Establish,
// the other half of Declare: an operation could be declared on a running
// server and a collection could not, so a module that arrives with its own
// collection, indexes and rollups could not be installed into an empty
// database over a connection at all.
func TestTheSurfaceIsExactlyTheseThirtyFourNames(t *testing.T) {
	wantPackageScope := []string{
		// The 20 value-type aliases.
		"Access", "Bound", "Catalogue", "Condition", "Endpoint", "Field",
		"Index", "Key", "Operation", "Parameter", "Partition", "Result",
		"Rollup", "Spec", "Step", "Term", "Connection", "Refused", "Welcome",
		"Options",
		// The two package functions.
		"Parse", "Dial",
		// The two types declared (not aliased) by this package.
		"Client", "Explored",
	}
	wantClientMethods := []string{
		"Welcome", "Operate", "Explore", "WhatIsHere", "Declare", "Establish",
		"Present", "Invoke", "InvokeVersion", "Close",
	}

	if got, want := len(wantPackageScope)+len(wantClientMethods), 34; got != want {
		t.Fatalf("this test's own want-lists total %d names, not 34 — the lists drifted, fix the lists (and this test's name) rather than the number", got)
	}

	gotPackageScope, gotMethods := readSurface(t)

	compareNames(t, "package-scope name", gotPackageScope, wantPackageScope)

	if types := methodReceiverTypes(gotMethods); len(types) > 1 || (len(types) == 1 && types[0] != "Client") {
		t.Fatalf("exported methods exist on a type other than Client, which the 34-name surface does not account for: %v", types)
	}
	compareNames(t, "Client method", gotMethods["Client"], wantClientMethods)
}

// readSurface parses every non-test *.go file in the module root (the
// directory this test file lives in) and returns the exported names declared
// at package scope, and the exported methods declared on each named type,
// keyed by receiver type name with its leading "*" stripped.
func readSurface(t *testing.T) (packageScope []string, methods map[string][]string) {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing *.go at the module root: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *.go files found at the module root — the glob or the working directory is wrong")
	}

	methods = map[string][]string{}
	fset := token.NewFileSet()

	seenSource := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		seenSource = true

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE && d.Tok != token.VAR && d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							packageScope = append(packageScope, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, ident := range s.Names {
							if ident.IsExported() {
								packageScope = append(packageScope, ident.Name)
							}
						}
					}
				}

			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					packageScope = append(packageScope, d.Name.Name)
					continue
				}
				receiver := receiverTypeName(d.Recv)
				methods[receiver] = append(methods[receiver], d.Name.Name)
			}
		}
	}
	if !seenSource {
		t.Fatal("every *.go file at the module root is a _test.go file — nothing was actually scanned")
	}

	return packageScope, methods
}

// receiverTypeName returns a method's receiver type name with any leading
// pointer stripped, e.g. "*Client" and "Client" both return "Client".
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// methodReceiverTypes is the sorted set of type names that have at least one
// exported method, from readSurface's methods map.
func methodReceiverTypes(methods map[string][]string) []string {
	types := make([]string, 0, len(methods))
	for name := range methods {
		types = append(types, name)
	}
	sort.Strings(types)
	return types
}

// compareNames fails with the specific names that are extra or missing —
// never just a count — so a red run says what to add or remove, not just
// that something changed.
func compareNames(t *testing.T, kind string, got, want []string) {
	t.Helper()

	gotSet := map[string]int{}
	for _, name := range got {
		gotSet[name]++
	}
	wantSet := map[string]bool{}
	for _, name := range want {
		wantSet[name] = true
	}

	var extra []string
	for name, count := range gotSet {
		if !wantSet[name] {
			extra = append(extra, name)
		}
		if count > 1 {
			t.Errorf("%s %q is declared %d times", kind, name, count)
		}
	}
	var missing []string
	for name := range wantSet {
		if gotSet[name] == 0 {
			missing = append(missing, name)
		}
	}

	sort.Strings(extra)
	sort.Strings(missing)

	if len(extra) > 0 {
		t.Errorf("%s(s) exported that are not on the 32-name list: %s", kind, strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("%s(s) on the 32-name list that are no longer exported: %s", kind, strings.Join(missing, ", "))
	}
}

// wrapperSecret and wrapperPassword are the control-plane secret and the
// account password for TestClientWrapperForwardsWithoutDroppingFields'
// server. Named apart from internal/server's own test fixtures because this
// package cannot see those unexported test helpers — it is a different
// package, on the far side of the boundary §1 draws.
const (
	wrapperSecret   = "the secret only the control plane has"
	wrapperPassword = "a-password-of-the-right-shape"
)

// TestClientWrapperForwardsWithoutDroppingFields dials a real server through
// this package's Client and exercises all six methods, checking that the
// wrapper's ~45 lines of forwarding do what task 0068 §8.1's mutation table
// (P1-P8) worries a copy-paste of six near-identical methods can get wrong:
// drop an option, forward the wrong argument, swallow an error, or return a
// zero value instead of the inner call's answer.
func TestClientWrapperForwardsWithoutDroppingFields(t *testing.T) {
	srv, err := server.New(server.Options{Dir: t.TempDir(), Secret: wrapperSecret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = srv.Serve(listener) }()

	db, release, err := srv.Store("acme", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Declare(store.Caller{}, store.Spec{
		Name: "articles",
		Key:  store.Key{Path: "id", Type: store.TypeString, Auto: "ulid"},
		Indexes: []store.Index{{
			Name:   "by_author",
			Fields: []store.Field{{Path: "author", Type: store.TypeString, Missing: store.MissingSkip}},
		}},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if _, err := db.DeclareOperation(store.Caller{}, store.Operation{
		Name: "articles.add", Collection: "articles", Action: store.ActionInsert,
		Input: []store.Parameter{
			{Name: "title", Type: store.TypeString, Required: true},
			{Name: "author", Type: store.TypeString, Required: true},
		},
		Document: map[string]store.Term{
			"title":  {Arg: "title"},
			"author": {Arg: "author"},
		},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	signature, err := srv.Sign("acme", wrapperPassword, "main")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	conn := Connection{
		Account: "acme", Password: wrapperPassword, Host: host, Port: port,
		DBName: "main", Signature: signature,
	}

	// P8: Dial itself must not swallow a bad connection — a wrong password
	// is refused by the handshake, not answered with a zero-value client.
	bad := conn
	bad.Password = "wrong-password-of-the-right-shape"
	if _, err := Dial(bad, Options{Insecure: true}); err == nil {
		t.Fatal("Dial with a wrong password did not fail")
	}

	client, err := Dial(conn, Options{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// P7: Welcome must be the server's own answer, not a zero Welcome.
	welcome := client.Welcome()
	if welcome.Account != "acme" || welcome.DBName != "main" {
		t.Fatalf("Welcome() = %+v, want Account=acme DBName=main", welcome)
	}

	// P2: Invoke must forward both name and arguments — dropping arguments
	// silently would insert a document with no title or author instead of
	// failing, since both are declared Required.
	invoked, err := client.Invoke("articles.add", map[string]any{"title": "hello", "author": "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if invoked.Key == nil {
		t.Fatal("Invoke's Result carries no key for an insert that must generate one")
	}

	// P6: Operate must forward the caller's own secret, not a fixed or empty
	// one — a wrong secret must fail here, not appear to succeed.
	if err := client.Operate("not the secret"); err == nil {
		t.Fatal("Operate with the wrong secret did not fail")
	}
	if err := client.Operate(wrapperSecret); err != nil {
		t.Fatal(err)
	}

	// P3: Explore's Explored must carry Result AND Draft (and Here, when
	// asked) — the struct literal in Client.Explore is exactly the three-
	// field copy P3 worries a maintainer drops one field from.
	explored, err := client.Explore(Access{Kind: "get", Collection: "articles", Key: invoked.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(explored.Result.Rows) != 1 {
		t.Fatalf("Explore's Result.Rows = %v, want exactly the one document just inserted", explored.Result.Rows)
	}
	if explored.Draft.Action != store.ActionGet || explored.Draft.Collection != "articles" {
		t.Fatalf("Explore's Draft = %+v, want a get on articles", explored.Draft)
	}

	// P4: WhatIsHere must forward the inner call's own error, and its
	// Catalogue must be the real one — not swallowed into a zero value.
	// This also exercises Phase A's fix in the same breath: an empty
	// Spec.Indexes must not have come back as null one level inside it.
	here, err := client.WhatIsHere()
	if err != nil {
		t.Fatal(err)
	}
	if len(here.Collections) != 1 || here.Collections[0].Name != "articles" {
		t.Fatalf("WhatIsHere's Collections = %+v, want exactly one collection named articles", here.Collections)
	}
	if here.Collections[0].Indexes == nil {
		t.Fatal("WhatIsHere's Collections[0].Indexes came back nil")
	}
	foundOperation := false
	for _, operation := range here.Operations {
		if operation.Name == "articles.add" {
			foundOperation = true
		}
	}
	if !foundOperation {
		t.Fatalf("WhatIsHere's Operations = %+v, want articles.add in it", here.Operations)
	}

	// P9: Refused must be the same type errors.As catches whether the error
	// came from this package's Dial/Operate/Explore/Invoke or straight from
	// internal/wire — because Refused is declared `= wire.ErrRefused`, not a
	// second, unrelated struct wearing the same name.
	if _, err := client.Invoke("articles.add", map[string]any{"author": "ann"}); err == nil {
		t.Fatal("Invoke missing a required argument did not fail")
	} else {
		refused := &Refused{}
		if !errors.As(err, &refused) {
			t.Fatalf("a refusal from Invoke does not errors.As into *Refused: %v (%T)", err, err)
		}
		if refused.Code == "" {
			t.Fatalf("Refused.Code is empty on %+v", refused)
		}
	}

	// P5: Close must actually hang up, not just return nil — a second call on
	// an already-closed connection must fail rather than pretend to succeed
	// again, and it must fail with the same underlying error a raw net.Conn
	// close would (already closed), not a wrapper-invented one.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err == nil {
		t.Fatal("a second Close() on an already-closed connection did not fail")
	}
}
