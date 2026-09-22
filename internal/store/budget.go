package store

import (
	"encoding/json"
	"fmt"
)

// budget is how many bytes of rows one call may still build.
//
// It exists because a limit checked after the work is done is not a limit on
// the work. The frame cap is enforced in protocol.Encode, and it is enforced
// by measuring len() of a byte slice that already exists — so for it to
// refuse, every row has already been read out of the tree and marshalled.
// That is how a server whose steady state for this workload never left
// sixteen to twenty megabytes reached three hundred and fifty-two and was
// killed while building the payload it was about to be told it could not
// send (ISS-37). This is the same number, spent while the rows are
// accumulated rather than counted once they are all here.
//
// The other two read paths already had a ceiling of their own and neither is
// this one. Explore caps a typed scan at MostRows, which is a count of rows
// and not of bytes — a thousand rows is about two megabytes at the sizes
// ISS-37 measured and about a gigabyte if each row is a megabyte, because
// nothing here bounds a stored value. Subscribe sends one frame per change
// and so never holds the whole log. Invoke — the declared path, the one this
// product's entire argument rests on — had neither, because a declared limit
// is a promise about the operation and not a budget the server holds itself
// to. This is the budget.
//
// What it counts is the rows. They are the only part of an answer that grows
// with the data; the envelope around them — the operation's name, the
// version, the count, the truncated flag — is a few dozen bytes whatever the
// answer is, and the two brackets of the array are two more. So an answer
// this allowed can still be a few dozen bytes over what one reply may carry,
// and what catches that is the check in internal/server's answer(), which is
// the same late measurement this replaces, kept deliberately as a backstop
// over a sliver rather than over the whole answer.
//
// A zero most is no budget at all. That is what a Go caller embedding this
// store as a library gets: there is no frame for its answer to fit into, and
// borrowing a wire limit for a caller that is not on a wire would refuse
// reads that work today. The server sets it when it opens the file.
type budget struct {
	most int
	used int
	rows int
}

// spend charges one row, and refuses when the row would not fit.
//
// The row is marshalled to measure it. That is the price of knowing the size
// before the answer exists, and the bytes are dropped here — nothing above
// this line holds them and nothing below it keeps one, which is the shape
// hashRange's chunks already have. It is not free: a read now marshals every
// row it returns twice, once here for the length and once for the answer
// itself. The alternative is guessing at a row's encoded size, and a guess is
// what makes a budget something other than the number it claims to be.
//
// One byte is added per row for the comma it sits behind. An array of n rows
// carries n-1 commas and two brackets, so charging a comma per row counts
// exactly what encoding/json writes for the array bar its closing bracket —
// one byte over the array's own contents, and never under.
func (b *budget) spend(name string, row map[string]any) error {
	if b == nil || b.most <= 0 {
		return nil
	}

	written, err := json.Marshal(row)
	if err != nil {
		return err
	}

	want := b.used + len(written) + 1
	if want > b.most {
		return fmt.Errorf("%w: %q reached %d rows and %d bytes, and the most one answer may carry is %d — declare a smaller limit, or fewer fields in the projection, and read the range in pages",
			ErrTooLarge, name, b.rows+1, want, b.most)
	}

	b.used = want
	b.rows++
	return nil
}
