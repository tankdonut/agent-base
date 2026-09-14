package gatewayclient

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeGateway is a scripted WebSocket SERVER speaking the pinned v4
// frames, recording every client frame for contract assertions. It
// implements just enough RFC 6455: unmasked writes, reads and unmasks
// client frames, answers nothing on its own.
type fakeGateway struct {
	t       *testing.T
	ln      net.Listener
	token   string
	invalid bool // negotiate a wrong protocol deliberately

	mu       sync.Mutex
	frames   []string
	requests map[string]string // request id → method
	active   net.Conn          // the current client connection (scripted writes)

	// script hooks; nil means a canned default response.
	onRequest func(method, id string) (payload any, rpcErr *RPCError)
}

func newFakeGateway(t *testing.T, token string) *fakeGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{t: t, ln: ln, token: token, requests: map[string]string{}}
	go g.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return g
}

func (g *fakeGateway) url() string { return "ws://" + g.ln.Addr().String() + "/" }

func (g *fakeGateway) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.frames...)
}

func (g *fakeGateway) serve() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.handle(conn)
	}
}

func (g *fakeGateway) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	accept := sha1.New()
	_, _ = io.WriteString(accept, key+wsGUID)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept.Sum(nil)) +
		"\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return
	}
	g.mu.Lock()
	g.active = conn
	g.mu.Unlock()

	g.sendText(conn, `{"type":"event","event":"connect.challenge","payload":{"nonce":"n","ts":1}}`)

	for {
		msg, err := readServerFrame(br)
		if err != nil {
			return
		}
		g.record(msg)
		var f frame
		if err := json.Unmarshal([]byte(msg), &f); err != nil {
			continue
		}
		switch f.Method {
		case "connect":
			g.mu.Lock()
			g.requests["connect"] = f.Method
			g.mu.Unlock()
			protocol := 4
			if g.invalid {
				protocol = 3
			}
			g.sendText(conn, fmt.Sprintf(
				`{"type":"res","id":"connect","ok":true,"payload":{"type":"hello-ok","protocol":%d,"server":{"version":"test-1","connId":"c1"},"features":{"methods":[],"events":[]},"snapshot":{},"auth":{"role":"operator","scopes":["operator.read","operator.approvals"]},"policy":{"maxPayload":1000,"maxBufferedBytes":2000,"tickIntervalMs":15000}}}`,
				protocol))
		default:
			g.mu.Lock()
			g.requests[f.ID] = f.Method
			g.mu.Unlock()
			payload, rpcErr := `{}`, (*RPCError)(nil)
			if g.onRequest != nil {
				p, e := g.onRequest(f.Method, f.ID)
				if e != nil {
					rpcErr = e
				} else if p != nil {
					b, _ := json.Marshal(p)
					payload = string(b)
				}
			}
			res := fmt.Sprintf(`{"type":"res","id":%q,"ok":%t,"payload":%s}`,
				f.ID, rpcErr == nil, payload)
			if rpcErr != nil {
				b, _ := json.Marshal(rpcErr)
				res = fmt.Sprintf(`{"type":"res","id":%q,"ok":false,"error":%s}`,
					f.ID, b)
			}
			g.sendText(conn, res)
		}
	}
}

func (g *fakeGateway) record(msg string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frames = append(g.frames, msg)
}

func (g *fakeGateway) sendText(conn net.Conn, msg string) {
	payload := []byte(msg)
	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var ext [8]byte
		for i := 7; i >= 0; i-- {
			ext[i] = byte(n)
			n >>= 8
		}
		header = append(header, ext[:]...)
	}
	if _, err := conn.Write(append(header, payload...)); err != nil {
		g.t.Logf("fake gateway write failed: %v", err)
	}
}

// sendFragmented pushes one response as two fragments — the client
// must reassemble.
func (g *fakeGateway) sendFragmented(conn net.Conn, msg string) {
	half := len(msg) / 2
	g.sendFrag(conn, false, msg[:half])
	g.sendFrag(conn, true, msg[half:])
}

func (g *fakeGateway) sendFrag(conn net.Conn, fin bool, part string) {
	// Two-fragment text message: first = TEXT without FIN (0x01),
	// final = CONTINUATION with FIN (0x80).
	b := byte(0x80)
	if !fin {
		b = 0x01
	}
	header := []byte{b}
	n := len(part)
	header = append(header, byte(n))
	if _, err := conn.Write(append(header, []byte(part)...)); err != nil {
		g.t.Logf("fragmented write failed: %v", err)
	}
}

// readServerFrame reads ONE client frame: unmask + return the text.
func readServerFrame(br *bufio.Reader) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return "", err
	}
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := int(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return "", err
		}
		length = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return "", err
		}
		for _, b := range ext {
			length = length<<8 | int(b)
		}
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return "", err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return "", err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if opcode != opText {
		return "", fmt.Errorf("unexpected opcode %d", opcode)
	}
	return string(payload), nil
}

// --- contract tests ---

func TestHandshakeSendsPinnedConnect(t *testing.T) {
	g := newFakeGateway(t, "sk-token-canary")
	c, err := Connect(t.Context(), g.url(), "sk-token-canary", Options{ClientVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	frames := g.recorded()
	if len(frames) == 0 {
		t.Fatal("no client frames recorded")
	}
	var connect map[string]any
	if err := json.Unmarshal([]byte(frames[0]), &connect); err != nil {
		t.Fatalf("first client frame is not JSON: %s", frames[0])
	}
	params := connect["params"].(map[string]any)
	if params["minProtocol"].(float64) != 4 || params["maxProtocol"].(float64) != 4 {
		t.Errorf("protocol not pinned to 4: %v", params)
	}
	if params["role"] != "operator" {
		t.Errorf("role = %v, want operator", params["role"])
	}
	client := params["client"].(map[string]any)
	if client["id"] != "cli" || client["mode"] != "cli" {
		t.Errorf("client identity %v — must be the first-party local-CLI shape or scopes get cleared", client)
	}
	auth := params["auth"].(map[string]any)
	if auth["token"] != "sk-token-canary" {
		t.Errorf("auth token missing from connect")
	}
	if c.ServerVersion() != "test-1" {
		t.Errorf("server version = %q", c.ServerVersion())
	}
}

func TestProtocolMismatchFailsClosed(t *testing.T) {
	g := newFakeGateway(t, "tok")
	g.invalid = true
	if _, err := Connect(t.Context(), g.url(), "tok", Options{}); err == nil ||
		!strings.Contains(err.Error(), "pins 4") {
		t.Fatalf("protocol 3 must fail closed, got %v", err)
	}
}

func TestCallCorrelationAndAllowlist(t *testing.T) {
	g := newFakeGateway(t, "tok")
	g.onRequest = func(method, id string) (any, *RPCError) {
		if method == "sessions.list" {
			return map[string]any{"sessions": []any{map[string]any{"k": 1}, map[string]any{"k": 2}}}, nil
		}
		return nil, &RPCError{Code: "NOT_FOUND", Message: "nope"}
	}
	c, err := Connect(t.Context(), g.url(), "tok", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var res struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := c.Call(t.Context(), "sessions.list", map[string]any{}, &res); err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(res.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(res.Sessions))
	}

	if err := c.Call(t.Context(), "config.get", map[string]any{"path": "gateway"}, nil); err == nil ||
		!strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("config.get must be refused client-side, got %v", err)
	}

	err = c.Call(t.Context(), "agent", map[string]any{}, nil)
	if err == nil || !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Fatalf("rpc error must surface, got %v", err)
	}
}

func TestResolveApprovalFrameShape(t *testing.T) {
	g := newFakeGateway(t, "tok")
	seen := map[string]string{}
	g.onRequest = func(method, id string) (any, *RPCError) {
		seen[method] = id
		return map[string]any{"ok": true}, nil
	}
	c, err := Connect(t.Context(), g.url(), "tok", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.ResolveApproval(t.Context(), "exec", "ap_1", DecisionApprove); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := c.ResolveApproval(t.Context(), "plugin", "pl_9", DecisionDeny); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := c.ResolveApproval(t.Context(), "exec", "ap_2", "maybe"); err == nil {
		t.Fatal("invalid decision must be refused")
	}
	// The recorded frames pin the exact wire shape (map keys marshal
	// alphabetically).
	joined := strings.Join(g.recorded(), "\n")
	for _, want := range []string{
		`"method":"exec.approval.resolve"`,
		`"decision":"approve","id":"ap_1"`,
		`"method":"plugin.approval.resolve"`,
		`"decision":"deny","id":"pl_9"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("wire frame missing %s:\n%s", want, joined)
		}
	}
}

func TestListApprovalsNormalizesWrappers(t *testing.T) {
	g := newFakeGateway(t, "tok")
	g.onRequest = func(method, id string) (any, *RPCError) {
		switch method {
		case "exec.approval.list":
			return map[string]any{"approvals": []any{map[string]any{"id": "e1", "command": "rm -rf /tmp/x"}}}, nil
		case "plugin.approval.list":
			return map[string]any{"items": []any{map[string]any{"id": "p1", "summary": "install skill"}}}, nil
		}
		return nil, nil
	}
	c, err := Connect(t.Context(), g.url(), "tok", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got, err := c.ListApprovals(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "e1" || got[0].Kind != "exec" || got[1].Kind != "plugin" {
		t.Errorf("approvals = %+v", got)
	}
}

func TestFragmentedResponseReassembles(t *testing.T) {
	g := newFakeGateway(t, "tok")
	payload := `{"sessions":[{"key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"key":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`
	c, err := Connect(t.Context(), g.url(), "tok", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// The scripted handler pushes the response as two fragments; the
	// default writer then adds a harmless error frame that lands after
	// Call has already returned.
	conn := g.currentConn()
	g.onRequest = func(method, id string) (any, *RPCError) {
		g.sendFragmented(conn, fmt.Sprintf(`{"type":"res","id":%q,"ok":true,"payload":%s}`, id, payload))
		return nil, &RPCError{}
	}
	var raw struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := c.Call(t.Context(), "sessions.list", map[string]any{}, &raw); err != nil {
		t.Fatalf("fragmented call: %v", err)
	}
	if len(raw.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2 (fragment reassembly)", len(raw.Sessions))
	}
}

// currentConn exposes the active server connection for scripted
// fragmented writes.
func (g *fakeGateway) currentConn() net.Conn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}
