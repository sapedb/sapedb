package store

// Removing a stretch of keys, with a ceiling.
//
// The machinery was already here — a walk of a key range, and a remove of one
// document — and this is those two things with a counter between them. What
// took a ticket rather than an afternoon is everything around that loop, so
// this comment is about those things rather than about the loop.
//
// # It is not a cheap truncation, and the name suggests it is
//
// "Delete a range" sounds like moving a boundary. It is not. Reading
// Collection.remove: each document has to be READ before it can go, because
// its index entries are derived from its contents; then every entry is
// deleted, then contribute(tree, document, -1) adjusts every rollup it fed,
// then the document itself goes, then the change is recorded. So one run
// costs
//
//	limit × (one document read + its index entries + its rollup contribution
//	         + one log entry)
//
// It is linear in limit and every unit is bounded, which is exactly what
// makes it declarable — and it is also why anybody who reads the name and
// assumes otherwise will be surprised by the log rather than by the latency.
//
// # N deletions write N log entries, and that is the decision
//
// This does not batch the change log, and not batching it is the answer
// rather than a thing left undone. The entries a deleteRange writes are
// exactly the entries the same deletes done one at a time write: the same
// ChangeDelete kind, one per key, carrying the same Attribution. A follower
// replaying them needs to understand nothing it did not already understand,
// and Apply's ChangeDelete arm removes them one at a time the way it always
// has. One entry carrying a list of keys would be a new log shape, and a new
// log shape is a thing every follower, every dump and every subscriber has to
// learn before the first one can be written.
//
// What this does do is make ISS-23 matter more: no operator can cap
// retention, so removing a large file's worth of rows grows the log by that
// much. That is named rather than fixed here, and it is the reason the limit
// on this action is required with no default — see validateOperation.
//
// # The ceiling is counted, not trusted
//
// The same rule hashRange wrote down, for the same measured reason (W7, in
// pipelines/tasks/0071: an external operation declaring `limit 50`, correctly
// sandboxed and correctly signed, served fifty million rows in one call —
// one million host calls of fifty each — because the host checked every
// individual call and never the total. A signature proves whose binary it is.
// A sandbox proves it does not escape. Neither proves what it costs).
//
// So the ceiling here is counted by the host, against ONE counter, per RUN of
// the operation and not per underlying call. A partitioned collection is
// where that distinction is visible without anybody writing a loop: one run
// of this is several walks of several files, and a counter that started again
// at each file would let a declared limit of five remove any multiple of five
// it liked. What is counted is rows removed.
//
// # At the ceiling it stops and says so, where a hashRange stops and refuses
//
// This is the one place this action deliberately differs from its sibling,
// and the ticket is read as requiring the difference rather than forbidding
// it, so the reasoning is written here rather than assumed.
//
// SAPE-34 refuses at the ceiling because a digest of part of a range is
// thirty-two bytes in the same field as a digest of all of it: there is no
// shorter honest answer, so there is no answer. That argument is about the
// SHAPE of the answer, not about walking, and it does not survive the move to
// this action. A stopped deleteRange has somewhere to put "there was more" —
// Truncated — and somewhere to put "here is where I got to" — Key. Its answer
// is therefore self-describing in a way a digest can never be, which is
// precisely the property the refusal exists to supply when it is absent.
//
// It also has to be this way for the ticket's own first acceptance criterion
// to be satisfiable: "a thousand rows in pages". Paging needs the last key
// removed to come BACK, and a refusal hands back nothing. And the rows are
// already gone by then — refusing would not un-remove them, it would only
// hide from the caller which ones went, which is the worst of the three
// available answers.
//
// Read that way the two sentences in the ticket agree rather than collide:
// the operation stops at the ceiling and refuses to go further, and it does
// not return a SHORTENED answer, because the answer it returns says it is
// short. See this task's report for the note that the ticket can also be read
// as asking for ErrCeiling here, and why that reading loses to the acceptance
// criteria it sits beside.
//
// # Collected first, removed after
//
// A visitor cannot delete from the tree it is being walked by. btree.ascend
// decodes a branch page once and then reads node.children[i] for each i in
// turn; a delete partway through can merge, rebalance and free exactly those
// pages, so the walk would go on to read a page that is no longer the page it
// decided to read. So the keys are collected while the walk runs and removed
// after it ends.
//
// This store already knew that, and this is the second place it is written
// down rather than the first: store.go's own deleteRange(prefix) — the
// byte-level one Drop uses to unlink a whole collection — collects into
// `doomed` and deletes afterwards, for the same reason in the same words.
// Which is also why the function below is runDeleteRange and not deleteRange:
// the good name was taken by the older, lower thing, and renaming that one to
// free it would have touched Drop for the sake of a name.
//
// Collecting is the one promise hashRange makes that this cannot keep:
// hashRange holds one row at a time and drops it, and this holds one key per
// row it is about to remove. The memory is not unbounded — it is the declared
// limit's worth of primary keys, and the limit is the number the declaration
// exists to say — but it is memory that grows with the limit, and saying so
// is better than letting somebody discover it by declaring a limit of ten
// million.

// runDeleteRange removes the declared stretch in key order, up to the declared
// limit, and hands back the cursor a caller needs to continue: how many went,
// the last key removed, and whether more remain.
//
// It takes the collection and the already-resolved Range for the same reason
// run() and hashRange() do: the declaration decided the access path, and this
// is only the walking of it.
func (s *Store) runDeleteRange(by Attribution, collection *Collection, operation Operation,
	within Range, result *Result) error {

	// removing is the keys this run will take, and its length is the counter
	// the ceiling is checked against. There is exactly one of it for the
	// whole run — a counter per underlying call is the W7 shape, and a walk
	// that started one per partition file would be that shape wearing this
	// store's clothes.
	removing := []any{}

	// more is what a hashRange carries out of its visitor as `refused`, and
	// the difference is the whole of the paragraph above: a visitor can only
	// say "stop", so something has to come out with it saying why it
	// stopped. For a digest that has to be an error. Here it is a flag,
	// because the answer this action returns has a field for it and the
	// caller can act on it — which is what makes stopping honest rather than
	// silent.
	more := false

	err := collection.walkKeys(within, func(key any) bool {
		// Checked before the key is taken rather than after, so the limit is
		// the number of rows this may REMOVE and not that number plus one.
		// Reaching this branch at all means the walk found one more key
		// inside the stretch than the declaration allows to go, which is
		// exactly what tells the caller there is another page — the same
		// one-row-past-the-limit rule a scan uses to set Truncated.
		if len(removing) >= operation.Limit {
			more = true
			return false
		}
		removing = append(removing, key)
		return true
	})
	if err != nil {
		return err
	}

	for _, key := range removing {
		removed, err := collection.DeleteBy(by, key)
		if err != nil {
			return err
		}
		if !removed {
			// The walk found it a moment ago and nothing else holds this
			// database while an operation runs, so this is damage rather
			// than a race. Counting it as removed would make Changed a
			// number that does not match the log.
			continue
		}
		// One entry per removal, written by remove() itself — the same entry,
		// in the same shape, that deleting this key on its own would write.
		result.Changed++
		// The last key removed, overwritten each time, so what survives the
		// loop is where this run got to. That is the cursor: the next call
		// asks for the same stretch starting exclusively after it.
		result.Key = key
	}

	result.Truncated = more
	return nil
}
