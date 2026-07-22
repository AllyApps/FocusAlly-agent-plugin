package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

func sampleState() tracker.State {
	var s tracker.State
	s.Apply(tracker.Event{
		Kind: tracker.WorkBegin, AgentKind: "claude",
		SessionID: "sess-1", ProjectPath: "/proj",
		At: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC),
	})
	s.Apply(tracker.Event{
		Kind: tracker.WorkEnd, AgentKind: "claude", SessionID: "sess-1",
		At: time.Date(2026, 7, 22, 10, 5, 0, 500_000_000, time.UTC),
	})
	s.MachineName = "devbox"
	return s
}

// The wire body must be JSON-RPC 2.0 tools/call with the snapshot under
// arguments.agentSession and ISO8601 `.000`-fraction UTC dates — the
// exact shape the backend's MCP dispatcher + SyncJSON decoder expect.
func TestReportWireShape(t *testing.T) {
	var captured []byte
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		authHeader = r.Header.Get("Authorization")
		captured, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}],"isError":false}}`))
	}))
	defer srv.Close()

	if err := Report(srv.URL, "fa_mcp_token", sampleState()); err != nil {
		t.Fatal(err)
	}
	if authHeader != "Bearer fa_mcp_token" {
		t.Fatalf("authorization header = %q", authHeader)
	}

	var body struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Name      string `json:"name"`
			Arguments struct {
				AgentSession map[string]json.RawMessage `json:"agentSession"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	if body.JSONRPC != "2.0" || body.Method != "tools/call" ||
		body.Params.Name != "agent_sessions.report" {
		t.Fatalf("envelope = %s", captured)
	}
	as := body.Params.Arguments.AgentSession
	if string(as["agentKind"]) != `"claude"` ||
		string(as["externalSessionId"]) != `"sess-1"` ||
		string(as["machineName"]) != `"devbox"` ||
		string(as["projectPath"]) != `"/proj"` {
		t.Fatalf("agentSession = %s", captured)
	}
	if string(as["startedAt"]) != `"2026-07-22T10:00:00.000Z"` {
		t.Fatalf("startedAt = %s", as["startedAt"])
	}
	if string(as["lastActivityAt"]) != `"2026-07-22T10:05:00.500Z"` {
		t.Fatalf("lastActivityAt = %s", as["lastActivityAt"])
	}
	if string(as["endedAt"]) != "null" {
		t.Fatalf("endedAt = %s", as["endedAt"])
	}
	var intervals []struct {
		Start string  `json:"start"`
		End   *string `json:"end"`
	}
	if err := json.Unmarshal(as["activeIntervals"], &intervals); err != nil {
		t.Fatal(err)
	}
	if len(intervals) != 1 || intervals[0].Start != "2026-07-22T10:00:00.000Z" ||
		intervals[0].End == nil || *intervals[0].End != "2026-07-22T10:05:00.500Z" {
		t.Fatalf("activeIntervals = %s", as["activeIntervals"])
	}
	// Local bookkeeping must never leak onto the wire.
	if _, leaked := as["dirty"]; leaked {
		t.Fatal("dirty leaked into the wire payload")
	}
}

func TestReport401MapsToErrUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"invalid_token"},"id":null}`))
	}))
	defer srv.Close()

	err := Report(srv.URL, "expired", sampleState())
	if _, ok := err.(ErrUnauthorized); !ok {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// The backend reuses JSON-RPC code -32001 for rate_limited (HTTP 429)
// and insufficient_scope (HTTP 200). Neither is a token problem — they
// must NOT map to ErrUnauthorized (which would trigger a refresh and,
// on failure, could nuke perfectly good credentials).
func TestReportRateLimitIsNotUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"rate_limited"},"id":null}`))
	}))
	defer srv.Close()

	err := Report(srv.URL, "tok", sampleState())
	if err == nil {
		t.Fatal("429 must be an error")
	}
	if _, ok := err.(ErrUnauthorized); ok {
		t.Fatal("429 rate limit must not map to ErrUnauthorized")
	}
}

func TestReportInsufficientScopeIsNotUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"insufficient_scope"}}`))
	}))
	defer srv.Close()

	err := Report(srv.URL, "tok", sampleState())
	if err == nil {
		t.Fatal("insufficient_scope must be an error")
	}
	if _, ok := err.(ErrUnauthorized); ok {
		t.Fatal("insufficient_scope must not map to ErrUnauthorized")
	}
}

// Only a 400/401 OAuth reply proves the refresh grant is dead; 5xx,
// 429, and transport failures must stay retryable.
func TestTokenErrorDefinitiveness(t *testing.T) {
	responses := []struct {
		status     int
		body       string
		definitive bool
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant","error_description":"expired"}`, true},
		{http.StatusUnauthorized, `{"error":"invalid_client","error_description":"unknown"}`, true},
		{http.StatusTooManyRequests, `{"code":"rate_limited","message":"too many requests"}`, false},
		{http.StatusBadGateway, `{"code":"server_error","message":"upstream error"}`, false},
	}
	for _, tc := range responses {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		_, err := Refresh(srv.URL, "client-1", Credentials{RefreshToken: "fa_mcr_x"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d must error", tc.status)
		}
		if got := IsDefinitiveTokenRejection(err); got != tc.definitive {
			t.Fatalf("status %d: definitive = %v, want %v (err %v)", tc.status, got, tc.definitive, err)
		}
	}
	// Transport failure (nothing listening) is never definitive.
	_, err := Refresh("http://127.0.0.1:1", "client-1", Credentials{RefreshToken: "fa_mcr_x"})
	if err == nil || IsDefinitiveTokenRejection(err) {
		t.Fatalf("transport error must be retryable, got %v", err)
	}
}

func TestCredentialsNearExpiry(t *testing.T) {
	c := Credentials{ExpiresAt: 1000}
	if c.NearExpiry(900) {
		t.Fatal("100 s before expiry is not near")
	}
	if !c.NearExpiry(950) || !c.NearExpiry(1000) || !c.NearExpiry(2000) {
		t.Fatal("within 60 s of (or past) expiry must be near")
	}
	if (Credentials{}).NearExpiry(2000) {
		t.Fatal("zero ExpiresAt (unknown) must not force refresh")
	}
}

func TestReportToolErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid_params: missing required field 'startedAt'"}}`))
	}))
	defer srv.Close()

	if err := Report(srv.URL, "tok", sampleState()); err == nil {
		t.Fatal("JSON-RPC error must surface as an error")
	}
}
