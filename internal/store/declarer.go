package store

// Who declared an operation (ISS-32).
//
// SAPE-9 recorded who owns a NAMESPACE, in a record of its own (spaceClaims,
// namespace.go). It recorded nothing on the declaration itself. The change log
// carries an Attribution for every declare — that is what DeclareOperation's
// own doc is about — but the log is the log: it is trimmed, it is followed,
// and it is not what a dump carries. dump.go's `line` has no attribution
// anywhere on it and Restore handles only collections, operations and
// documents, so a database dumped and restored today holds every declaration
// and knows who wrote none of them. In the unnamed namespace — every flat name
// there has ever been — there is not even a claim record to fall back on, so
// for those names the answer is gone the moment the log is.
//
// A field fixes that for free, because a dump carries the whole Operation. The
// reason it lands now rather than in a 1.1 is COMPATIBILITY.md section 3: a
// 1.x MAY add an optional field to a declaration, so this one could in
// principle arrive later — but the declaration is stored INSIDE the database,
// and every operation declared between the tag and that release would be one
// this field can never be filled in for. The rule can wait; the field cannot.
//
// WHAT IT IS NOT. It is a record, not a rule. Nothing reads it to decide
// anything: who may declare into a namespace is still settled by claimFor and
// spaceClaims, and this field changes no answer that check gives. Writing down
// who did something and deciding who may do it are two different jobs, and a
// field that quietly started doing the second would be a rule nobody could
// find.

// Declarer is the identity that declared one version of an operation.
//
// Kind and identity, the same two facts and the same vocabulary as Claim — a
// key and an actor spelled identically are two different declarers, not one,
// and the kind is stored rather than guessed from the string for exactly the
// reason namespace.go gives for storing it on a claim.
//
// A SEPARATE TYPE RATHER THAN Claim ITSELF, and the reason is Claim's third
// field. A Claim carries the Namespace it is a claim ON, which is the fact
// that makes it a claim; on a declaration that field would be either empty
// (every flat name, which is most of them) or a second copy of a substring of
// Operation.Name. A second copy is a field that can disagree with the first
// after any round trip through JSON, and a reader would then have to be told
// which of the two wins. Reusing the shape would save one type declaration and
// buy a field whose only possible states are redundant or wrong. The
// vocabulary — ClaimedByKey and ClaimedByActor — is shared, because that is
// the part that has to mean the same thing in both places.
type Declarer struct {
	// Kind is ClaimedByKey or ClaimedByActor. See namespace.go, which is where
	// both constants are declared and documented.
	Kind string `json:"kind"`
	// Identity is the key or the actor, depending on Kind. The two are never
	// compared across kinds.
	Identity string `json:"identity"`
}

// declarerOf is the Declarer a caller would be recorded as, or nil when the
// caller names nobody.
//
// The signing key wins over the actor, the same order and for the same reason
// holder() uses it: a key was checked by ed25519 over exactly the declarations
// it installed, and an actor is a name somebody was called at the time.
//
// A caller that carries NEITHER records nothing rather than recording an actor
// spelled "". That case is real — the shell and a good deal of this package's
// own test suite declare through an empty Caller — and an entry saying the
// declarer is the actor whose name is the empty string asserts a fact nobody
// supplied. Absent is the honest shape for it, and absent is a state this
// field has to read correctly forever anyway, because every declaration made
// before this existed is in it.
func declarerOf(caller Caller) *Declarer {
	if caller.Signer != "" {
		return &Declarer{Kind: ClaimedByKey, Identity: caller.Signer}
	}
	if caller.Actor != "" {
		return &Declarer{Kind: ClaimedByActor, Identity: caller.Actor}
	}
	return nil
}
