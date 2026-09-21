package cli

import (
	"sort"
	"strings"

	"github.com/sapedb/sapedb/internal/store"
)

// Completion, which this shell can do exactly rather than approximately.
//
// That is a consequence of the thing it is often criticised for. Because the
// only access paths are declared ones, the shell knows every collection, every
// index of each collection, and every field each index is over — so what it
// offers is the set of things that will work, not a guess ranked by how often
// people type them. A shell with an expression language cannot do this: at the
// point where a value could be any expression, there is nothing to complete.
//
// What is never completed is a value. The shell does not know what keys exist
// and will not go and find out, because a completion that reads the database
// is a query nobody typed, running against production, on every tab.

// suggest is what could come next, given everything typed so far.
//
// `line` is the whole line up to the cursor. The last word is the one being
// completed — an empty last word (the line ends in a space) means everything
// that could start here.
func suggest(line string, here store.Catalogue) []string {
	words := strings.Fields(line)
	partial := ""
	if !strings.HasSuffix(line, " ") && len(words) > 0 {
		partial = words[len(words)-1]
		words = words[:len(words)-1]
	}

	return narrow(candidates(words, here), partial)
}

// candidates is everything that could stand in this position.
func candidates(before []string, here store.Catalogue) []string {
	if len(before) == 0 {
		return []string{"count", "declare", "exit", "get", "hash", "help", "invoke", "ls", "scan"}
	}

	switch before[0] {
	case "get", "scan", "count", "hash":
	case "invoke":
		// The names the database holds, then the arguments the named
		// declaration holds. This is the completion this shell is best at and
		// the one it was missing: an operator who installed a module and does
		// not know what is in it can reach every operation and every argument
		// of one without reading the bundle, and nothing offered here was
		// invented — it all came out of the catalogue.
		return invokable(before, here)
	default:
		// Nothing follows ls, help or exit, and a name for declare is the
		// operator's to choose — offering one would be inventing it.
		return nil
	}

	if len(before) == 1 {
		return collections(here)
	}

	collection, found := lookup(here, before[1])
	if !found {
		return nil
	}

	// A get is a collection and a key, and a key is a value. The shell does not
	// complete values.
	if before[0] == "get" {
		return nil
	}

	// A hash is a different little grammar and not a scan with two extra
	// words: it walks key order, so there is no index to offer straight after
	// the collection, and it hands back a digest, so there is no `fields`
	// either. Offering a scan's words here would offer things the parser then
	// refuses, which this file's own doc says is worse than offering nothing.
	if before[0] == "hash" {
		return hashing(collection, before[2:])
	}

	rest := before[2:]

	// The index goes straight after the collection, and only there.
	if len(rest) == 0 {
		return append(indexes(collection), "after", "before", "fields", "from", "limit", "to")
	}
	if !keyword(rest[0]) {
		rest = rest[1:]
	}

	// After `fields`, the fields this collection is known to have: its key, and
	// everything its indexes are over. A document may hold more — nothing here
	// declares the shape of one — so these are offered, not enforced.
	if last := lastKeyword(rest); last == "fields" {
		return append(fields(collection), "after", "before", "from", "limit", "to")
	} else if last == "from" || last == "after" || last == "to" || last == "before" || last == "limit" {
		// A bound and a limit take values, which are not completed. What can be
		// offered is the keyword that ends them.
		return []string{"after", "before", "fields", "from", "limit", "to"}
	}

	return []string{"after", "before", "fields", "from", "limit", "to"}
}

// hashing is what could come next in a `hash` line, after the collection.
//
// `field` and `decode` each take exactly one word, so unlike `fields` they
// are finished as soon as one has been typed — which is why this counts what
// follows the keyword instead of asking only which keyword came last.
func hashing(collection store.Spec, rest []string) []string {
	words := []string{"after", "before", "decode", "field", "from", "limit", "to"}

	switch {
	case waiting(rest, "field"):
		// The fields this collection is known to have: its key, and
		// everything its indexes are over. A document may hold more —
		// nothing here declares the shape of one — so these are offered,
		// not enforced.
		return append(fields(collection), words...)
	case waiting(rest, "decode"):
		// The only encoding there is. A second one is a second thing to
		// keep forever, and nobody has asked for one.
		return append([]string{store.DecodeBase64}, words...)
	}
	return words
}

// waiting says whether the line ends with this keyword still expecting its
// one value.
func waiting(words []string, word string) bool {
	return len(words) > 0 && words[len(words)-1] == word
}

// invokable is what could come next in an `invoke` line: the declared names
// in the operation's position, and the declared argument names after it.
//
// An argument already written is not offered again — the shell refuses a
// repeated argument, so offering one would be offering something that will not
// work, which this file's whole doc says is worse than offering nothing. A
// value is never offered, for the same reason values are never offered
// anywhere else here.
func invokable(before []string, here store.Catalogue) []string {
	if len(before) == 1 {
		names := make([]string, 0, len(here.Operations))
		for _, operation := range here.Operations {
			names = append(names, operation.Name)
		}
		return names
	}

	operation, found := declared(here, before[1])
	if !found {
		return nil
	}

	written := map[string]bool{}
	for _, word := range before[2:] {
		if at := strings.Index(word, "="); at > 0 {
			written[word[:at]] = true
		}
	}

	names := make([]string, 0, len(operation.Input))
	for _, parameter := range operation.Input {
		if !written[parameter.Name] {
			names = append(names, parameter.Name+"=")
		}
	}
	return names
}

// lastKeyword is the keyword whose values are still being typed, or "".
func lastKeyword(words []string) string {
	for i := len(words) - 1; i >= 0; i-- {
		if keyword(words[i]) {
			return words[i]
		}
	}
	return ""
}

func lookup(here store.Catalogue, name string) (store.Spec, bool) {
	for _, spec := range here.Collections {
		if spec.Name == name {
			return spec, true
		}
	}
	return store.Spec{}, false
}

func collections(here store.Catalogue) []string {
	names := make([]string, 0, len(here.Collections))
	for _, spec := range here.Collections {
		names = append(names, spec.Name)
	}
	return names
}

func indexes(spec store.Spec) []string {
	names := make([]string, 0, len(spec.Indexes))
	for _, index := range spec.Indexes {
		names = append(names, index.Name)
	}
	return names
}

func fields(spec store.Spec) []string {
	seen := map[string]bool{spec.Key.Path: true}
	names := []string{spec.Key.Path}
	for _, index := range spec.Indexes {
		for _, field := range index.Fields {
			if !seen[field.Path] {
				seen[field.Path] = true
				names = append(names, field.Path)
			}
		}
		for _, path := range index.Include {
			if !seen[path] {
				seen[path] = true
				names = append(names, path)
			}
		}
	}
	return names
}

// narrow keeps what the half-typed word could become, in an order that does
// not change between one tab and the next.
func narrow(all []string, partial string) []string {
	kept := make([]string, 0, len(all))
	for _, one := range all {
		if strings.HasPrefix(one, partial) {
			kept = append(kept, one)
		}
	}
	sort.Strings(kept)
	return kept
}

// shared is the longest beginning every candidate agrees on, which is how far
// one press of tab can move without choosing for the operator.
func shared(all []string) string {
	if len(all) == 0 {
		return ""
	}
	common := all[0]
	for _, one := range all[1:] {
		for !strings.HasPrefix(one, common) {
			common = common[:len(common)-1]
			if common == "" {
				return ""
			}
		}
	}
	return common
}
