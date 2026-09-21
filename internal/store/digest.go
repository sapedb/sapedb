package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
)

// Hashing a stretch of keys without moving it.
//
// The question this answers is the one an application storing a large file as
// many small rows cannot answer any other way: did all of it arrive, and is it
// still what it was? Reading it back to check means moving the whole file to
// wherever the check runs, which is exactly the thing the rows were split up
// to avoid. So the check runs here, and what crosses the wire is thirty-two
// bytes.
//
// Three rules make the answer worth having, and each of them is a rule about
// what is NOT done:
//
//   - Nothing between the values. A separator would make the chunk boundaries
//     part of the digest, and then the same bytes stored as one-kilobyte rows
//     and as four-kilobyte rows would hash differently — which defeats the
//     purpose, because how a file was split is not a fact about the file.
//   - Nothing skipped. A row whose field is missing, or is not text, or does
//     not decode, stops the whole walk and names the key. The rollup already
//     refuses a document it cannot add rather than skipping it, on the
//     grounds that a total that silently skips is a total nobody can trust;
//     a digest with a hole in it is worse, because thirty-two bytes with a
//     row missing look exactly like thirty-two bytes with none missing.
//   - Nothing held. The digest is fed a chunk at a time and the chunk is
//     dropped, so the memory this costs is one row, not one range. The joined
//     value never exists anywhere.
//
// # The ceiling is counted, not trusted
//
// This is the first thing in this store that walks a range at run time, and
// the property it breaks is worth writing down because everything else here
// still relies on it. Every other declaration is bounded BY CONSTRUCTION: a
// declaration cannot express a loop, because a step pins operation@version,
// version numbers only ever rise, and a pinned reference resolves only to a
// version that already exists — so a cycle would need a version to exist
// before it was declared. That is why ceiling() (compose.go) can total a
// declaration's whole cost at declare time and why nothing counts anything
// while a call runs.
//
// That property does not survive a thing that walks. The gap it opens is
// measured, in pipelines/tasks/0071 as measurement W7: an external operation
// declaring `limit 50`, correctly sandboxed and correctly signed, served
// fifty million rows in one call — one million host calls of fifty each —
// because the host checked every individual call and never the total. A
// signature proves whose binary it is. A sandbox proves it does not escape.
// Neither proves what it costs.
//
// So the limit here is counted by the host, against one counter, per RUN of
// the operation rather than per underlying call, and what is counted is rows
// read. At the ceiling the walk stops and returns ErrCeiling. It does not
// hand back the digest of the rows it managed: a digest of part of a range is
// indistinguishable from a digest of all of it, which makes a partial answer
// the most dangerous possible output — worse than no answer, because it is
// the same thirty-two hex characters in the same field and a caller comparing
// it with the file's own hash simply sees a mismatch and looks for corruption
// that is not there.

// hashRange walks the declared stretch in key order and puts the SHA-256 of
// the values it read, joined with nothing between them, into the result.
//
// It takes the collection and the already-resolved Range for the same reason
// run() does: the declaration decided the access path, and this is only the
// walking of it.
func (s *Store) hashRange(collection *Collection, operation Operation, within Range, result *Result) error {
	digest := sha256.New()

	// read is the counter the ceiling is checked against, and there is
	// exactly one of it for the whole run. A counter per underlying call is
	// the W7 shape, and a walk that reset one per partition file would be
	// that shape wearing this store's clothes.
	read := 0

	// refused carries the reason out of the callback. walkRange's visitor
	// can only say "stop", so a refusal that returned nothing else would come
	// back as a complete digest of a prefix — the one answer this must never
	// give.
	var refused error

	err := collection.walkRange(within, func(key any, document map[string]any) bool {
		// Checked before the row is read rather than after, so the limit is
		// the number of rows this may READ and not the number it may read
		// plus one. A declaration saying 5000 walks five thousand rows and
		// refuses on the five thousand and first.
		if read >= operation.Limit {
			refused = fmt.Errorf("%w: %q reached %d rows at key %v, and its digest is not handed back short — a digest of part of a range cannot be told apart from a digest of all of it",
				ErrCeiling, operation.Name, operation.Limit, key)
			return false
		}
		read++

		chunk, err := chunkOf(operation, collection.spec.Name, key, document)
		if err != nil {
			refused = err
			return false
		}
		// Fed and dropped. This is the whole of why the memory does not grow
		// with the range: nothing above this line holds a chunk, and nothing
		// below it keeps one.
		digest.Write(chunk)
		return true
	})
	if err != nil {
		return err
	}
	if refused != nil {
		return refused
	}

	result.Count = read
	result.Digest = hexOf(digest)
	return nil
}

// chunkOf is one document's contribution to the digest, or the refusal that
// says why it has none.
//
// Every refusal names the key, because the only useful thing to do about one
// is go and look at that row, and a message saying "some document in this
// range" sends somebody to read the whole range by hand — which is what this
// action exists so that nobody has to do.
func chunkOf(operation Operation, collection string, key any, document map[string]any) ([]byte, error) {
	value, found := at(document, operation.Field)
	if !found {
		return nil, fmt.Errorf("%w: %v in %q has no %q",
			ErrDigest, key, collection, operation.Field)
	}

	text, isText := value.(string)
	if !isText {
		return nil, fmt.Errorf("%w: %v in %q has %T at %q, and a digest is over text",
			ErrDigest, key, collection, value, operation.Field)
	}

	if operation.Decode != DecodeBase64 {
		return []byte(text), nil
	}

	// Strict, and the padding matters. A row is base64 of its own chunk, so a
	// chunk whose length is not a multiple of three ends in padding — which
	// every row of a file split at 1024 or 4096 bytes does. Decoding is what
	// removes that padding from the answer, and it is the reason the same
	// bytes split two ways hash the same: the padding is a fact about the
	// split, not about the file.
	chunk, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %v in %q does not read as base64 at %q: %v",
			ErrDigest, key, collection, operation.Field, err)
	}
	return chunk, nil
}

// hexOf is a finished digest as the lower-case hex a person compares by eye
// with what sha256sum printed.
//
// Sum is given a nil rather than a scratch buffer on purpose: it appends to
// what it is handed, so passing anything with content in it would produce
// that content followed by the digest, which is a bug that reads as a longer
// hash rather than as a wrong one.
func hexOf(digest hash.Hash) string {
	return hex.EncodeToString(digest.Sum(nil))
}
