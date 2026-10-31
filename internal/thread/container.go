// SPDX-License-Identifier: AGPL-3.0-only

package thread

// container is a JWZ Container: a node in the threading tree that may or
// may not correspond to a real Message. A container with message == nil is
// a "dummy" or "empty" container, standing in for a message referenced by
// others but never seen (e.g. never delivered to this store, or filtered
// out upstream).
type container struct {
	// id is the Message-ID this container represents. It may be empty
	// (message had no Message-ID) and, for a message whose Message-ID
	// collided with an earlier message's, it is set for display purposes
	// only — such a container is deliberately never registered in the
	// builder's id_table (see builder.attachMessage), so lookups by other
	// messages' References keep resolving to the original. It is instead
	// parented under that original, so the two share a thread (#210).
	id string

	// message is nil for a dummy container.
	message *Message

	parent   *container
	children []*container
}

// builder holds the id_table (JWZ step 1) and every container allocated
// while threading one batch of messages.
type builder struct {
	idTable map[string]*container
	all     []*container
}

func newBuilder(n int) *builder {
	return &builder{idTable: make(map[string]*container, n)}
}

func (b *builder) newContainer(id string) *container {
	c := &container{id: id}
	b.all = append(b.all, c)
	return c
}

// attachMessage implements JWZ step 1 for a single message: find or create
// its container, filling in an existing empty container if one was already
// created by a forward reference from another message.
func (b *builder) attachMessage(m *Message) *container {
	if m.MessageID == "" {
		c := b.newContainer("")
		c.message = m
		return c
	}
	if existing, ok := b.idTable[m.MessageID]; ok {
		if existing.message == nil {
			existing.message = m
			return existing
		}
		// Duplicate Message-ID (spoofed or genuinely reused). RFC 5322 §3.6.4
		// makes a Message-ID globally unique, so two messages bearing one are
		// the same message and belong in ONE thread (#210). Give this message
		// its own container so none is lost, and never let it claim or
		// overwrite the id_table entry the original owns — but attach it UNDER
		// the original rather than leaving it to root on its own.
		//
		// ⚠ ATTACHING HERE, IN JWZ STEP 1, IS WHAT MAKES IT STICK. Thread runs
		// every attachMessage before any linkMessage, and linkParentChild
		// refuses to re-parent a container that already has a parent, so the
		// duplicate's own References can no longer pull it into a DIFFERENT
		// thread. Doing this in step 2 instead would race the reference walk.
		//
		// Before this, a duplicate with no References — which is most bulk
		// mail — became its own root and so its own thread. Measured on a
		// real mailbox: the duplicate Message-ID groups were split
		// across separate threads.
		//
		// ⚠ THE DIRECTION MATTERS AND MUST NOT BE REVERSED. The duplicate
		// becomes a CHILD of the original: the original keeps its parent, its
		// id_table entry and its position. So a message bearing a Message-ID
		// it does not own can ADD itself to a thread — which it could already
		// do by naming that id in References, which is why the spoofing
		// concern this code used to cite is not load-bearing — but it cannot
		// REMOVE, re-parent or displace the message that was already there.
		// TestDuplicateMessageIDCannotDisplaceTheOriginal pins that.
		c := b.newContainer(m.MessageID)
		c.message = m
		linkParentChild(existing, c)
		return c
	}
	c := b.newContainer(m.MessageID)
	c.message = m
	b.idTable[m.MessageID] = c
	return c
}

// getOrCreate returns the container registered under id, creating a dummy
// one if none exists yet. Returns nil for an empty id.
func (b *builder) getOrCreate(id string) *container {
	if id == "" {
		return nil
	}
	if c, ok := b.idTable[id]; ok {
		return c
	}
	c := b.newContainer(id)
	b.idTable[id] = c
	return c
}

// rootSet returns every allocated container with no parent (JWZ step 3).
func (b *builder) rootSet() []*container {
	roots := make([]*container, 0, len(b.all))
	for _, c := range b.all {
		if c.parent == nil {
			roots = append(roots, c)
		}
	}
	return roots
}

// effectiveReferences returns the chain of ancestor Message-IDs to link,
// oldest first: References if present, otherwise In-Reply-To as the
// reference of last resort.
func effectiveReferences(m *Message) []string {
	if len(m.References) > 0 {
		return m.References
	}
	return m.InReplyTo
}

// linkMessage implements JWZ step 2 for one message: walk its reference
// chain, creating/finding dummy containers as needed and linking each as
// the parent of the next, with the message's own container linked as the
// child of the last reference.
func linkMessage(b *builder, m *Message, self *container) {
	var parent *container
	for _, id := range effectiveReferences(m) {
		if id == "" || id == m.MessageID {
			continue // ignore empty and self-referential ids
		}
		c := b.getOrCreate(id)
		if parent != nil {
			linkParentChild(parent, c)
		}
		parent = c
	}
	if parent != nil {
		linkParentChild(parent, self)
	}
}

// linkParentChild sets parent as child's parent, unless doing so is unsafe
// or redundant:
//
//   - a container already claiming a parent keeps it (the first-seen
//     relationship wins — later, possibly-stale References data does not
//     override it);
//   - a link that would make child an ancestor of parent (i.e. parent is
//     already reachable by walking up from child, or parent == child) is
//     rejected, which is what makes reference loops (A references B, B
//     references A) terminate instead of cycling.
func linkParentChild(parent, child *container) {
	if parent == nil || child == nil || parent == child {
		return
	}
	if child.parent != nil {
		return
	}
	if isAncestor(child, parent) {
		return
	}
	parent.children = append(parent.children, child)
	child.parent = parent
}

// isAncestor reports whether candidate appears in of's parent chain.
func isAncestor(candidate, of *container) bool {
	for c := of; c != nil; c = c.parent {
		if c == candidate {
			return true
		}
	}
	return false
}

// pruneRoots implements JWZ step 4 over the whole root set: prune empty
// containers below the root unconditionally, and at the root itself only
// when there is exactly one child to promote in its place.
func pruneRoots(roots []*container) []*container {
	out := make([]*container, 0, len(roots))
	for _, c := range roots {
		pruneChildren(c)
		if c.message == nil {
			switch len(c.children) {
			case 0:
				continue // empty container, no children: drop it
			case 1:
				child := c.children[0]
				child.parent = nil
				out = append(out, child)
				continue
			default:
				// Keep as a dummy grouping root: removing it would
				// otherwise merge unrelated branches into the root set.
			}
		}
		out = append(out, c)
	}
	return out
}

// pruneChildren recursively (post-order) removes empty containers from c's
// subtree below the root: an empty container with no children is dropped,
// and an empty container with children is spliced out with its children
// promoted to c's own child list, regardless of how many children it has —
// the root-only "keep if >1 children" exception in pruneRoots does not
// apply below the root.
func pruneChildren(c *container) {
	// Deliberately not filtering in place: a single slot in c.children can
	// expand into zero, one, or several grandchildren once spliced, which
	// would otherwise overwrite not-yet-read elements of the same backing
	// array.
	kept := make([]*container, 0, len(c.children))
	for _, child := range c.children {
		pruneChildren(child)
		if child.message == nil {
			if len(child.children) == 0 {
				continue // drop
			}
			for _, gc := range child.children {
				gc.parent = c
			}
			kept = append(kept, child.children...)
			continue
		}
		kept = append(kept, child)
	}
	c.children = kept
}
