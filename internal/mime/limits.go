// SPDX-License-Identifier: AGPL-3.0-only

package mime

// Defensive limits applied while walking a message's MIME structure
// (docs/mime-policy.md, "nested message/rfc822"). These bound the work
// Parse/ParseBytes will do on hostile input: a message that hits one of
// these limits stops descending further, sets Message.Truncated on every
// ancestor in its Nested chain, and appends an explanatory Warnings entry
// — it is never a fatal error and never panics/OOMs.
const (
	// MaxDepth bounds how many levels deep the parser will descend into
	// nested structure: both multipart-in-multipart nesting and nested
	// message/rfc822 (or message/global) parts share this single depth
	// counter, since either can be used to build a nesting bomb.
	MaxDepth = 20

	// MaxParts bounds the total number of parts (leaf parts plus nested
	// message/rfc822 containers) visited across a message's whole tree,
	// including its Nested descendants.
	MaxParts = 2000

	// MaxTotalSize bounds the cumulative decoded byte size read out of all
	// parts across a message's whole tree (defends against a message —
	// nested or otherwise — that tries to make the parser materialize an
	// unbounded amount of memory).
	MaxTotalSize = 64 << 20 // 64 MiB
)
