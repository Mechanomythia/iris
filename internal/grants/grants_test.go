package grants

import (
	"testing"
	"time"
)

func TestOnceIsConsumed(t *testing.T) {
	s := New()
	s.Add("kube", "staging", "", "read", "once", 0, true, false)
	if _, ok := s.Use("kube", "staging", ""); !ok {
		t.Fatal("first use should succeed")
	}
	if _, ok := s.Use("kube", "staging", ""); ok {
		t.Fatal("once grant reused")
	}
}

func TestTTLExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := New()
	s.SetClock(func() time.Time { return now })
	s.Add("kube", "staging", "", "read", "15m", 15*time.Minute, false, false)
	for i := 0; i < 3; i++ {
		if _, ok := s.Use("kube", "staging", ""); !ok {
			t.Fatal("should be valid within TTL")
		}
	}
	now = now.Add(15 * time.Minute)
	if _, ok := s.Use("kube", "staging", ""); ok {
		t.Fatal("expired grant used")
	}
	if len(s.List()) != 0 {
		t.Fatal("expired grant listed")
	}
}

func TestRevoke(t *testing.T) {
	s := New()
	g := s.Add("aws", "dev", "", "", "1h", time.Hour, false, false)
	s.Add("gcp", "p", "", "", "session", time.Hour, false, true)
	s.Add("git", "github.com", "", "", "1h", time.Hour, false, false)
	if n := s.Revoke(g.ID); n != 1 {
		t.Fatalf("revoke by id = %d", n)
	}
	if n := s.RevokeSession(""); n != 1 {
		t.Fatalf("revoke session = %d", n)
	}
	if n := s.RevokeAll(); n != 1 {
		t.Fatalf("revoke all = %d", n)
	}
}

func TestGrantsArePerProfile(t *testing.T) {
	s := New()
	s.Add("kube", "staging", "claude-work", "read", "1h", time.Hour, false, true)
	if _, ok := s.Use("kube", "staging", "claude-personal"); ok {
		t.Fatal("grant leaked across profiles")
	}
	if _, ok := s.Use("kube", "staging", "claude-work"); !ok {
		t.Fatal("grant missing for its own profile")
	}
	s.Add("kube", "staging", "claude-personal", "read", "session", time.Hour, false, true)
	if n := s.RevokeSession("claude-work"); n != 1 {
		t.Fatalf("session revoke should only hit claude-work, got %d", n)
	}
	if _, ok := s.Use("kube", "staging", "claude-personal"); !ok {
		t.Fatal("other profile's session grant was revoked")
	}
}
