package sharing

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

type conflictStatus int

const (
	// NoConflict is the status when the rev is in the revisions chain (OK)
	NoConflict conflictStatus = iota
	// LostConflict is the status when rev is greater than the last revision of
	// the chain (the resolution is often to abort the update)
	LostConflict
	// WonConflict is the status when rev is not in the chain,
	// but the last revision of the chain is still (the resolution can be to
	// make the update but including rev in the revisions chain)
	WonConflict
)

// MaxDepth is the maximum number of revisions in a chain that we keep for a
// document.
const MaxDepth = 100

// RevsStruct is a struct for revisions in bulk methods of CouchDB
type RevsStruct struct {
	Start int      `json:"start"`
	IDs   []string `json:"ids"`
}

// RevsTree is a tree of revisions, like CouchDB has.
// The revisions are sorted by growing generation (the number before the hyphen).
// http://docs.couchdb.org/en/stable/replication/conflicts.html#revision-tree
type RevsTree struct {
	// Rev is a revision, with the generation and the id
	// e.g. 1-1bad9a88f0a608ea78c12ab49882ac41
	Rev string `json:"rev"`

	// Branches is the list of revisions that have this revision for parent.
	// The general case is to have only one branch, but we can have more with
	// conflicts.
	Branches []RevsTree `json:"branches,omitempty"`
}

// Clone duplicates the RevsTree
func (rt *RevsTree) Clone() RevsTree {
	cloned := RevsTree{Rev: rt.Rev}
	cloned.Branches = make([]RevsTree, len(rt.Branches))
	for i, b := range rt.Branches {
		cloned.Branches[i] = b.Clone()
	}
	return cloned
}

// Generation returns the maximal generation of a revision in this tree
func (rt *RevsTree) Generation() int {
	if len(rt.Branches) == 0 {
		return revision.Generation(rt.Rev)
	}
	max := 0
	for _, b := range rt.Branches {
		if g := b.Generation(); g > max {
			max = g
		}
	}
	return max
}

// Find returns the sub-tree for the given revision, or nil if not found. It
// also gives the depth of the sub-tree (how many nodes are traversed from the
// root of RevsTree to reach this sub-tree).
func (rt *RevsTree) Find(rev string) (*RevsTree, int) {
	if rt.Rev == rev {
		return rt, 1
	}
	for i := range rt.Branches {
		if sub, depth := rt.Branches[i].Find(rev); sub != nil {
			return sub, depth + 1
		}
	}
	return nil, 0
}

// ensureMaxDepth will remove the first nodes of the branch that contains
// parent, but never parent itself. As the nodes are collapsed in place, the
// callers must look for parent again after this call, as a pointer on a node
// can be stale.
func (rt *RevsTree) ensureMaxDepth(parent string, depth int) {
	current := rt
	for depth > MaxDepth {
		if current.Rev == parent || len(current.Branches) == 0 {
			break
		} else if len(current.Branches) == 1 {
			next := current.Branches[0]
			current.Rev = next.Rev
			current.Branches = next.Branches
			depth--
		} else {
			var found *RevsTree
			for i := range current.Branches {
				b := &current.Branches[i]
				if sub, _ := b.Find(parent); sub != nil {
					found = b
					break
				}
			}
			if found == nil {
				break
			}
			current = found
		}
	}
}

// Add inserts the given revision in the main branch
func (rt *RevsTree) Add(rev string) *RevsTree {
	if rev == rt.Rev {
		return rt
	}

	if revision.Generation(rev) < revision.Generation(rt.Rev) {
		rt.Branches = []RevsTree{
			{Rev: rt.Rev, Branches: rt.Branches},
		}
		rt.Rev = rev
		return rt
	}

	if len(rt.Branches) > 0 {
		// XXX This condition shouldn't be true, but it can help to limit
		// damage in case bugs happen.
		if rt.Branches[0].Rev == rev {
			return &rt.Branches[0]
		}
		return rt.Branches[0].Add(rev)
	}

	rt.Branches = []RevsTree{
		{Rev: rev},
	}
	return &rt.Branches[0]
}

// InsertAfter inserts the given revision in the tree as a child of the second
// revision.
func (rt *RevsTree) InsertAfter(rev, parent string) {
	subtree, depth := rt.Find(parent)
	if subtree == nil {
		// XXX This condition shouldn't be true, but it can help to limit
		// damage in case bugs happen.
		if sub, _ := rt.Find(rev); sub != nil {
			return
		}
		subtree = rt.Add(parent)
	}

	rt.ensureMaxDepth(parent, depth+1)
	if sub, _ := rt.Find(parent); sub != nil {
		subtree = sub
	}

	for _, b := range subtree.Branches {
		if b.Rev == rev {
			return
		}
	}
	subtree.Branches = append(subtree.Branches, RevsTree{Rev: rev})
}

// InsertChain inserts a chain of revisions, ie the first revision is the
// parent of the second revision, which is itself the parent of the third
// revision, etc. The first revisions of the chain are very probably already in
// the tree, the last one is certainly not.
// TODO ensure the MaxDepth limit is respected
func (rt *RevsTree) InsertChain(chain []string) {
	if len(chain) == 0 {
		return
	}
	common := 0
	var subtree *RevsTree
	var depth int
	for i, rev := range chain {
		subtree, depth = rt.Find(rev)
		if subtree != nil {
			depth += len(chain) - i
			common = i
			break
		}
	}
	if subtree == nil {
		subtree = rt.Add(chain[0])
	}

	rt.ensureMaxDepth(subtree.Rev, depth)
	if sub, _ := rt.Find(chain[common]); sub != nil {
		subtree = sub
	}

	for _, rev := range chain[common+1:] {
		if len(subtree.Branches) > 0 {
			found := false
			for i := range subtree.Branches {
				if subtree.Branches[i].Rev == rev {
					found = true
					subtree = &subtree.Branches[i]
					break
				}
			}
			if found {
				continue
			}
		}
		subtree.Branches = append(subtree.Branches, RevsTree{Rev: rev})
		subtree = &subtree.Branches[0]
	}
}

// revsMapToStruct builds a RevsStruct from a json unmarshaled to a map
func revsMapToStruct(revs interface{}) *RevsStruct {
	revisions, ok := revs.(map[string]interface{})
	if !ok {
		return nil
	}
	start, ok := revisions["start"].(float64)
	if !ok {
		return nil
	}
	slice, ok := revisions["ids"].([]interface{})
	if !ok {
		return nil
	}
	ids := make([]string, len(slice))
	for i, id := range slice {
		ids[i], _ = id.(string)
	}
	return &RevsStruct{
		Start: int(start),
		IDs:   ids,
	}
}

// revsChainToStruct transforms revisions from on format to another:
// ["2-aa", "3-bb", "4-cc"] -> { start: 4, ids: ["cc", "bb", "aa"] }
func revsChainToStruct(revs []string) RevsStruct {
	s := RevsStruct{
		IDs: make([]string, len(revs)),
	}
	var last string
	for i, rev := range revs {
		parts := strings.SplitN(rev, "-", 2)
		last = parts[0]
		s.IDs[len(s.IDs)-i-1] = parts[1]
	}
	s.Start, _ = strconv.Atoi(last)
	return s
}

// revsStructToChain is the reverse of revsChainToStruct:
// { start: 4, ids: ["cc", "bb", "aa"] } -> ["2-aa", "3-bb", "4-cc"]
func revsStructToChain(revs RevsStruct) []string {
	start := revs.Start
	ids := revs.IDs
	chain := make([]string, len(ids))
	for i, id := range ids {
		rev := fmt.Sprintf("%d-%s", start, id)
		chain[len(ids)-i-1] = rev
		start--
	}
	return chain
}

// detectConflict says if there is a conflict (ie rev is not in the revisions
// chain), and if it is the case, if the update should be made (WonConflict) or
// aborted (LostConflict)
func detectConflict(rev string, chain []string) conflictStatus {
	if len(chain) == 0 {
		return LostConflict
	}
	for _, r := range chain {
		if r == rev {
			return NoConflict
		}
	}

	last := chain[len(chain)-1]
	genl := revision.Generation(last)
	genr := revision.Generation(rev)
	if genl > genr {
		return WonConflict
	} else if genl < genr {
		return LostConflict
	} else if last > rev {
		return WonConflict
	}
	return LostConflict
}

// MixupChainToResolveConflict creates a new chain of revisions that can be
// used to resolve a conflict: the new chain will start the old rev and include
// other revisions from the chain with a greater generation.
func MixupChainToResolveConflict(rev string, chain []string) []string {
	gen := revision.Generation(rev)
	mixed := make([]string, 0)
	found := false
	for _, r := range chain {
		if found {
			mixed = append(mixed, r)
		} else if gen == revision.Generation(r) {
			mixed = append(mixed, rev)
			found = true
		}
	}
	return mixed
}

// addMissingRevsToChain includes the missing doc revisions to the chain, i.e.
// all the doc revisions between the highest revision saved in the
// revisions tree, and the lowest revision of the chain.
// This can occur when both local and remote updates are made to the same doc,
// with the remote ones saved before the local ones.
func addMissingRevsToChain(db prefixer.Prefixer, ref *SharedRef, chain []string) ([]string, error) {
	refHighestGen := ref.Revisions.Generation()
	chainLowestGen := revision.Generation(chain[0])
	if refHighestGen >= chainLowestGen-1 {
		return chain, nil
	}
	docRef := extractDocReferenceFromID(ref.SID)
	doc := &couchdb.JSONDoc{}
	err := couchdb.GetDocWithRevs(db, docRef.Type, docRef.ID, doc)
	if err != nil {
		return chain, err
	}
	revisions := revsMapToStruct(doc.M["_revisions"])
	if len(revisions.IDs) < chainLowestGen-1 {
		return nil, fmt.Errorf("Cannot add the missing revs to io.cozy.shared %s", docRef.ID)
	}
	var oldRevs []string
	for i := refHighestGen + 1; i < chainLowestGen; i++ {
		revID := revisions.IDs[len(revisions.IDs)-i]
		revGen := strconv.Itoa(i)
		rev := revGen + "-" + revID
		oldRevs = append(oldRevs, rev)
	}
	chain = append(oldRevs, chain...)
	return chain, nil
}

// conflictName generates a new name for a file/folder in conflict with another
// that has the same path. A conflicted file `foo` will be renamed foo (2),
// then foo (3), etc.
func conflictName(indexer vfs.Indexer, dirID, name string, isFile bool) string {
	base, ext := name, ""
	if isFile {
		ext = filepath.Ext(name)
		base = strings.TrimSuffix(base, ext)
	}
	i := 2
	if strings.HasSuffix(base, ")") {
		if idx := strings.LastIndex(base, " ("); idx > 0 {
			num, err := strconv.Atoi(base[idx+2 : len(base)-1])
			if err == nil {
				i = num + 1
				base = base[0:idx]
			}
		}
	}
	for j := 0; j < 1000; j++ {
		newname := fmt.Sprintf("%s (%d)%s", base, i, ext)
		exists, err := indexer.DirChildExists(dirID, newname)
		if err != nil || !exists {
			return newname
		}
		i++
	}
	return fmt.Sprintf("%s (%d)%s", base, i, ext)
}

// conflictID generates a new ID for a file/folder that has a conflict between
// two versions of its content.
func conflictID(id, rev string) string {
	parts := strings.SplitN(rev, "-", 2)
	key := []byte(parts[1])
	for i, c := range key {
		switch {
		case '0' <= c && c <= '9':
			key[i] = c - '0'
		case 'a' <= c && c <= 'f':
			key[i] = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			key[i] = c - 'A' + 10
		}
	}
	return XorID(id, key)
}

// CheckSharedError is the type used when checking the io.cozy.shared, and one
// document has two revisions where a child don't its generation equal to the
// generation of the parent plus one.
type CheckSharedError struct {
	Type   string `json:"type"`
	ID     string `json:"_id"`
	Parent string `json:"parent_rev"`
	Child  string `json:"child_rev"`
}

func (rt *RevsTree) check() *CheckSharedError {
	if len(rt.Branches) == 0 {
		return nil
	}

	gen := revision.Generation(rt.Rev)
	for _, b := range rt.Branches {
		if revision.Generation(b.Rev) != gen+1 {
			return &CheckSharedError{
				Type:   "invalid_revs_suite",
				Parent: rt.Rev,
				Child:  b.Rev,
			}
		}
	}

	for _, b := range rt.Branches {
		if check := b.check(); check != nil {
			return check
		}
	}
	return nil
}
