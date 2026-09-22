// Package number is the one rule about a number this database cannot store as
// it was sent, and the one place that rule is written down.
//
// `number` is `float64`. A JSON number becomes one at the decode, and from
// that moment the digits the caller wrote are gone: 9007199254740993 and
// 9007199254740992 are the *same* float64 — the same 64 bits, indistinguishable
// by anything downstream. So a check placed after a decode, on the float, can
// only ever catch SOME of the losses, and not the ones worth catching:
//
//   - It cannot tell a rounded value from an exact one. Nothing about a
//     float64 records that it was rounded on the way in. 2^53 and 2^53+1
//     arrive as one value, so no test applied to that value can separate
//     them — not `matches` in internal/store, not internal/keys, not any
//     amount of care taken later.
//   - The only thing it *could* do is refuse floats by magnitude — refuse
//     anything above 2^53 — and that is a different rule with a different and
//     wrong answer: it would refuse 9007199254740994, which comes back exactly
//     as sent, while still being unable to tell 2^53+1 from 2^53.
//
// There is no honest version of this check downstream of a decode, so every
// door decodes with UseNumber and runs this package against the literal text
// the caller sent, which is the last place the original value still exists.
// Holding the digits back from float64 long enough to look at them is the
// whole technique.
//
// This package exists as a package, rather than as a function in the server,
// because there is more than one door. ISS-35's first half closed the invoke
// frame; its second half found that a declaration reaches the database through
// `sapedb apply`, `sapedb install`, the `declare`, `establish` and `explore`
// frames, and the operator shell — none of which pass the invoke door. Two
// predicates for "this number cannot be stored as sent" would be two answers
// that can drift, and the whole point is that they agree. So there is one, and
// it is here, and every door calls it.
//
// # What is refused, precisely
//
// A JSON number **written as a plain integer** — an optional `-`, then digits,
// with no `.` and no exponent — must be a value float64 holds exactly. Every
// other number is accepted exactly as before.
//
// The boundary among those integers is exact representability, not magnitude,
// and the two are not the same test:
//
//	9007199254740992    = 2^53        accepted   float64 holds it exactly
//	9007199254740993    = 2^53 + 1    REFUSED    nearest float64 is 2^53
//	9007199254740994    = 2^53 + 2    accepted   larger, and still exact
//	9223372036854775807 = 2^63 - 1    REFUSED    nearest float64 is 2^63
//	9223372036854775808 = 2^63        accepted   far larger, and still exact
//
// A rule that refused everything above 2^53 would refuse three of those five,
// and all three come back as the caller sent them. Above 2^53 the representable
// integers thin out; they do not stop.
//
// # Why the shape of the literal decides whether it is checked at all
//
// Because the shape is the caller saying which kind of number they meant, and
// it is the only statement of intent that reaches this database.
//
// A caller who writes 1234567890123456789 — no point, no exponent — is writing
// an identifier, and ISS-35 is about exactly those: a Snowflake id, an account
// number, a nanosecond timestamp. A caller who writes 1.5e300 has chosen
// scientific notation, which is float notation, and gets back the number they
// wrote when it is printed. Refusing them would be refusing a working caller —
// 1.5e300 is not a float64 exactly either, and neither is 0.1, and neither is
// almost every decimal fraction ever sent to this database. A refusal that
// eager would break more than the bug does, and ISS-35 puts decimals out of
// scope by name.
//
// This is deliberately a rule about the literal and not about its value, and
// the cost is written down rather than hidden: 9.007199254740993e15 is the
// same value as the second row above and is NOT refused, because it is not
// written as an integer. See number_test.go, which pins that as a known hole
// rather than leaving somebody to find it.
package number

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// ErrPrecision is a number a caller sent that `number` cannot carry as sent.
//
// It is its own sentinel, and through internal/server's codeFor its own wire
// code, rather than falling through to "failed" — the shape ISS-21 records. A
// client that reads "failed" is told its request broke and has nowhere to go.
// What actually happened is that one value in it cannot be stored as written,
// and the thing to do about it is send that value differently. Those are two
// different sentences and they need two different codes.
var ErrPrecision = errors.New("sapedb: that number cannot be stored as sent")

// Carries says whether float64 holds what this literal says, and — when it
// does not — what would be stored instead.
//
// The second return is formatted the way the value would actually be printed
// back out of the database, shortest-round-trip in plain notation, so the
// refusal quotes the number the caller would otherwise have found in a dump
// rather than a second rendering of it.
func Carries(literal json.Number) (value float64, instead string, ok bool) {
	text := literal.String()

	stored, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Outside float64's range altogether. encoding/json refused this
		// before these paths turned UseNumber on, so nothing newly fails
		// here: what changes is that it is refused under a code of its own
		// instead of as an unreadable request.
		return 0, "", false
	}

	if !plainInteger(text) {
		return stored, "", true
	}

	exact, valid := new(big.Rat).SetString(text)
	if !valid {
		return stored, "", true
	}
	held := new(big.Rat).SetFloat64(stored)
	if held != nil && held.Cmp(exact) == 0 {
		return stored, "", true
	}
	return stored, strconv.FormatFloat(stored, 'f', -1, 64), false
}

// plainInteger says whether this literal is written as an integer and nothing
// else: an optional minus, then at least one digit, and no `.` and no
// exponent. The text has already been through encoding/json's own number
// scanner by the time it gets here, so this is narrowing a valid JSON number
// rather than validating one.
func plainInteger(text string) bool {
	text = strings.TrimPrefix(text, "-")
	if text == "" {
		return false
	}
	return strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// ExactArguments checks every number a call carries and replaces the ones that
// survive with the float64 the rest of the database has always been handed.
//
// The replacement matters as much as the check: everything downstream — the
// store's `matches`, internal/keys, the change log — expects float64 and says
// so, and a json.Number leaking past this point would be a value of a type
// none of them know, which is a second bug wearing the first one's fix as a
// disguise. Nothing downstream changes because nothing downstream sees a
// difference.
func ExactArguments(arguments map[string]any) error {
	for name, value := range arguments {
		checked, err := Exact(strconv.Quote(name), value)
		if err != nil {
			return err
		}
		arguments[name] = checked
	}
	return nil
}

// Exact is ExactArguments applied to one value, naming where it was found.
//
// A document body is nested and unconstrained, and a number buried three
// levels inside one is rounded exactly as silently as a top-level argument is
// — so the walk goes all the way down, and carries a path so the refusal can
// say which field rather than which call.
func Exact(where string, value any) (any, error) {
	switch held := value.(type) {
	case json.Number:
		stored, instead, ok := Carries(held)
		if ok {
			return stored, nil
		}
		if instead == "" {
			return nil, fmt.Errorf("%w: %s is %s, which is outside the range a number can hold at all — a number is a float64",
				ErrPrecision, where, held.String())
		}
		return nil, fmt.Errorf("%w: %s is %s, and a number is a float64, which would store it as %s instead — send it as a string, or send a value a float64 holds exactly",
			ErrPrecision, where, held.String(), instead)

	case map[string]any:
		for key, inner := range held {
			checked, err := Exact(where+"."+key, inner)
			if err != nil {
				return nil, err
			}
			held[key] = checked
		}
		return held, nil

	case []any:
		for index, inner := range held {
			checked, err := Exact(fmt.Sprintf("%s[%d]", where, index), inner)
			if err != nil {
				return nil, err
			}
			held[index] = checked
		}
		return held, nil
	}
	return value, nil
}
