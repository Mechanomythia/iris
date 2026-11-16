// Package peer identifies the process on the other end of a Unix socket and
// walks its ancestry. Used to (a) show the real requesting PID in the popup
// and (b) refuse control-socket connections from processes descended from
// Claude Code, as defence in depth on top of the sandbox socket allowlist.
package peer

import (
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// PID returns the kernel-verified PID of the peer, or 0 if unavailable.
func PID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) { pid = peerPID(int(fd)) })
	return pid
}

// Info describes a process.
type Info struct {
	PID  int
	PPID int
	Comm string // executable path or name
	Args string
}

// Lookup uses ps(1), present on macOS and Linux, to avoid cgo.
func Lookup(pid int) (Info, bool) {
	if pid <= 0 {
		return Info{}, false
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return Info{}, false
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 2 {
		return Info{}, false
	}
	ppid, _ := strconv.Atoi(f[0])
	args, _ := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return Info{PID: pid, PPID: ppid, Comm: strings.Join(f[1:], " "), Args: strings.TrimSpace(string(args))}, true
}

// Ancestry returns pid and its ancestors (closest first), max 64 deep.
func Ancestry(pid int) []Info {
	var out []Info
	for i := 0; i < 64 && pid > 1; i++ {
		inf, ok := Lookup(pid)
		if !ok {
			break
		}
		out = append(out, inf)
		pid = inf.PPID
	}
	return out
}

// IsClaudeDescendant reports whether any ancestor looks like Claude Code.
func IsClaudeDescendant(pid int) bool {
	for _, a := range Ancestry(pid) {
		if looksLikeClaude(a) {
			return true
		}
	}
	return false
}

func looksLikeClaude(a Info) bool {
	base := filepath.Base(a.Comm)
	if base == "claude" || strings.HasPrefix(base, "claude-") {
		return true
	}
	// npm installs run as `node .../@anthropic-ai/claude-code/cli.js`.
	return strings.Contains(a.Args, "@anthropic-ai/claude-code") || strings.Contains(a.Args, "/claude-code/cli")
}
