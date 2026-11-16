// Package grants is the in-memory grant table. Grants never touch disk:
// restarting the daemon revokes everything, which is the safe direction.
package grants

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/mechanomythia/iris/internal/protocol"
)

type Grant struct {
	ID        string
	Tool      string
	Target    string
	Scope     string
	Option    string
	Once      bool
	Session   bool
	GrantedAt time.Time
	ExpiresAt time.Time
	Uses      int
}

type Store struct {
	mu  sync.Mutex
	g   map[string]*Grant // key tool/target
	now func() time.Time
}

func New() *Store { return &Store{g: map[string]*Grant{}, now: time.Now} }

// SetClock is for tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func key(tool, target string) string { return tool + "/" + target }

func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add records a grant and returns it. A once-grant is consumed by the very
// next Use (the request that caused the prompt).
func (s *Store) Add(tool, target, scope, option string, ttl time.Duration, once, session bool) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	g := &Grant{ID: NewID(), Tool: tool, Target: target, Scope: scope, Option: option,
		Once: once, Session: session, GrantedAt: now, ExpiresAt: now.Add(ttl)}
	if once {
		// A once grant only needs to live long enough for the request that created it.
		g.ExpiresAt = now.Add(time.Minute)
	}
	s.g[key(tool, target)] = g
	return g
}

// Use returns a live grant for (tool,target) and counts the use. Once-grants
// are removed on use.
func (s *Store) Use(tool, target string) (*Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tool, target)
	g, ok := s.g[k]
	if !ok {
		return nil, false
	}
	if !s.now().Before(g.ExpiresAt) {
		delete(s.g, k)
		return nil, false
	}
	g.Uses++
	cp := *g
	if g.Once {
		delete(s.g, k)
	}
	return &cp, true
}

// Revoke by id; returns number removed.
func (s *Store) Revoke(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, g := range s.g {
		if g.ID == id {
			delete(s.g, k)
			return 1
		}
	}
	return 0
}

func (s *Store) RevokeAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.g)
	s.g = map[string]*Grant{}
	return n
}

func (s *Store) RevokeSession() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, g := range s.g {
		if g.Session {
			delete(s.g, k)
			n++
		}
	}
	return n
}

// List returns live grants, pruning expired ones.
func (s *Store) List() []protocol.GrantInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := []protocol.GrantInfo{}
	for k, g := range s.g {
		if !now.Before(g.ExpiresAt) {
			delete(s.g, k)
			continue
		}
		out = append(out, protocol.GrantInfo{ID: g.ID, Tool: g.Tool, Target: g.Target, Scope: g.Scope,
			Option: g.Option, GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt, Uses: g.Uses})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}
