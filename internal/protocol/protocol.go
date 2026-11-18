// Package protocol defines the JSON-lines messages spoken over the two Unix sockets.
//
// broker.sock (reachable from the sandbox):  credential, context, revoke-session, ping
// control.sock (never reachable from the sandbox): status, grants, revoke, audit,
//
//	subscribe (UI), decision, reload
package protocol

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
)

// Request is used on both sockets; Op selects the meaning.
type Request struct {
	Op string `json:"op"`

	// credential
	Tool   string `json:"tool,omitempty"`
	Target string `json:"target,omitempty"`
	// Self-reported by the helper; the broker overrides PID with the
	// kernel-verified peer PID when available.
	PID  int      `json:"pid,omitempty"`
	PPID int      `json:"ppid,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	Argv []string `json:"argv,omitempty"`

	// Profile names the Claude config dir (IRIS_PROFILE, set per settings
	// file by iris install). Self-reported: a label, not a security boundary.
	Profile string `json:"profile,omitempty"`

	// context (PreToolUse hook)
	Command   string `json:"command,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	ToolUseID string `json:"toolUseId,omitempty"` // context / context-end

	// ssh-host (SSH ProxyCommand, before connecting)
	Host string `json:"host,omitempty"`
	Port string `json:"port,omitempty"`

	// control
	ID     string `json:"id,omitempty"`     // revoke / decision
	All    bool   `json:"all,omitempty"`    // revoke
	Choice string `json:"choice,omitempty"` // decision: deny | once | 15m | ...
	N      int    `json:"n,omitempty"`      // audit lines
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	Credential *Credential       `json:"credential,omitempty"`
	Grants     []GrantInfo       `json:"grants,omitempty"`
	Status     *Status           `json:"status,omitempty"`
	Audit      []json.RawMessage `json:"audit,omitempty"`
	Revoked    int               `json:"revoked,omitempty"`
	Detail     string            `json:"detail,omitempty"` // ssh-host: known | synced | unknown
}

// Credential is what the helper turns into tool-specific output.
type Credential struct {
	Kind      string    `json:"kind"` // kube | aws | gcp | git | docker
	Token     string    `json:"token,omitempty"`
	Username  string    `json:"username,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	// LongLived: the secret itself does not expire (static token, client
	// cert, PAT); revoking the grant stops iris handing it out, not its use.
	LongLived bool              `json:"longLived,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"` // aws keys, kube client cert, ...
}

type GrantInfo struct {
	ID        string    `json:"id"`
	Tool      string    `json:"tool"`
	Target    string    `json:"target"`
	Scope     string    `json:"scope"`
	Profile   string    `json:"profile,omitempty"`
	Option    string    `json:"option"`
	GrantedAt time.Time `json:"grantedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Uses      int       `json:"uses"`
}

type Status struct {
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"startedAt"`
	UIConnected   bool      `json:"uiConnected"`
	Identities    []string  `json:"identities"`
	ProxyListen   string    `json:"proxyListen,omitempty"`
	PendingPrompt int       `json:"pendingPrompts"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// Event is pushed from the daemon to subscribed UIs on control.sock.
type Event struct {
	Type string `json:"type"` // approval_request | cancel | grants_changed

	ID          string `json:"id,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Target      string `json:"target,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Label       string `json:"label,omitempty"`
	Passthrough bool   `json:"passthrough,omitempty"` // uses the user's own credentials
	Profile     string `json:"profile,omitempty"`
	Command     string `json:"command,omitempty"`
	// CommandSpans: Command highlighted and line-broken for display.
	CommandSpans []CommandSpan `json:"commandSpans,omitempty"`
	ProcessPID   int           `json:"pid,omitempty"`
	ProcessCwd   string        `json:"cwd,omitempty"`
	ProcessArgv  []string      `json:"argv,omitempty"`
	Options      []OptionInfo  `json:"options,omitempty"`
	TouchID      bool          `json:"touchID,omitempty"`
	Deadline     time.Time     `json:"deadline,omitempty"`
	Recent       int           `json:"recent,omitempty"` // approved this N times in the last 10 min
}

type CommandSpan struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

type OptionInfo struct {
	Name    string `json:"name"`    // value to send back as Choice
	Display string `json:"display"` // button title
}

// Conn wraps a net.Conn with JSON-lines encode/decode.
type Conn struct {
	net.Conn
	r *bufio.Reader
	e *json.Encoder
}

func Wrap(c net.Conn) *Conn {
	return &Conn{Conn: c, r: bufio.NewReaderSize(c, 64<<10), e: json.NewEncoder(c)}
}

func (c *Conn) Send(v any) error { return c.e.Encode(v) }

func (c *Conn) Recv(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, v)
}

// Call dials a socket, sends one request and reads one response.
func Call(socket string, req Request, timeout time.Duration) (*Response, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if timeout > 0 {
		_ = c.SetDeadline(time.Now().Add(timeout))
	}
	pc := Wrap(c)
	if err := pc.Send(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := pc.Recv(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
