package recents

import (
	"errors"
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
)

// SourceState is what a traversal knows of the source instance of rows.
type SourceState int

const (
	// SourceLive is a source instance on this stack.
	SourceLive SourceState = iota
	// SourceMoved is a source instance that has moved to another instance of
	// this stack, its successor. A move keeps the file IDs, so the rows of the
	// source are migrated to the successor.
	SourceMoved
	// SourceGone is a source instance that is no longer on this stack and has
	// no successor on it: its rows can never be shown again.
	SourceGone
	// SourceUnavailable is a source instance that could not be checked: its
	// rows are hidden, and may come back.
	SourceUnavailable
)

// Source is the resolved source instance of rows.
type Source struct {
	State SourceState
	// Instance holds the files of the rows: the source itself when it is
	// live, its successor when it has moved.
	Instance *instance.Instance
}

// maxMoves bounds the chain of moves followed from a source.
const maxMoves = 3

// errUnknownMove is the error of a moved source whose target is unknown.
var errUnknownMove = errors.New("the target of the move is unknown")

// errMoveLoop is the error of a source whose chain of moves loops or is
// longer than maxMoves.
var errMoveLoop = errors.New("the chain of moves does not end")

// getInstancesByIDs loads the source instances. It is a variable for the
// tests, to simulate a failure of CouchDB.
var getInstancesByIDs = instance.GetByIDs

// Sources resolves the source instances of the rows of a traversal by the
// instance ID in their keys, and memoizes them for this traversal. The domain
// of an instance never changes: a move, a change of domain included, creates a
// new instance with a new ID, and the old one is marked as moved until it is
// deleted.
type Sources struct {
	user  *instance.Instance
	known map[string]*Source
}

// NewSources returns the resolver of the sources of the rows of a user. The
// rows of the user's own files are always live, even on an instance that has
// moved: they are never migrated.
func NewSources(user *instance.Instance) *Sources {
	return &Sources{
		user: user,
		known: map[string]*Source{
			user.DocID: {State: SourceLive, Instance: user},
		},
	}
}

// Resolve resolves the sources of the rows that are not known yet. Their
// instances are loaded in one request; the moved and deleted ones are followed
// to their successor, one source at a time.
func (s *Sources) Resolve(rows []FileDirActivity) {
	domains := make(map[string]string)
	var ids []string
	for i := range rows {
		instID, _, ok := ParseResourceKey(rows[i].DocID)
		if !ok {
			continue
		}
		if _, known := s.known[instID]; known {
			continue
		}
		if _, seen := domains[instID]; seen {
			continue
		}
		domains[instID], _, _ = ParseSourceLocator(rows[i].SourceLocator)
		ids = append(ids, instID)
	}
	if len(ids) == 0 {
		return
	}

	insts, err := getInstancesByIDs(ids)
	if err != nil {
		s.user.Logger().WithNamespace("recents").
			Warnf("Cannot load the source instances: %s", err)
		for _, id := range ids {
			s.known[id] = &Source{State: SourceUnavailable}
		}
		return
	}
	for _, id := range ids {
		inst := insts[id]
		if inst != nil && !inst.Moved {
			s.known[id] = &Source{State: SourceLive, Instance: inst}
			continue
		}
		s.known[id] = s.successor(id, inst, domains[id])
	}
}

// Get returns the source of the rows of an instance ID, as resolved by
// Resolve.
func (s *Sources) Get(instID string) *Source {
	if src, ok := s.known[instID]; ok {
		return src
	}
	return &Source{State: SourceUnavailable}
}

// successor finds the instance of this stack that holds the files of a source
// that has moved (inst) or that is deleted (inst is nil, known by the domain
// of its rows).
func (s *Sources) successor(id string, inst *instance.Instance, domain string) *Source {
	next, err := followMoves(id, inst, domain)
	switch {
	case err == nil:
		return &Source{State: SourceMoved, Instance: next}
	case errors.Is(err, instance.ErrNotFound):
		return &Source{State: SourceGone}
	case errors.Is(err, errUnknownMove), errors.Is(err, errMoveLoop):
		return &Source{State: SourceUnavailable}
	default:
		s.user.Logger().WithNamespace("recents").
			Warnf("Cannot find the successor of the source instance %s: %s", id, err)
		return &Source{State: SourceUnavailable}
	}
}

// followMoves follows the chain of moves of a source that has moved (inst)
// or that is deleted (inst is nil, known by its domain) up to the instance of
// this stack that holds its files, which has not moved itself.
// instance.ErrNotFound means that the source has no successor on this stack:
// a target that is the instance itself, when a move to another stack keeps
// the domain, is not on this stack either, as no other instance can have its
// domain. errUnknownMove and errMoveLoop mean that a successor may exist.
func followMoves(id string, inst *instance.Instance, domain string) (*instance.Instance, error) {
	visited := map[string]bool{id: true}
	for range maxMoves {
		next, err := nextInstance(inst, domain)
		if err != nil {
			return nil, err
		}
		if inst != nil && next.DocID == inst.DocID {
			return nil, instance.ErrNotFound
		}
		if visited[next.DocID] {
			return nil, errMoveLoop
		}
		if !next.Moved {
			return next, nil
		}
		visited[next.DocID] = true
		inst, domain = next, next.Domain
	}
	return nil, errMoveLoop
}

// nextInstance returns the instance of this stack that a source has moved to:
// the target of the move, written in the settings of a moved source, or else
// the instance that declares the domain of the source as its old domain. An
// instance that only has that domain as an alias, or as its own domain, is not
// a successor: it may belong to another person. instance.ErrNotFound means
// that the source has no successor on this stack.
func nextInstance(inst *instance.Instance, domain string) (*instance.Instance, error) {
	if inst == nil {
		if domain == "" {
			return nil, instance.ErrNotFound
		}
		return findByOldDomain(domain)
	}

	host, err := movedToHost(inst)
	if err != nil {
		return nil, err
	}
	if host == "" {
		// The move may have a target on this stack that cannot be told: the
		// source is not gone.
		next, err := findByOldDomain(inst.Domain)
		if errors.Is(err, instance.ErrNotFound) {
			return nil, errUnknownMove
		}
		return next, err
	}
	next, err := instance.Get(host)
	if errors.Is(err, instance.ErrNotFound) {
		// The target is on another stack, unless an instance of this stack
		// declares the domain of the source as its old domain.
		return findByOldDomain(inst.Domain)
	}
	return next, err
}

// movedToHost returns the host of the target of the move of an instance, or
// an empty string when it is unknown.
func movedToHost(inst *instance.Instance) (string, error) {
	doc, err := inst.SettingsDocument()
	if err != nil {
		return "", err
	}
	to, _ := doc.M["moved_to"].(string)
	if to == "" {
		return "", nil
	}
	u, err := url.Parse(to)
	if err != nil || u.Host == "" {
		u, err = url.Parse("https://" + to)
		if err != nil {
			return "", nil
		}
	}
	return u.Host, nil
}

func findByOldDomain(domain string) (*instance.Instance, error) {
	next, err := instance.FindByOldDomain(domain)
	if err != nil {
		return nil, err
	}
	if err := next.MakeVFS(); err != nil {
		return nil, err
	}
	return next, nil
}
