package mcpproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/withally/focusally-agent-plugin/internal/api"
)

// fakeBackend fakes POST /mcp and POST /oauth/token. Tokens with the
// prefix "bad" are rejected with 401.
type fakeBackend struct {
	srv           *httptest.Server
	mcpBodies     [][]byte
	mcpAuth       []string
	mu            sync.Mutex
	refreshCalls  atomic.Int64
	nextAccess    string
	refreshStatus int
	// toolsListJSON, when set, is the raw JSON of result.tools for
	// tools/list requests.
	toolsListJSON string
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{nextAccess: "fresh-token", refreshStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		fb.mu.Lock()
		fb.mcpBodies = append(fb.mcpBodies, body)
		fb.mcpAuth = append(fb.mcpAuth, auth)
		fb.mu.Unlock()
		if strings.HasPrefix(auth, "Bearer bad") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		switch env.Method {
		case "tools/list":
			tools := fb.toolsListJSON
			if tools == "" {
				tools = `[{"name":"tasks.create","inputSchema":{"type":"object"}}]`
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`, env.ID, tools)
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"FocusAlly","version":"9"}}}`, env.ID)
		case "rate/limited":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32001,"message":"rate limited"}}`, env.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"echo":%s,"weird":{"future":[1,2,3]}}}`, env.ID, string(body))
		}
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fb.refreshCalls.Add(1)
		if fb.refreshStatus != http.StatusOK {
			w.WriteHeader(fb.refreshStatus)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt2","token_type":"Bearer","expires_in":3600}`, fb.nextAccess)
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBackend) lastAuth() string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.mcpAuth) == 0 {
		return ""
	}
	return fb.mcpAuth[len(fb.mcpAuth)-1]
}

func setupProfile(t *testing.T, fb *fakeBackend, accessToken string) string {
	t.Helper()
	dir := t.TempDir()
	if err := api.SaveConfig(dir, api.Config{BaseURL: fb.srv.URL, ClientID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if accessToken != "" {
		err := api.SaveCredentials(dir, fb.srv.URL, api.Credentials{
			AccessToken: accessToken, RefreshToken: "rt1", ExpiresAt: 9999999999,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// drive runs Serve over the given input lines and returns the output
// messages in order.
func drive(t *testing.T, dir string, fb *fakeBackend, lines ...string) []map[string]any {
	t.Helper()
	raw := driveRaw(t, dir, fb, lines...)
	var out []map[string]any
	for _, line := range raw {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("non-JSON output line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func driveRaw(t *testing.T, dir string, fb *fakeBackend, lines ...string) [][]byte {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{Profile: "default", ConfigDir: dir, Client: fb.srv.Client()})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var out [][]byte
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out
}

func TestPassthroughByteIdentical(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks.create","arguments":{"task":{"x":1}},"futureField":{"a":[true,null]}}}`
	list := `{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{}}`
	out := driveRaw(t, dir, fb, call, list)

	if len(fb.mcpBodies) != 2 {
		t.Fatalf("backend saw %d requests, want 2", len(fb.mcpBodies))
	}
	if string(fb.mcpBodies[0]) != call {
		t.Errorf("tools/call body altered:\n got %s\nwant %s", fb.mcpBodies[0], call)
	}
	if string(fb.mcpBodies[1]) != list {
		t.Errorf("tools/list body altered: %s", fb.mcpBodies[1])
	}
	if got := fb.lastAuth(); got != "Bearer good-token" {
		t.Errorf("auth header = %q", got)
	}
	if len(out) != 2 {
		t.Fatalf("got %d replies, want 2", len(out))
	}
	// Replies relay verbatim (modulo compaction, which the fake's
	// replies already satisfy).
	if !bytes.Contains(out[0], []byte(`"futureField":{"a":[true,null]}`)) {
		t.Errorf("echoed params lost fidelity: %s", out[0])
	}
	if !bytes.Contains(out[0], []byte(`"id":7`)) || !bytes.Contains(out[1], []byte(`"id":8`)) {
		t.Errorf("ids answered out of order: %s / %s", out[0], out[1])
	}
}

func TestNotificationsDroppedAndPingLocal(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	out := drive(t, dir, fb,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	)
	if len(fb.mcpBodies) != 0 {
		t.Errorf("notifications/ping must not reach the backend, saw %d", len(fb.mcpBodies))
	}
	if len(out) != 1 {
		t.Fatalf("got %d replies, want 1 (ping)", len(out))
	}
	if res, ok := out[0]["result"].(map[string]any); !ok || len(res) != 0 {
		t.Errorf("ping reply = %v, want empty result", out[0])
	}
}

func TestRefreshOn401ThenRetry(t *testing.T) {
	fb := newFakeBackend(t)
	fb.nextAccess = "fresh-token"
	dir := setupProfile(t, fb, "bad-expired")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)

	if got := fb.refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls = %d, want 1", got)
	}
	if got := fb.lastAuth(); got != "Bearer fresh-token" {
		t.Errorf("retry auth = %q, want fresh token", got)
	}
	if len(out) != 1 || out[0]["result"] == nil {
		t.Fatalf("expected relayed result after refresh, got %v", out)
	}
	creds, ok := api.LoadCredentials(dir)
	if !ok || creds.AccessToken != "fresh-token" {
		t.Errorf("rotated credentials not persisted: %+v ok=%v", creds, ok)
	}
}

func TestDefinitiveRefreshRejectionDropsCredentials(t *testing.T) {
	fb := newFakeBackend(t)
	fb.refreshStatus = http.StatusBadRequest
	dir := setupProfile(t, fb, "bad-dead")

	out := drive(t, dir, fb,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tasks.list"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
	)

	if _, ok := api.LoadCredentials(dir); ok {
		t.Error("credentials must be deleted on definitive rejection")
	}
	replies, notifications := splitNotifications(out)
	if len(notifications) != 1 || notifications[0]["method"] != "notifications/tools/list_changed" {
		t.Errorf("paired→unpaired must emit exactly one list_changed, got %v", notifications)
	}
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}
	res, ok := replies[0]["result"].(map[string]any)
	if !ok || res["isError"] != true {
		t.Errorf("tools/call unpaired reply must be an isError result, got %v", replies[0])
	}
	if !strings.Contains(fmt.Sprint(res["content"]), "auth.login") {
		t.Errorf("unpaired text must point at auth.login: %v", res)
	}
	errObj, ok := replies[1]["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32002) {
		t.Errorf("non-tool unpaired reply must be error -32002, got %v", replies[1])
	}
}

// splitNotifications separates request replies from server-initiated
// notifications (messages without an id).
func splitNotifications(out []map[string]any) (replies, notifications []map[string]any) {
	for _, m := range out {
		if _, hasID := m["id"]; hasID {
			replies = append(replies, m)
		} else {
			notifications = append(notifications, m)
		}
	}
	return replies, notifications
}

func TestRPCErrorMinus32001PassesThroughWithoutRefresh(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":6,"method":"rate/limited"}`)

	if got := fb.refreshCalls.Load(); got != 0 {
		t.Errorf("-32001 must not trigger refresh, calls = %d", got)
	}
	errObj, ok := out[0]["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32001) {
		t.Errorf("-32001 not relayed verbatim: %v", out[0])
	}
}

func TestInitializePatchesListChanged(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)

	res := out[0]["result"].(map[string]any)
	caps := res["capabilities"].(map[string]any)
	tools := caps["tools"].(map[string]any)
	if tools["listChanged"] != true {
		t.Errorf("listChanged not patched: %v", out[0])
	}
	if info := res["serverInfo"].(map[string]any); info["version"] != "9" {
		t.Errorf("server result replaced instead of relayed: %v", res)
	}
}

func TestInitializeWithExpiredTokenRefreshesToServerResult(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "bad-expired")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)

	res := out[0]["result"].(map[string]any)
	info, ok := res["serverInfo"].(map[string]any)
	if !ok || info["version"] != "9" {
		t.Errorf("expired-but-refreshable initialize must relay the SERVER result, got %v", res)
	}
	if got := fb.refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls = %d, want 1", got)
	}
}

func TestInitializeUnpairedFallsBackToLocalStub(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "") // no credentials at all

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-01-01"}}`)

	res := out[0]["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], ProtocolVersion)
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "FocusAlly" || info["version"] != "tracker" {
		t.Errorf("serverInfo = %v", info)
	}
	tools := res["capabilities"].(map[string]any)["tools"].(map[string]any)
	if tools["listChanged"] != true {
		t.Errorf("local initialize must advertise listChanged: %v", res)
	}
}

func TestMalformedAndOversizedInputSurvive(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	big := `{"jsonrpc":"2.0","id":1,"method":"x","params":"` + strings.Repeat("a", maxMessage) + `"}`
	out := drive(t, dir, fb,
		`this is not json`,
		big,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(out) != 3 {
		t.Fatalf("got %d replies, want 3 (2 parse errors + ping)", len(out))
	}
	for _, m := range out[:2] {
		errObj, ok := m["error"].(map[string]any)
		if !ok || errObj["code"] != float64(-32700) {
			t.Errorf("expected -32700, got %v", m)
		}
	}
	if out[2]["result"] == nil {
		t.Errorf("loop did not survive to answer ping: %v", out[2])
	}
}

func TestConcurrent401sRefreshOnce(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "bad-expired")

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = api.RefreshUnderLock(dir, fb.srv.URL, "c1", "bad-expired")
		}()
	}
	wg.Wait()

	if got := fb.refreshCalls.Load(); got != 1 {
		t.Errorf("token round-trips = %d, want exactly 1", got)
	}
	creds, ok := api.LoadCredentials(dir)
	if !ok || creds.AccessToken != "fresh-token" {
		t.Errorf("credentials after concurrent refresh: %+v ok=%v", creds, ok)
	}
}
