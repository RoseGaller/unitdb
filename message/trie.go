package message

import (
	"sync"
)

const (
	// nMutex = 16

	nul = 0x0
)

// ssid represents a sequence set which can contain only unique values.
type ssid []uint64

// addUnique adds a seq to the set.
func (ss *ssid) addUnique(value uint64) (added bool) {
	if ss.contains(value) == false {
		*ss = append(*ss, value)
		added = true
	}
	return
}

// remove a seq from the set.
func (ss *ssid) remove(value uint64) (removed bool) {
	for i, v := range *ss {
		// if bytes.Equal(v, value) {
		if v == value {
			a := *ss
			a[i] = a[len(a)-1]
			//a[len(a)-1] = nil
			a = a[:len(a)-1]
			*ss = a
			removed = true
			return
		}
	}
	return
}

// contains checks whether a seq is in the set.
func (ss *ssid) contains(value uint64) bool {
	for _, v := range *ss {
		// if bytes.Equal(v, value) {
		// 	return true
		// }
		if v == value {
			return true
		}
	}
	return false
}

type key struct {
	query     uint32
	wildchars uint8
}

type part struct {
	k        key
	depth    uint8
	ss       ssid
	parent   *part
	children map[key]*part
}

func (p *part) orphan() {
	if p.parent == nil {
		return
	}

	delete(p.parent.children, p.k)
	if len(p.parent.ss) == 0 && len(p.parent.children) == 0 {
		p.parent.orphan()
	}
}

// partTrie represents an efficient collection of Trie with lookup capability.
type partTrie struct {
	root *part // The root node of the tree.
}

// NewPartTrie creates a new matcher for the Trie.
func NewPartTrie() *partTrie {
	return &partTrie{
		root: &part{
			ss:       ssid{},
			children: make(map[key]*part),
		},
	}
}

// Trie trie data structure to store topic parts
type Trie struct {
	sync.RWMutex
	Mutex
	partTrie *partTrie
	count    int // Number of Trie in the Trie.
}

// NewTrie new trie creates a Trie with an initialized Trie.
// Mutex is used to lock concurent read/write on a contract, and it does not lock entire trie.
func NewTrie() *Trie {
	trie := &Trie{
		Mutex:    NewMutex(),
		partTrie: NewPartTrie(),
	}

	return trie
}

// Count returns the number of entries in Trie.
func (t *Trie) Count() int {
	t.RLock()
	defer t.RUnlock()
	return t.count
}

// Add adds a message seq to topic trie.
func (t *Trie) Add(contract uint64, parts []Part, depth uint8, seq uint64) (added bool) {
	// Get mutex
	mu := t.GetMutex(contract)
	mu.Lock()
	defer mu.Unlock()
	curr := t.partTrie.root
	for _, p := range parts {
		k := key{
			query:     p.Query,
			wildchars: p.Wildchars,
		}
		t.RLock()
		child, ok := curr.children[k]
		t.RUnlock()
		if !ok {
			child = &part{
				k:        k,
				ss:       ssid{},
				parent:   curr,
				children: make(map[key]*part),
			}
			t.Lock()
			curr.children[k] = child
			t.Unlock()
		}
		curr = child
	}
	// if ok := curr.ss.addUnique(ssid); ok {
	curr.ss = append(curr.ss, seq)
	added = true
	curr.depth = depth
	t.count++
	// }

	return
}

// Remove removes a message seq from topic trie
func (t *Trie) Remove(contract uint64, parts []Part, seq uint64) (removed bool) {
	mu := t.GetMutex(contract)
	mu.Lock()
	defer mu.Unlock()
	curr := t.partTrie.root

	for _, part := range parts {
		k := key{
			query:     part.Query,
			wildchars: part.Wildchars,
		}
		t.RLock()
		child, ok := curr.children[k]
		t.RUnlock()
		if !ok {
			removed = false
			// message seq doesn't exist.
			return
		}
		curr = child
	}
	// Remove a message seq and decrement the counter
	if ok := curr.ss.remove(seq); ok {
		removed = true
		t.count--
	}
	// Remove orphans
	t.Lock()
	defer t.Unlock()
	if len(curr.ss) == 0 && len(curr.children) == 0 {
		curr.orphan()
	}
	return
}

// Lookup returns seq set for given topic.
func (t *Trie) Lookup(contract uint64, parts []Part) (ss ssid) {
	t.RLock()
	defer t.RUnlock()
	t.ilookup(contract, parts, uint8(len(parts)-1), &ss, t.partTrie.root)
	return
}

func (t *Trie) ilookup(contract uint64, parts []Part, depth uint8, ss *ssid, part *part) {
	// Add seq set from the current branch
	for _, s := range part.ss {
		if part.depth == depth || (part.depth >= TopicMaxDepth && depth > part.depth-TopicMaxDepth) {
			// ss.addUnique(s)
			*ss = append(*ss, s)
		}
	}

	// If we're not yet done, continue
	if len(parts) > 0 {
		// Go through the exact match branch
		for k, p := range part.children {
			if k.query == parts[0].Query && uint8(len(parts)) >= k.wildchars+1 {
				t.ilookup(contract, parts[k.wildchars+1:], depth, ss, p)
			}
		}
	}
}
