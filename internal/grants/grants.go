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
	ID      string
	Tool    string
	Target  string
	Scope   string
	Profile string
	Option  string
	Once    bool
	Session bool
	// Command binds the grant to one Bash command (Claude Code's tool use
	// id): it covers every request that command makes, until it ends.
	Command string
	// Reads: covers only reads (HTTP GET/HEAD, or a command iris classified
	// as read-only); writes still ask.
	Reads     bool
	GrantedAt time.Time
	ExpiresAt time.Time
	Uses      int
}

type Store struct {
	mu      sync.Mutex
	g       map[string]*Grant // key tool/target@profile
	now     func() time.Time
	history map[string][]time.Time // approvals per key, for the "allowed N times" hint
}

func New() *Store {
	return &Store{g: map[string]*Grant{}, now: time.Now, history: map[string][]time.Time{}}
}

// CommandMaxTTL caps a command-bound grant when the command's end is never
// reported (no PostToolUse hook).
const CommandMaxTTL = 10 * time.Minute

// AddForCommand records a grant that covers every request of one command
// until EndCommand(cmd) or CommandMaxTTL.
func (s *Store) AddForCommand(tool, target, profile, scope, option, cmd string) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	g := &Grant{ID: NewID(), Tool: tool, Target: target, Profile: profile, Scope: scope, Option: option,
		Command: cmd, GrantedAt: now, ExpiresAt: now.Add(CommandMaxTTL)}
	k := key(tool, target, profile)
	s.g[k] = g
	s.note(k, now)
	return g
}

// EndCommand drops the grants bound to a finished command.
func (s *Store) EndCommand(cmd string) int {
	if cmd == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, g := range s.g {
		if g.Command == cmd {
			delete(s.g, k)
			n++
		}
	}
	return n
}

func (s *Store) note(k string, t time.Time) {
	h := append(s.history[k], t)
	cut := 0
	for i, x := range h {
		if t.Sub(x) > time.Hour {
			cut = i + 1
		}
	}
	s.history[k] = h[cut:]
}

// Recent counts approvals for (tool, target, profile) within window.
func (s *Store) Recent(tool, target, profile string, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, t := range s.history[key(tool, target, profile)] {
		if now.Sub(t) <= window {
			n++
		}
	}
	return n
}

// SetClock is for tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

// Grants are per Claude profile, so approving kubectl for "claude-work" does
// not silently cover "claude-personal".
func key(tool, target, profile string) string { return tool + "/" + target + "@" + profile }

func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add records a grant and returns it. A once-grant is consumed by the very
// next Use (the request that caused the prompt).
func (s *Store) Add(tool, target, profile, scope, option string, ttl time.Duration, once, session bool) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	g := &Grant{ID: NewID(), Tool: tool, Target: target, Profile: profile, Scope: scope, Option: option,
		Once: once, Session: session, GrantedAt: now, ExpiresAt: now.Add(ttl)}
	if once {
		// A once grant only needs to live long enough for the request that created it.
		g.ExpiresAt = now.Add(time.Minute)
	}
	s.g[key(tool, target, profile)] = g
	s.note(key(tool, target, profile), now)
	return g
}

// Use returns a live grant for (tool,target) and counts the use. Once-grants
// are removed on use.
func (s *Store) Use(tool, target, profile string) (*Grant, bool) {
	return s.UseFor(tool, target, profile, false)
}

// UseFor returns a live grant covering this use: a full grant, or for a read
// also a reads grant. Once-grants are removed on use.
func (s *Store) UseFor(tool, target, profile string, read bool) (*Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := []string{key(tool, target, profile)}
	if read {
		keys = append(keys, readsKey(tool, target, profile))
	}
	for _, k := range keys {
		g, ok := s.g[k]
		if !ok {
			continue
		}
		if !s.now().Before(g.ExpiresAt) {
			delete(s.g, k)
			continue
		}
		g.Uses++
		cp := *g
		if g.Once {
			delete(s.g, k)
		}
		return &cp, true
	}
	return nil, false
}

// AddReads records a grant for reads only, next to any full grant.
func (s *Store) AddReads(tool, target, profile, scope, option string, ttl time.Duration, session bool) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	g := &Grant{ID: NewID(), Tool: tool, Target: target, Profile: profile, Scope: scope, Option: option,
		Session: session, Reads: true, GrantedAt: now, ExpiresAt: now.Add(ttl)}
	s.g[readsKey(tool, target, profile)] = g
	s.note(key(tool, target, profile), now)
	return g
}

func readsKey(tool, target, profile string) string { return key(tool, target, profile) + "#reads" }

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

// RevokeSession drops "until session end" grants of one profile ("" = all).
func (s *Store) RevokeSession(profile string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, g := range s.g {
		if g.Session && (profile == "" || g.Profile == profile) {
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
		out = append(out, protocol.GrantInfo{ID: g.ID, Tool: g.Tool, Target: g.Target, Scope: g.Scope, Profile: g.Profile,
			Option: g.Option, Reads: g.Reads, GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt, Uses: g.Uses})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}
