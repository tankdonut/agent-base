// client.go hosts the RPC client: the v4 handshake (challenge →
// connect → hello-ok) and id-correlated request/response over the
// single socket. The protocol version is pinned — a server outside
// the pinned range fails closed instead of best-effort framing.
package gatewayclient

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the pinned gateway protocol this client speaks.
// hello-ok must negotiate exactly this version.
const ProtocolVersion = 4

// DefaultRequestTimeout mirrors the reference client's per-RPC budget.
const DefaultRequestTimeout = 30 * time.Second

// frame is the union envelope; exactly one shape arrives per frame.
type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
}

// RPCError is the protocol's error member on failed responses.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("gateway: %s: %s", e.Code, e.Message)
}

// Options tunes Connect.
type Options struct {
	// ClientVersion reports the CLI version in the handshake
	// (observability server-side; defaults to "dev").
	ClientVersion string
	// RequestTimeout overrides the per-RPC budget.
	RequestTimeout time.Duration
}

// Client is one connected gateway RPC session. Safe for concurrent
// Call use; events are ignored (status polling only — subscriptions
// are deliberately out of scope until a live-watch consumer exists).
type Client struct {
	ws      *wsConn
	token   string
	timeout time.Duration

	serverVersion string
	scopes        []string

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan frame
	closed  bool
}

// Connect dials, completes the v4 handshake, and returns a ready
// client. The token authenticates the operator role (shared gateway
// token — trusted operator access per docs/gateway/operator-scopes).
func Connect(ctx context.Context, rawURL, token string, opts Options) (*Client, error) {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "dev"
	}
	ws, err := dialWS(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	c := &Client{ws: ws, token: token, timeout: opts.RequestTimeout, pending: map[string]chan frame{}}

	// 1. The gateway opens with connect.challenge; its nonce is bound
	// into the device signature below.
	helloDeadline := time.Now().Add(15 * time.Second)
	ws.setDeadline(helloDeadline)
	first, err := ws.readMessage()
	if err != nil {
		ws.close()
		return nil, fmt.Errorf("reading connect challenge: %w", err)
	}
	var challenge frame
	if err := json.Unmarshal(first, &challenge); err != nil || challenge.Type != "event" || challenge.Event != "connect.challenge" {
		ws.close()
		return nil, fmt.Errorf("first frame was not connect.challenge (got %.80s)", first)
	}
	nonce := challengeStringField(challenge.Payload, "nonce")

	// 2. connect request: protocol pinned to 4 on both ends, with
	// DEVICE identity. A published-port connection arrives on the
	// container's network interface — remote locality — and remote
	// device-less connects get their scopes cleared (MISSING_SCOPE on
	// every call), so the client signs the challenge: Ed25519 over the
	// v3 payload built from the connect params (buildDeviceAuthPayloadV3
	// in the gateway's device-auth module).
	scopes := []string{"operator.read", "operator.approvals"}
	signedAtMs := time.Now().UnixMilli()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		ws.close()
		return nil, fmt.Errorf("device keypair: %w", err)
	}
	deviceID := fmt.Sprintf("%x", sha256.Sum256(pub))
	platform := strings.ToLower(strings.TrimSpace("linux"))
	devicePayload := strings.Join([]string{
		"v3",
		deviceID,
		"cli",
		"cli",
		"operator",
		strings.Join(scopes, ","),
		strconv.FormatInt(signedAtMs, 10),
		token,
		nonce,
		platform,
		"",
	}, "|")
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(devicePayload)))
	connect := map[string]any{
		"type":   "req",
		"id":     "connect",
		"method": "connect",
		"params": map[string]any{
			"minProtocol": ProtocolVersion,
			"maxProtocol": ProtocolVersion,
			"client": map[string]any{
				"id":       "cli",
				"version":  opts.ClientVersion,
				"platform": platform,
				"mode":     "cli",
			},
			"role":      "operator",
			"scopes":    scopes,
			"caps":      []string{},
			"commands":  []string{},
			"auth":      map[string]any{"token": token},
			"locale":    "en-US",
			"userAgent": "agentctl/" + opts.ClientVersion,
			"device": map[string]any{
				"id":        deviceID,
				"publicKey": base64.RawURLEncoding.EncodeToString(pub),
				"signature": sig,
				"signedAt":  signedAtMs,
				"nonce":     nonce,
			},
		},
	}
	body, err := json.Marshal(connect)
	if err != nil {
		ws.close()
		return nil, err
	}
	if err := ws.writeText(body); err != nil {
		ws.close()
		return nil, fmt.Errorf("sending connect: %w", err)
	}

	// 3. hello-ok response (id "connect").
	res, err := ws.readMessage()
	if err != nil {
		ws.close()
		return nil, fmt.Errorf("reading hello-ok: %w", err)
	}
	var hello frame
	if err := json.Unmarshal(res, &hello); err != nil {
		ws.close()
		return nil, fmt.Errorf("malformed hello-ok: %w", err)
	}
	if hello.ID != "connect" || hello.Type != "res" {
		ws.close()
		return nil, fmt.Errorf("expected the connect response, got %.80s", res)
	}
	if hello.Error != nil {
		ws.close()
		return nil, fmt.Errorf("gateway rejected the connect: %w", hello.Error)
	}
	if hello.OK == nil || !*hello.OK {
		ws.close()
		return nil, fmt.Errorf("gateway connect response was not ok")
	}
	var payload struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
		Server   struct {
			Version string `json:"version"`
			ConnID  string `json:"connId"`
		} `json:"server"`
		Auth struct {
			Role   string   `json:"role"`
			Scopes []string `json:"scopes"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(hello.Payload, &payload); err != nil || payload.Type != "hello-ok" {
		ws.close()
		return nil, fmt.Errorf("hello-ok payload malformed: %w", err)
	}
	if payload.Protocol != ProtocolVersion {
		ws.close()
		return nil, fmt.Errorf("gateway negotiated protocol %d, this client pins %d — upgrade agentctl", payload.Protocol, ProtocolVersion)
	}
	c.serverVersion = payload.Server.Version
	c.scopes = payload.Auth.Scopes

	ws.setDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

// challengeStringField extracts a top-level string field from the
// connect.challenge payload.
func challengeStringField(payload json.RawMessage, key string) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// ServerVersion reports the hello-ok server version.
func (c *Client) ServerVersion() string { return c.serverVersion }

// Scopes reports the negotiated operator scopes.
func (c *Client) Scopes() []string { return c.scopes }

// Close sends a websocket close and tears down; safe to call twice.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.ws.writeClose()
	c.ws.close()
}

// Call sends one allowed RPC and decodes the payload into result.
// The method must be in the package allowlist — this client is not a
// general RPC surface.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if !methodAllowed(method) {
		return fmt.Errorf("method %q is outside the gatewayclient allowlist", method)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client is closed")
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := map[string]any{"type": "req", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(c.timeout)
	}
	c.ws.setDeadline(deadline)
	if err := c.ws.writeText(body); err != nil {
		return fmt.Errorf("sending %s: %w", method, err)
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return fmt.Errorf("connection closed while awaiting %s", method)
		}
		if f.Error != nil {
			return f.Error
		}
		if f.OK == nil || !*f.OK {
			return fmt.Errorf("gateway %s response was not ok", method)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(f.Payload, result); err != nil {
			return fmt.Errorf("decoding %s payload: %w", method, err)
		}
		return nil
	case <-time.After(time.Until(deadline)):
		return fmt.Errorf("%s timed out", method)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readLoop dispatches responses to their pending call until the
// connection dies; events are dropped (status client).
func (c *Client) readLoop() {
	for {
		msg, err := c.ws.readMessage()
		if err != nil {
			c.failAllPending()
			return
		}
		var f frame
		if err := json.Unmarshal(msg, &f); err != nil {
			continue // tolerate a non-JSON push by ignoring it
		}
		if f.Type != "res" || f.ID == "" {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[f.ID]
		c.mu.Unlock()
		if ok {
			ch <- f
		}
	}
}

func (c *Client) failAllPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}
