package number

import (
	"fmt"
	"reflect"
	"strings"
)

// A declaration is not a call, and that is why it needs its own walk.
//
// An invoke arrives as map[string]any: one shape, walked by Exact, every
// number in it reachable by a type switch. A declaration does not. It arrives
// as store.Spec or store.Operation — typed structs, with the constants a
// caller wrote buried in the handful of fields declared `any`:
//
//	store.Term.Value        a constant written into a document, a key, a bound
//	store.Parameter.Default what an omitted argument stands for
//	store.Access.Key        which document a typed `get` wants
//	store.Bound.Values      where a typed scan starts and stops
//
// Those are the ones that exist today. Enumerating them here by name would
// make this file a list that has to be remembered every time somebody adds a
// field, and the failure mode of forgetting is silent — exactly the failure
// ISS-35 is about. So the walk is over the shape rather than over a list: it
// descends the struct and hands every `any` it reaches to Exact, which is the
// same function the invoke door calls on the same kind of value. A field added
// to a declaration tomorrow is covered the day it is added.
//
// # Where this runs, and why it cannot run later
//
// Immediately after the decode that produced the struct, on a decoder that was
// told UseNumber — the same instant, and for the same reason, as the invoke
// door. The package doc above has the argument in full; the short form is that
// a `float64` in Term.Value no longer knows whether it was rounded, so the
// only place the question can be answered is while the literal's text is still
// there. `Term.Value` is `any`, so UseNumber puts a json.Number there and the
// text survives exactly that long.
//
// # What it leaves behind
//
// float64, everywhere it found a json.Number that survives. The store, the key
// encoding and the change log expect float64 in these fields and say so; a
// json.Number reaching them would be a value of a type none of them know.
// Nothing downstream changes because nothing downstream sees a difference.

// ExactIn checks every constant a declaration carries, and replaces the ones
// that survive with float64.
//
// It takes a non-nil pointer because it rewrites what it walks: a declaration
// passed by value would be checked and then thrown away, which is a check that
// reads as working and stores json.Number anyway.
//
// `where` names the thing being checked — `collection "ids"`, `operation
// "ids.put"` — and the path of each field is appended to it, so a refusal says
// which constant of which declaration rather than which file.
func ExactIn(where string, declaration any) error {
	held := reflect.ValueOf(declaration)
	if held.Kind() != reflect.Pointer || held.IsNil() {
		return fmt.Errorf("sapedb/number: ExactIn walks a non-nil pointer, and was given %T", declaration)
	}
	return walk(where, held.Elem())
}

// walk descends one value. Only the `any` fields are of interest; everything
// else is either a type JSON decoded without going through float64 at all
// (string, bool, int) or a container on the way to an `any`.
func walk(where string, held reflect.Value) error {
	switch held.Kind() {
	case reflect.Interface:
		if held.IsNil() {
			return nil
		}
		if !held.CanSet() {
			// Unreachable from ExactIn, which starts at a pointer and only
			// ever descends through settable values. Loud rather than
			// skipped: a silent return here would be a constant that was
			// looked at and then stored as a json.Number anyway.
			return fmt.Errorf("sapedb/number: %s cannot be written back", where)
		}
		checked, err := Exact(where, held.Interface())
		if err != nil {
			return err
		}
		if checked == nil {
			held.Set(reflect.Zero(held.Type()))
			return nil
		}
		held.Set(reflect.ValueOf(checked))
		return nil

	case reflect.Pointer:
		if held.IsNil() {
			return nil
		}
		return walk(where, held.Elem())

	case reflect.Struct:
		shape := held.Type()
		for i := 0; i < shape.NumField(); i++ {
			field := shape.Field(i)
			if !field.IsExported() {
				continue
			}
			if err := walk(where+"."+nameOf(field), held.Field(i)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Slice, reflect.Array:
		for i := 0; i < held.Len(); i++ {
			if err := walk(fmt.Sprintf("%s[%d]", where, i), held.Index(i)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Map:
		// A map entry is not addressable, so it is copied out into something
		// that is, walked, and put back. The copy is why this is not simply
		// held.MapIndex(key) passed down: reflect would refuse the Set.
		for _, key := range held.MapKeys() {
			entry := reflect.New(held.Type().Elem()).Elem()
			entry.Set(held.MapIndex(key))
			if err := walk(fmt.Sprintf("%s.%v", where, key.Interface()), entry); err != nil {
				return err
			}
			held.SetMapIndex(key, entry)
		}
		return nil
	}
	return nil
}

// nameOf is the field's name as the caller wrote it, which is its JSON tag
// when it has one. A refusal that said `Value` where the file says `value`
// would be naming a Go field at somebody reading a schema file.
func nameOf(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" || tag == "-" {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}
