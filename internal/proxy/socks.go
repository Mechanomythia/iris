package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/config"
)

// ServeSOCKS serves SOCKS5 (RFC 1928, no auth, CONNECT only) for non-HTTP
// traffic from the sandbox: git over SSH on port 22, databases, anything
// that isn't an HTTP client. Every destination goes through decide.
func (p *Proxy) ServeSOCKS(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go p.handleSOCKS(ctx, c)
	}
}

const (
	socksOK          = 0x00
	socksNotAllowed  = 0x02
	socksHostUnreach = 0x04
	socksCmdUnsupp   = 0x07
	socksAddrUnsupp  = 0x08
)

func (p *Proxy) handleSOCKS(ctx context.Context, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	host, port, err := socksHandshake(c)
	if err != nil {
		return
	}
	// The dialog may take a while: no deadline while deciding.
	_ = c.SetDeadline(time.Time{})
	reply := func(code byte) { _, _ = c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	target := net.JoinHostPort(host, port)
	logc := func(detail string) {
		p.B.Audit.Write(audit.Entry{Event: "connect", Tool: config.ToolNetwork, Target: target, Detail: detail})
	}
	if err := p.decide(ctx, host, port, "connect "+target); err != nil {
		logc("blocked: " + err.Error())
		reply(socksNotAllowed)
		return
	}
	up, err := p.DialUpstream(ctx, "tcp", target)
	if err != nil {
		logc("unreachable: " + err.Error())
		reply(socksHostUnreach)
		return
	}
	logc("connected")
	defer up.Close()
	reply(socksOK)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// socksHandshake reads the greeting and CONNECT request; returns host, port.
func socksHandshake(c net.Conn) (string, string, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return "", "", errors.New("not socks5")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", "", err
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{5, 0xff})
		return "", "", errors.New("no acceptable auth")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", "", err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 {
		return "", "", errors.New("bad request")
	}
	fail := func(code byte) { _, _ = c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	if req[1] != 1 { // CONNECT
		fail(socksCmdUnsupp)
		return "", "", errors.New("only CONNECT")
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = net.IP(b).String()
	case 3:
		n := make([]byte, 1)
		if _, err := io.ReadFull(c, n); err != nil {
			return "", "", err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = string(b)
	default:
		fail(socksAddrUnsupp)
		return "", "", fmt.Errorf("address type %d", req[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", "", err
	}
	return host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))), nil
}

// DialSOCKS connects to addr through a SOCKS5 server (used by iris-helper).
func DialSOCKS(server, host, port string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", server, 10*time.Second)
	if err != nil {
		return nil, err
	}
	pn, err := strconv.Atoi(port)
	if err != nil || len(host) > 255 {
		c.Close()
		return nil, errors.New("bad host or port")
	}
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil || resp[1] != 0 {
		c.Close()
		return nil, errors.New("socks: no-auth refused")
	}
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(pn))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		return nil, err
	}
	switch rep[1] {
	case socksOK:
		return c, nil
	case socksNotAllowed:
		c.Close()
		return nil, fmt.Errorf("iris denied network access to %s:%s", host, port)
	default:
		c.Close()
		return nil, fmt.Errorf("socks: cannot reach %s:%s (code %d)", host, port, rep[1])
	}
}
