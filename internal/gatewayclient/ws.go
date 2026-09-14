// Package gatewayclient is a minimal, deliberately hand-rolled
// WebSocket client for the OpenClaw Gateway RPC protocol (v4). The
// repo carries no websocket dependency on purpose: the client needs
// only text frames, client-side masking, ping/pong, and close — a
// fixed, small slice of RFC 6455 that stays auditable and stdlib-only.
//
// Frame contract (docs/gateway/protocol.md, pinned image):
//
//	Request:  {type:"req", id, method, params}
//	Response: {type:"res", id, ok, payload|error}
//	Event:    {type:"event", event, payload, seq?, stateVersion?}
//
// Handshake: the gateway sends connect.challenge, the client answers
// a `connect` request (protocol 4, role operator, shared token), and
// the gateway replies hello-ok with the negotiated scopes.
package gatewayclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wsGUID is the RFC 6455 handshake magic.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn is a client-mode text WebSocket connection.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialWS performs the HTTP upgrade handshake and returns a client
// connection speaking text frames. Server: header tolerance follows
// RFC 6455 (one exact value, no list parsing).
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", rawURL, err)
	}
	switch u.Scheme {
	case "ws":
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), "80")
		}
	case "wss":
		return nil, fmt.Errorf("wss:// is not supported yet — agentctl talks to loopback gateways over plain ws")
	default:
		return nil, fmt.Errorf("unsupported scheme %q (want ws://)", u.Scheme)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", u.Host, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("generating websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := strings.Join([]string{
		"GET " + path + " HTTP/1.1",
		"Host: " + u.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + key,
		"Sec-WebSocket-Version: 13",
		"User-Agent: agentctl",
		"\r\n",
	}, "\r\n")
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sending upgrade: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading upgrade response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("gateway refused the websocket upgrade (HTTP %d)", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		_ = conn.Close()
		return nil, fmt.Errorf("gateway handshake malformed (missing websocket accept)")
	}
	// From here on the deadline belongs to frame I/O, set per call.
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsClose* are the RFC opcodes this client handles.
const (
	opContinuation = 0x0
	opText         = 0x1
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// writeText sends one masked text frame (client→server frames MUST be
// masked).
func (w *wsConn) writeText(payload []byte) error {
	return w.writeFrame(opText, payload)
}

// writeClose sends a normal-close frame; best effort by design.
func (w *wsConn) writeClose() {
	_ = w.writeFrame(opClose, []byte{})
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode) // FIN + opcode
	maskKey := make([]byte, 4)
	if _, err := rand.Read(maskKey); err != nil {
		return fmt.Errorf("generating mask: %w", err)
	}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		header = append(header, ext[:]...)
	default:
		header = append(header, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	header = append(header, maskKey...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}
	if _, err := w.conn.Write(append(header, masked...)); err != nil {
		return fmt.Errorf("writing frame: %w", err)
	}
	return nil
}

// readMessage reads one complete text message: control frames are
// handled inline (ping → pong), fragments are reassembled. Returns
// io.EOF on close.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opPing:
			if err := w.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return nil, io.EOF
		case opText, opContinuation, 0x2: // binary tolerated, not expected
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("unexpected websocket opcode 0x%x", opcode)
		}
	}
}

// readFrame reads one frame header + payload. Server→client frames
// must be unmasked per RFC 6455 §5.1 — a masked server frame is a
// protocol violation and fails closed.
func (w *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(w.br, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(w.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(w.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 1<<22 { // 4 MiB cap: hello + method payloads are far below
		err = fmt.Errorf("websocket frame too large (%d bytes)", length)
		return
	}
	if masked {
		err = fmt.Errorf("server sent a masked frame (RFC 6455 violation)")
		return
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return
	}
	return
}

// setDeadline arms frame I/O for one RPC round trip.
func (w *wsConn) setDeadline(t time.Time) { _ = w.conn.SetDeadline(t) }

// close tears the TCP connection down.
func (w *wsConn) close() { _ = w.conn.Close() }
