package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/schaepher/pty-mcp/internal/session"
)

// newHTTPTestServer starts an httptest server wrapping the Streamable HTTP
// transport with a real (empty) session manager.
func newHTTPTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	h := NewHandler(session.NewManager(60), nil)
	srv := httptest.NewServer(newHTTPTransport(h, token))
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestHTTPInitializeReturnsSessionAndJSON(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, body := doReq(t, http.MethodPost, srv.URL, initBody,
		map[string]string{"Accept": "application/json, text/event-stream"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if sid := resp.Header.Get(mcpSessionHeader); sid == "" {
		t.Fatal("expected Mcp-Session-Id header on initialize response")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, body)
	}
	res, _ := out["result"].(map[string]any)
	if res == nil {
		t.Fatalf("missing result: %s", body)
	}
	if got, _ := res["protocolVersion"].(string); got == "" {
		t.Fatalf("missing protocolVersion: %s", body)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if name, _ := info["name"].(string); name != "pty-mcp" {
		t.Fatalf("serverInfo.name = %v, want pty-mcp", info["name"])
	}
}

func TestHTTPToolsList(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, body := doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"tools"`) || !strings.Contains(body, "send_input") {
		t.Fatalf("tools/list body unexpected: %s", body)
	}
}

func TestHTTPNotificationAccepted(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, body := doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		map[string]string{"Accept": "application/json"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(body) != "" {
		t.Fatalf("expected empty body for notification, got %q", body)
	}
}

func TestHTTPRequiresBearerToken(t *testing.T) {
	srv := newHTTPTestServer(t, "s3cret")

	resp, _ := doReq(t, http.MethodPost, srv.URL, initBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodPost, srv.URL, initBody,
		map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", resp.StatusCode)
	}
	resp, body := doReq(t, http.MethodPost, srv.URL, initBody,
		map[string]string{"Authorization": "Bearer s3cret", "Accept": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct token: status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestHTTPGetNotAllowed(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, _ := doReq(t, http.MethodGet, srv.URL, "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Fatalf("Allow header = %q, want includes POST", allow)
	}
}

func TestHTTPSessionLifecycle(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, _ := doReq(t, http.MethodPost, srv.URL, initBody,
		map[string]string{"Accept": "application/json"})
	sid := resp.Header.Get(mcpSessionHeader)
	if sid == "" {
		t.Fatal("no session id issued")
	}

	// Unknown session id must be rejected.
	resp, _ = doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		map[string]string{mcpSessionHeader: "does-not-exist"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: status = %d, want 404", resp.StatusCode)
	}

	// Valid session id is accepted.
	resp, body := doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		map[string]string{mcpSessionHeader: sid, "Accept": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid session: status = %d, body = %s", resp.StatusCode, body)
	}

	// DELETE terminates the session.
	resp, _ = doReq(t, http.MethodDelete, srv.URL, "",
		map[string]string{mcpSessionHeader: sid})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		map[string]string{mcpSessionHeader: sid})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after DELETE: status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPSSEResponse(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, body := doReq(t, http.MethodPost, srv.URL,
		`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		map[string]string{"Accept": "text/event-stream"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(body, "event: message") || !strings.Contains(body, "data: {") {
		t.Fatalf("SSE body unexpected: %s", body)
	}
}

func TestHTTPBatch(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	batch := `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`
	resp, body := doReq(t, http.MethodPost, srv.URL, batch,
		map[string]string{"Accept": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("expected JSON array, got %q: %v", body, err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 responses, got %d: %s", len(out), body)
	}
}

func TestHTTPMalformedJSON(t *testing.T) {
	srv := newHTTPTestServer(t, "")
	resp, body := doReq(t, http.MethodPost, srv.URL, `{not json`,
		map[string]string{"Accept": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "-32700") {
		t.Fatalf("expected parse error -32700, got %s", body)
	}
}
