package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MCP Streamable HTTP transport (protocol revisions 2025-03-26 / 2025-06-18).
//
// A single endpoint serves JSON-RPC over HTTP POST. The server may assign a
// session id (returned in the Mcp-Session-Id header on the initialize
// response); clients echo it on every subsequent request and may terminate it
// with DELETE. Server-initiated messages are not used by pty-mcp, so GET (the
// optional server->client SSE stream) replies 405, which the spec permits.

const (
	mcpSessionHeader = "Mcp-Session-Id"
	// mcpProtocolHeader is accepted but not strictly enforced, for
	// compatibility with clients that omit or send it.
	mcpProtocolHeader = "MCP-Protocol-Version"

	httpSessionTTL   = 30 * time.Minute
	httpMaxBodyBytes = 10 << 20 // 10MB, matches the stdio scanner limit
)

// ServeHTTP runs the MCP Streamable HTTP transport on addr and blocks until
// the listener fails. When token is non-empty, every request must carry
// "Authorization: Bearer <token>".
func ServeHTTP(h *Handler, addr, token string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", newHTTPTransport(h, token))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found (MCP endpoint is /mcp)", http.StatusNotFound)
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("[mcp] Streamable HTTP listening on %s (endpoint /mcp)", addr)
	return srv.ListenAndServe()
}

type httpTransport struct {
	handler  *Handler
	token    string
	inflight *inflightRegistry
	sem      chan struct{}

	mu       sync.Mutex
	sessions map[string]time.Time
}

func newHTTPTransport(h *Handler, token string) *httpTransport {
	return &httpTransport{
		handler:  h,
		token:    token,
		inflight: newInflightRegistry(),
		sem:      make(chan struct{}, maxConcurrentToolCalls),
		sessions: make(map[string]time.Time),
	}
}

func (t *httpTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		t.handlePost(w, r)
	case http.MethodDelete:
		t.handleDelete(w, r)
	default:
		// GET (server->client SSE) is not offered; DELETE and POST are.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (t *httpTransport) authorized(r *http.Request) bool {
	if t.token == "" {
		return true
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(t.token)) == 1
}

func (t *httpTransport) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpMaxBodyBytes))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	batch := trimmed[0] == '['
	var raws []json.RawMessage
	if batch {
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			writeJSONRPCError(w, -32700, "parse error")
			return
		}
	} else {
		raws = []json.RawMessage{trimmed}
	}

	sid := r.Header.Get(mcpSessionHeader)
	if sid != "" && !t.sessionValid(sid) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	responses := make([]response, 0, len(raws))
	sawInitialize := false
	for _, raw := range raws {
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			responses = append(responses, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.Method == "initialize" {
			sawInitialize = true
		}
		resp, ok := t.dispatch(r.Context(), sid, &req)
		if ok {
			responses = append(responses, resp)
		}
	}

	// Assign a session id on a successful initialize request. Requests without
	// a session id are still served (stateless mode); only an *unknown* id is
	// rejected, per spec.
	if sid == "" && sawInitialize {
		sid = t.newSession()
		w.Header().Set(mcpSessionHeader, sid)
	}

	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if wantsSSE(r.Header.Get("Accept")) {
		writeSSE(w, responses)
		return
	}
	writeJSON(w, responses, batch)
}

// dispatch runs one JSON-RPC message, mirroring the stdio Serve loop's
// cancellation and concurrency semantics. The second return value is false
// when no response should be sent (notifications, or a cancelled request).
func (t *httpTransport) dispatch(parent context.Context, scope string, req *request) (response, bool) {
	key := scope + "\x00" + string(req.ID)

	if req.Method == "notifications/cancelled" || req.Method == "$/cancelRequest" {
		t.cancelScoped(scope, req.Params)
		return response{}, false
	}

	isNotification := len(req.ID) == 0
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var entry *inflightEntry
	if !isNotification {
		entry = t.inflight.add(key, cancel)
	}

	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		if !isNotification {
			t.inflight.finish(key, entry)
		}
		return response{}, false
	}
	defer func() { <-t.sem }()

	resp := handle(ctx, t.handler, req)
	if isNotification {
		return response{}, false
	}
	if t.inflight.finish(key, entry) {
		return response{}, false // cancelled: spec says do not respond
	}
	return resp, true
}

func (t *httpTransport) cancelScoped(scope string, raw json.RawMessage) {
	var p cancelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	id := p.RequestID
	if len(id) == 0 {
		id = p.ID
	}
	if len(id) == 0 || string(id) == "null" {
		return
	}
	t.inflight.cancel(scope + "\x00" + string(id))
}

func (t *httpTransport) handleDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get(mcpSessionHeader)
	if sid == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	t.mu.Lock()
	_, ok := t.sessions[sid]
	delete(t.sessions, sid)
	t.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (t *httpTransport) newSession() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck
	sid := hex.EncodeToString(b[:])
	t.mu.Lock()
	// Opportunistically evict expired sessions so the map cannot grow forever.
	for id, created := range t.sessions {
		if time.Since(created) > httpSessionTTL {
			delete(t.sessions, id)
		}
	}
	t.sessions[sid] = time.Now()
	t.mu.Unlock()
	return sid
}

func (t *httpTransport) sessionValid(sid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	created, ok := t.sessions[sid]
	if !ok {
		return false
	}
	if time.Since(created) > httpSessionTTL {
		delete(t.sessions, sid)
		return false
	}
	return true
}

// wantsSSE reports whether the client asked for SSE but not JSON. When both
// are acceptable (the spec requires clients to list both) JSON is preferred.
func wantsSSE(accept string) bool {
	return strings.Contains(accept, "text/event-stream") &&
		!strings.Contains(accept, "application/json")
}

func writeJSON(w http.ResponseWriter, responses []response, batch bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if batch {
		enc.Encode(responses) //nolint:errcheck
		return
	}
	enc.Encode(responses[0]) //nolint:errcheck
}

func writeSSE(w http.ResponseWriter, responses []response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, resp := range responses {
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeJSONRPCError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.Encode(response{JSONRPC: "2.0", Error: &rpcError{Code: code, Message: msg}}) //nolint:errcheck
}
