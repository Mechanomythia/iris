// Package approval asks the human. Preferred path: the Iris menu bar app,
// subscribed on control.sock. Fallback when no app is connected: an
// osascript dialog (macOS), limited to three buttons.
package approval

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/protocol"
)

type Request struct {
	ID       string
	Identity config.Identity
	Profile  string // Claude config dir label
	Command  string // from PreToolUse hook, advisory only
	PID      int
	Cwd      string
	Argv     []string
	Options  []config.Option
	TouchID  bool
	Deadline time.Time
}

type Decision struct {
	Option   config.Option
	Denied   bool
	TimedOut bool
	Via      string // ui | dialog | none
}

// Subscriber is a connected UI.
type Subscriber struct {
	ch chan protocol.Event
}

func (s *Subscriber) Events() <-chan protocol.Event { return s.ch }

type Hub struct {
	mu      sync.Mutex
	subs    map[*Subscriber]struct{}
	pending map[string]chan string // id -> choice
	// Fallback is used when no UI is subscribed. Replaceable in tests.
	Fallback func(ctx context.Context, r Request) (string, error)
}

func NewHub() *Hub {
	return &Hub{subs: map[*Subscriber]struct{}{}, pending: map[string]chan string{}, Fallback: dialogFallback}
}

func (h *Hub) Subscribe() *Subscriber {
	s := &Subscriber{ch: make(chan protocol.Event, 32)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
	h.mu.Unlock()
}

func (h *Hub) UIConnected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

func (h *Hub) Pending() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pending)
}

// Broadcast sends a non-request event (grants_changed) to all UIs.
func (h *Hub) Broadcast(ev protocol.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default: // slow UI: drop, it will refresh on next event
		}
	}
}

// Decide delivers a UI decision. Returns false if the id is unknown/expired.
func (h *Hub) Decide(id, choice string) bool {
	h.mu.Lock()
	ch, ok := h.pending[id]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- choice:
		return true
	default:
		return false
	}
}

// Ask blocks until the human answers or the deadline passes (= deny).
func (h *Hub) Ask(ctx context.Context, r Request) Decision {
	ctx, cancel := context.WithDeadline(ctx, r.Deadline)
	defer cancel()

	if !h.UIConnected() {
		choice, err := h.Fallback(ctx, r)
		if err != nil || choice == "" {
			return Decision{Denied: true, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Via: "dialog"}
		}
		return resolve(r, choice, "dialog")
	}

	ch := make(chan string, 1)
	h.mu.Lock()
	h.pending[r.ID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, r.ID)
		h.mu.Unlock()
		h.Broadcast(protocol.Event{Type: "cancel", ID: r.ID})
	}()

	h.Broadcast(Event(r))
	select {
	case choice := <-ch:
		return resolve(r, choice, "ui")
	case <-ctx.Done():
		return Decision{Denied: true, TimedOut: true, Via: "ui"}
	}
}

func resolve(r Request, choice, via string) Decision {
	if choice == "deny" {
		return Decision{Denied: true, Via: via}
	}
	for _, o := range r.Options {
		if o.Name == choice {
			return Decision{Option: o, Via: via}
		}
	}
	// Unknown choice: never fail open.
	return Decision{Denied: true, Via: via}
}

func Event(r Request) protocol.Event {
	ev := protocol.Event{
		Type: "approval_request", ID: r.ID,
		Tool: r.Identity.Tool, Target: r.Identity.Target, Scope: r.Identity.Scope, Label: r.Identity.Label(), Profile: r.Profile,
		Command: r.Command, ProcessPID: r.PID, ProcessCwd: r.Cwd, ProcessArgv: r.Argv,
		TouchID: r.TouchID, Deadline: r.Deadline,
	}
	for _, o := range r.Options {
		ev.Options = append(ev.Options, protocol.OptionInfo{Name: o.Name, Display: o.Display()})
	}
	return ev
}

// Message is the human-readable prompt body (also used by the dialog fallback).
func Message(r Request) string {
	var b strings.Builder
	who := "Claude"
	if r.Profile != "" {
		who += " (" + r.Profile + ")"
	}
	fmt.Fprintf(&b, "%s wants to use: %s\n", who, r.Identity.Label())
	if r.Command != "" {
		fmt.Fprintf(&b, "\nCommand:\n%s\n", truncate(r.Command, 300))
	}
	if len(r.Argv) > 0 {
		fmt.Fprintf(&b, "\nProcess: %s", truncate(strings.Join(r.Argv, " "), 200))
	}
	if r.PID > 0 {
		fmt.Fprintf(&b, " (pid %d)", r.PID)
	}
	if r.Cwd != "" {
		fmt.Fprintf(&b, "\nIn: %s", r.Cwd)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const dialogScript = `on run argv
	set msg to item 1 of argv
	set secs to (item 2 of argv) as integer
	set btns to {}
	repeat with i from 3 to count of argv
		set end of btns to item i of argv
	end repeat
	try
		set r to display dialog msg with title "Iris · credential request" buttons btns default button 1 cancel button 1 giving up after secs with icon caution
		if gave up of r then return ""
		return button returned of r
	on error
		return "Deny"
	end try
end run`

// dialogFallback shows Deny + the two shortest options (AppleScript allows 3 buttons).
func dialogFallback(ctx context.Context, r Request) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("no Iris app connected and no dialog fallback on " + runtime.GOOS)
	}
	secs := int(time.Until(r.Deadline).Seconds())
	if secs < 1 {
		return "", context.DeadlineExceeded
	}
	args := []string{"-e", dialogScript, Message(r) + "\n\n(Install/open Iris.app for all options and Touch ID.)", fmt.Sprint(secs), "Deny"}
	byDisplay := map[string]string{}
	for i, o := range r.Options {
		if i == 2 {
			break
		}
		args = append(args, o.Display())
		byDisplay[o.Display()] = o.Name
	}
	out, err := exec.CommandContext(ctx, "osascript", args...).Output()
	if err != nil {
		return "", err
	}
	got := strings.TrimSpace(string(out))
	if got == "" || got == "Deny" {
		return "deny", nil
	}
	if name, ok := byDisplay[got]; ok {
		return name, nil
	}
	return "deny", nil
}
