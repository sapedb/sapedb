package store

import (
	"fmt"
)

// Invoke runs a declared operation. This is the only way to read or write.
//
// Nothing here decides anything the declaration did not already decide: the
// arguments are checked against what was declared, the values they carry go
// into the places the declaration left for them, and the access path is the
// one that was written down. An argument cannot widen a scan, reach another
// collection, or become part of a key.
func (s *Store) Invoke(caller Caller, name string, version int, arguments map[string]any) (Result, error) {
	operation, found, err := s.Operation(name, version)
	if err != nil || !found {
		return Result{}, err
	}

	if err := allowed(caller, operation); err != nil {
		return Result{}, err
	}

	return s.perform(caller, operation, arguments)
}

// perform runs an operation that has already been found and allowed.
//
// Separate from Invoke because the operator shell runs operations that were
// never declared — it builds the one its typed access would have to be
// declared as and runs that. Sharing this means an access somebody types goes
// down the same path as one somebody declared, rather than down a second path
// that agrees with the first until the day it does not.
func (s *Store) perform(caller Caller, operation Operation, arguments map[string]any) (Result, error) {
	values, err := bind(operation, arguments)
	if err != nil {
		return Result{}, err
	}

	result := Result{Operation: operation.Name, Version: operation.Version}
	by := Attribution{
		Operation: operation.Name,
		Version:   operation.Version,
		Actor:     caller.Actor,
		WriteID:   caller.WriteID,
	}

	// A write that arrives twice under one id is answered from what the first
	// one did. The connection dropping after the write landed but before the
	// answer got back is the ordinary case, and a caller that retries then must
	// not end up with two documents.
	if writes(operation.Action) && caller.WriteID != "" {
		lsn, key, done, err := s.Wrote(caller.WriteID)
		if err != nil {
			return Result{}, err
		}
		if done {
			result.Key = key
			result.Changed = 1
			result.Repeated = lsn
			return result, nil
		}
	}

	if err := s.runInline(by, operation, values, &result); err != nil {
		return Result{}, err
	}

	// An operation is a transaction, so this is where it ends. Leaving the
	// commit to whoever called meant every caller had to remember, and the one
	// that forgot would not find out until a batch refused to start on top of
	// its half-written work — which is a long way from where the mistake was.
	if result.Changed > 0 {
		if err := s.Commit(); err != nil {
			return Result{}, err
		}
	}

	return result, nil
}

// runInline does one operation's work into a Result that may already hold what
// came before it, and does not commit.
//
// It is separate from perform so that an operation named by a step of another
// operation runs down exactly this path and not a second one written beside
// it. What perform keeps for itself is the part that is true only of the
// operation a caller named: binding the caller's arguments, answering a repeat
// of a write id from what the first one did, and committing. A called
// operation has none of those — its arguments come from a declaration, the
// write id covers the whole transaction rather than each part of it, and there
// is one transaction, which is not its to end.
func (s *Store) runInline(by Attribution, operation Operation, values map[string]any, result *Result) error {
	collection, err := s.Collection(operation.Collection)
	if err != nil {
		return err
	}

	switch operation.Action {
	case ActionBatch:
		if err := s.runBatch(by, operation, values, result); err != nil {
			return err
		}

	case ActionGet:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return err
		}
		document, found, err := collection.Get(key)
		if err != nil {
			return err
		}
		if found {
			result.Rows = []map[string]any{project(document, operation.Projection)}
			result.Count = 1
		}

	case ActionTotals:
		within, err := bounds(operation, values)
		if err != nil {
			return err
		}
		err = collection.Totals(operation.Rollup, within, func(row Totals) bool {
			if len(result.Rows) >= operation.Limit {
				result.Truncated = true
				return false
			}
			result.Rows = append(result.Rows, rowOf(row))
			result.Count = len(result.Rows)
			return true
		})
		if err != nil {
			return err
		}

	case ActionScan, ActionCount:
		within, err := bounds(operation, values)
		if err != nil {
			return err
		}
		if err := s.run(collection, operation, within, result); err != nil {
			return err
		}

	case ActionHashRange:
		// The same bounds() every other range read goes through, so a
		// hashRange's stretch is worked out by the one calculation rather
		// than by a second one written beside it — including the refusal of
		// a From that sorts after To, which a digest wants for exactly the
		// reason a scan does.
		within, err := bounds(operation, values)
		if err != nil {
			return err
		}
		if err := s.hashRange(collection, operation, within, result); err != nil {
			return err
		}

	case ActionDeleteRange:
		// The same bounds() again, for the same reason, and here it also
		// buys the refusal of a From that sorts after To — which on a
		// destructive action is worth more than on a read: a backwards
		// stretch that quietly read as empty would report "removed nothing,
		// nothing more to do" about a range somebody meant to clear, and the
		// rows would still be there.
		within, err := bounds(operation, values)
		if err != nil {
			return err
		}
		if err := s.runDeleteRange(by, collection, operation, within, result); err != nil {
			return err
		}

	case ActionInsert, ActionPut:
		document, err := build(operation.Document, values)
		if err != nil {
			return err
		}
		if operation.Action == ActionInsert {
			if key, carries := document[collection.spec.Key.Path]; carries && key != nil {
				if _, taken, err := collection.Get(key); err != nil {
					return err
				} else if taken {
					return fmt.Errorf("%w: %v in %q", ErrExists, key, collection.spec.Name)
				}
			}
		}
		key, err := collection.PutBy(by, document)
		if err != nil {
			return err
		}
		result.Key = key
		result.Changed = 1

	case ActionUpdate:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return err
		}
		document, found, err := collection.Get(key)
		if err != nil {
			return err
		}
		if !found {
			break
		}
		changes, err := build(operation.Set, values)
		if err != nil {
			return err
		}
		for field, value := range changes {
			document[field] = value
		}
		if _, err := collection.PutBy(by, document); err != nil {
			return err
		}
		result.Key = key
		result.Changed = 1

	case ActionDelete:
		key, err := resolve(*operation.Key, values)
		if err != nil {
			return err
		}
		removed, err := collection.DeleteBy(by, key)
		if err != nil {
			return err
		}
		result.Key = key
		if removed {
			result.Changed = 1
		}

	default:
		return fmt.Errorf("%w: %q", ErrDeclaration, operation.Action)
	}

	return nil
}

// run walks the declared stretch of the declared index, stopping at the
// declared limit and saying so.
func (s *Store) run(collection *Collection, operation Operation, within Range, result *Result) error {
	limit := operation.Limit
	counting := operation.Action == ActionCount

	visit := func(key any, document map[string]any) bool {
		if counting {
			// A declared limit stops a count too, and says so. Without one it
			// counts the whole range: a count returns a number rather than
			// rows, so the cost is a walk and the answer is not partial.
			if limit > 0 && result.Count >= limit {
				result.Truncated = true
				return false
			}
			result.Count++
			return true
		}

		if len(result.Rows) >= limit {
			// One row past the limit is what tells the caller there was more.
			result.Truncated = true
			return false
		}
		result.Rows = append(result.Rows, project(document, operation.Projection))
		result.Count = len(result.Rows)
		return true
	}

	if operation.Index == ClusteredIndex || operation.Index == "" {
		return collection.walkRange(within, visit)
	}

	return collection.Scan(operation.Index, within, func(entry Found) bool {
		document, found, err := collection.Get(entry.Key)
		if err != nil || !found {
			// An index entry with no document behind it is damage, not an
			// empty row: the two are written together and must stay together.
			return false
		}
		return visit(entry.Key, document)
	})
}

// writes says whether an action changes anything, which decides whether a
// write id means something for it.
func writes(action string) bool { return Writes(action) }

// Writes says whether an action changes the database.
//
// It is exported so that a caller deciding whether to let an operation run at
// all — a server holding a follower, which may not write — asks the same
// question, of the same list, that SharedRead asks when it decides between the
// read lock and the write lock. A second list kept somewhere else is a list
// that will one day disagree with this one, and the day it does, the caller
// that trusted it lets a write through.
func Writes(action string) bool {
	switch action {
	case ActionInsert, ActionPut, ActionUpdate, ActionDelete, ActionBatch, ActionDeleteRange:
		return true
	}
	return false
}

// allowed is the fail-closed scope check: every scope the operation names must
// be one the caller presents.
func allowed(caller Caller, operation Operation) error {
	held := map[string]bool{}
	for _, scope := range caller.Scopes {
		held[scope] = true
	}
	for _, scope := range operation.Scopes {
		if !held[scope] {
			return fmt.Errorf("%w: %q needs %q", ErrNotAllowed, operation.Name, scope)
		}
	}
	return nil
}

// bind checks the arguments of a call against what the operation declares, and
// fills in the defaults.
//
// An argument the operation does not declare is refused rather than ignored.
// Ignoring it would let a caller believe something was applied that was not,
// which for a store meant to be driven by generated code is the worst of the
// three possible answers.
func bind(operation Operation, arguments map[string]any) (map[string]any, error) {
	values := make(map[string]any, len(operation.Input))
	declared := make(map[string]bool, len(operation.Input))

	for _, parameter := range operation.Input {
		declared[parameter.Name] = true

		value, given := arguments[parameter.Name]
		if !given {
			if parameter.Required {
				return nil, fmt.Errorf("%w: %q needs %q", ErrArgument, operation.Name, parameter.Name)
			}
			if parameter.Default != nil {
				values[parameter.Name] = parameter.Default
			}
			continue
		}
		if !matches(parameter.Type, value) {
			return nil, fmt.Errorf("%w: %q is a %s, and this is %T", ErrArgument, parameter.Name, parameter.Type, value)
		}
		values[parameter.Name] = value
	}

	for name := range arguments {
		if !declared[name] {
			return nil, fmt.Errorf("%w: %q does not take %q", ErrArgument, operation.Name, name)
		}
	}
	return values, nil
}

// resolve is the value a term stands for.
func resolve(term Term, values map[string]any) (any, error) {
	if term.Arg == "" {
		return term.Value, nil
	}
	value, given := values[term.Arg]
	if !given {
		return nil, fmt.Errorf("%w: %q was not given and has no default", ErrArgument, term.Arg)
	}
	return value, nil
}

// bounds turns the declared endpoints into the range to walk.
//
// An endpoint whose arguments were not given falls away: an operation that
// takes an optional "since" is unbounded below when nobody passes one, which
// is the ordinary way such an operation is written.
func bounds(operation Operation, values map[string]any) (Range, error) {
	within := Range{}

	for _, end := range []struct {
		declared **Endpoint
		into     **Bound
	}{
		{&operation.From, &within.From},
		{&operation.To, &within.To},
	} {
		endpoint := *end.declared
		if endpoint == nil {
			continue
		}

		bound := &Bound{Exclusive: endpoint.Exclusive}
		for _, term := range endpoint.Terms {
			if term.Arg != "" {
				if _, given := values[term.Arg]; !given {
					bound = nil
					break
				}
			}
			value, err := resolve(term, values)
			if err != nil {
				return Range{}, err
			}
			bound.Values = append(bound.Values, value)
		}
		*end.into = bound
	}

	// The direction does not fall away when it is missing the way an endpoint
	// does: a missing bound is a wider stretch, which is a thing an operation
	// can mean, but a missing direction is no order at all. The declaration is
	// checked so that this cannot happen — the argument is required or has a
	// default — so if it does happen it is an error rather than a guess.
	direction, err := directionFor(operation.Direction, values)
	if err != nil {
		return Range{}, err
	}
	within.Direction = direction

	return within, nil
}

// directionFor is which way this call runs along the index.
func directionFor(term *Term, values map[string]any) (Direction, error) {
	if term == nil {
		return Forward, nil
	}
	value, err := resolve(*term, values)
	if err != nil {
		return Forward, err
	}
	direction, err := directionOf(value)
	if err != nil {
		return Forward, fmt.Errorf("%w: the direction: %v", ErrArgument, err)
	}
	return direction, nil
}

// build makes a document, or the changes to one, out of terms.
func build(terms map[string]Term, values map[string]any) (map[string]any, error) {
	document := make(map[string]any, len(terms))

	for field, term := range terms {
		if term.Arg != "" {
			if _, given := values[term.Arg]; !given {
				// An optional argument that was not passed leaves the field
				// alone rather than writing a null over it.
				continue
			}
		}
		value, err := resolve(term, values)
		if err != nil {
			return nil, err
		}
		document[field] = value
	}
	return document, nil
}

// project keeps only the declared fields. No projection returns the document.
func project(document map[string]any, fields []string) map[string]any {
	if len(fields) == 0 {
		return document
	}

	kept := make(map[string]any, len(fields))
	for _, path := range fields {
		if value, found := at(document, path); found {
			kept[path] = value
		}
	}
	return kept
}

// rowOf is a rollup row as a document, so that a caller reading totals gets
// the same shape back as a caller reading anything else.
func rowOf(row Totals) map[string]any {
	made := map[string]any{"count": float64(row.Count)}
	if len(row.Group) > 0 {
		made["group"] = row.Group
	}
	for path, total := range row.Sum {
		made[path] = total
	}
	return made
}

// SharedRead is the operation a call names, and whether it can run while
// other readers run.
//
// Two things have to hold for it to be shared. The action must not change
// anything — that is what writes() already decides. And the collection must
// not be partitioned: for a partitioned collection even a get goes through
// Collection.into, which opens the partition file if it is not open yet
// (mutating the open set, and creating the file when it is new) and then runs
// expiry, which drops files. A "read" there is a writer wearing a reader's
// name.
//
// The operation comes back so that a caller which decided to share does not
// have to look it up a second time: the lookup is itself a read of the tree,
// and paying for it twice on every call is most of what a cheap read costs.
func (s *Store) SharedRead(caller Caller, name string, version int) (Operation, bool, error) {
	operation, found, err := s.Operation(name, version)
	if err != nil || !found {
		return Operation{}, false, err
	}
	if err := allowed(caller, operation); err != nil {
		return Operation{}, false, err
	}
	if writes(operation.Action) {
		return operation, false, nil
	}
	collection, err := s.Collection(operation.Collection)
	if err != nil {
		return Operation{}, false, err
	}
	return operation, collection.spec.Partition == nil, nil
}

// Run performs an operation SharedRead already found and allowed.
func (s *Store) Run(caller Caller, operation Operation, arguments map[string]any) (Result, error) {
	return s.perform(caller, operation, arguments)
}
