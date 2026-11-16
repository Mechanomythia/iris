// Package audit appends one JSON line per issue / deny / revoke.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Entry struct {
	Time    time.Time `json:"time"`
	Event   string    `json:"event"` // issue | deny | timeout | grant | revoke | error | proxy-inject
	Tool    string    `json:"tool,omitempty"`
	Target  string    `json:"target,omitempty"`
	Scope   string    `json:"scope,omitempty"`
	Profile string    `json:"profile,omitempty"`
	GrantID string    `json:"grant,omitempty"`
	Option  string    `json:"option,omitempty"`
	PID     int       `json:"pid,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Command string    `json:"command,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

type Log struct {
	mu   sync.Mutex
	path string
}

func Open(path string) *Log { return &Log{path: path} }

func (l *Log) Write(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Tail returns the last n lines (raw JSON).
func (l *Log) Tail(n int) []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ring []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		ring = append(ring, json.RawMessage(append([]byte(nil), sc.Bytes()...)))
		if n > 0 && len(ring) > n {
			ring = ring[1:]
		}
	}
	return ring
}
