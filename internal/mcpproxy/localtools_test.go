package mcpproxy

import (
	stdbufio "bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/pairing"
)

func toolNames(t *testing.T, m map[string]any) []string {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	tools, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("no tools array in %v", res)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func toolText(t *testing.T, m map[string]any) string {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	content := res["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

func TestUnpairedToolsListIsAuthOnly(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	names := toolNames(t, out[0])
	if len(names) != 2 || names[0] != "auth.status" || names[1] != "auth.login" {
		t.Errorf("unpaired tools/list = %v, want exactly the two auth tools", names)
	}
	if len(fb.mcpBodies) != 0 {
		t.Errorf("unpaired tools/list must not hit the backend")
	}
}

func TestUnpairedInitializeCarriesInstructions(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)

	res := out[0]["result"].(map[string]any)
	instructions, _ := res["instructions"].(string)
	if !strings.Contains(instructions, "auth.login") || !strings.Contains(instructions, "default") {
		t.Errorf("instructions must name auth.login and the profile: %q", instructions)
	}
}

func TestPairedToolsListAppendsAndDedupes(t *testing.T) {
	fb := newFakeBackend(t)
	fb.toolsListJSON = `[{"name":"tasks.create","inputSchema":{"type":"object","futureKeyword":[1,2]}},{"name":"auth.login","description":"server impostor"},{"name":"sessions.list"}]`
	dir := setupProfile(t, fb, "good-token")

	raw := driveRaw(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var m map[string]any
	if err := json.Unmarshal(raw[0], &m); err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, m)
	want := []string{"tasks.create", "sessions.list", "auth.status", "auth.login"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("tools = %v, want %v (server dupe dropped, locals appended)", names, want)
	}
	if bytes.Contains(raw[0], []byte("server impostor")) {
		t.Error("server duplicate of a local tool must be dropped")
	}
	if !bytes.Contains(raw[0], []byte(`"futureKeyword":[1,2]`)) {
		t.Error("server tool objects must stay byte-preserved")
	}
}

func TestPairedToolsListPreservesHTMLishBytes(t *testing.T) {
	fb := newFakeBackend(t)
	fb.toolsListJSON = `[{"name":"tasks.create","description":"use <task> & {\"a\":1} > see docs"}]`
	dir := setupProfile(t, fb, "good-token")

	raw := driveRaw(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	if !bytes.Contains(raw[0], []byte(`"use <task> & {\"a\":1} > see docs"`)) {
		t.Errorf("server description bytes were rewritten (HTML escaping?): %s", raw[0])
	}
}

func TestTransientRefreshFailureIsNotUnpaired(t *testing.T) {
	fb := newFakeBackend(t)
	fb.refreshStatus = 503
	dir := setupProfile(t, fb, "bad-expired")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tasks.list"}}`)

	replies, notifications := splitNotifications(out)
	if len(notifications) != 0 {
		t.Errorf("transient refresh failure must not flip paired state: %v", notifications)
	}
	errObj, ok := replies[0]["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32000) {
		t.Errorf("transient refresh failure must be a -32000 error, got %v", replies[0])
	}
	if _, ok := api.LoadCredentials(dir); !ok {
		t.Error("transient refresh failure must leave credentials on disk")
	}
}

func TestAuthStatusUnpairedWithDirtySessions(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	rootDir := t.TempDir()
	stateDir := t.TempDir()
	sessions := filepath.Join(stateDir, "sessions")
	os.MkdirAll(sessions, 0o700)
	os.WriteFile(filepath.Join(sessions, "a.json"), []byte(`{"dirty":true}`), 0o600)
	os.WriteFile(filepath.Join(sessions, "b.json"), []byte(`{"dirty":false}`), 0o600)
	os.WriteFile(filepath.Join(sessions, "c.json"), []byte(`{"dirty":true}`), 0o600)
	os.WriteFile(filepath.Join(sessions, "broken.json"), []byte(`{{{`), 0o600)
	api.SaveGlobalConfig(rootDir, api.GlobalConfig{TrackingProfile: "work"})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.status"}}` + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{
		Profile:       "default",
		ConfigDir:     dir,
		Client:        fb.srv.Client(),
		RootConfigDir: rootDir,
		StateDirFor: func(profile string) (string, error) {
			if profile != "work" {
				t.Errorf("dirty count must use the TRACKING profile, asked for %q", profile)
			}
			return stateDir, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(toolText(t, m)), &status); err != nil {
		t.Fatalf("auth.status text is not JSON: %v", err)
	}
	if status["paired"] != false || status["profile"] != "default" {
		t.Errorf("status = %v", status)
	}
	if status["trackingProfile"] != "work" || status["dirtySessions"] != float64(2) {
		t.Errorf("tracking fields wrong: %v", status)
	}
	if status["apiBase"] != fb.srv.URL {
		t.Errorf("apiBase = %v", status["apiBase"])
	}
}

func TestAuthStatusPairedAndTrackingNone(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")
	rootDir := t.TempDir()
	api.SaveGlobalConfig(rootDir, api.GlobalConfig{TrackingProfile: "none"})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.status"}}` + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{
		Profile:       "default",
		ConfigDir:     dir,
		Client:        fb.srv.Client(),
		RootConfigDir: rootDir,
		StateDirFor: func(string) (string, error) {
			t.Error("trackingProfile none must not scan state dirs")
			return "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(buf.Bytes(), &m)
	var status map[string]any
	if err := json.Unmarshal([]byte(toolText(t, m)), &status); err != nil {
		t.Fatal(err)
	}
	if status["paired"] != true {
		t.Errorf("paired = %v", status["paired"])
	}
	if status["tokenExpiresAt"] == nil {
		t.Error("paired status must report tokenExpiresAt")
	}
	if status["trackingProfile"] != "none" || status["dirtySessions"] != float64(0) {
		t.Errorf("none tracking fields wrong: %v", status)
	}
}

func TestAuthStatusReportsMismatchedBinding(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	api.SaveCredentials(dir, "https://other.example", api.Credentials{AccessToken: "at"})

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.status"}}`)

	var status map[string]any
	if err := json.Unmarshal([]byte(toolText(t, out[0])), &status); err != nil {
		t.Fatal(err)
	}
	if status["paired"] != false || status["credentialsBoundTo"] != "https://other.example" {
		t.Errorf("mismatch not surfaced: %v", status)
	}
}

func TestAuthLoginUnpairedReturnsCode(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	pendingJSON, _ := json.Marshal(pairing.PendingFile{
		Code:      "ABCD2345",
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Verifier:  "v",
	})
	os.WriteFile(filepath.Join(dir, "pairing.json"), pendingJSON, 0o600)

	var spawned [][]string
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.login"}}` + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{
		Profile:   "work",
		ConfigDir: dir,
		Client:    fb.srv.Client(),
		Spawn:     func(args ...string) { spawned = append(spawned, args) },
		LoginWait: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(buf.Bytes(), &m)
	text := toolText(t, m)
	if !strings.Contains(text, "ABCD-2345") {
		t.Errorf("login text must carry the formatted code: %q", text)
	}
	if !strings.Contains(text, "MCP keys") {
		t.Errorf("login text must carry approval instructions: %q", text)
	}
	if len(spawned) != 1 || fmt.Sprint(spawned[0]) != fmt.Sprint([]string{"pair", "--profile", "work"}) {
		t.Errorf("spawned = %v, want one pair --profile work", spawned)
	}
}

func TestAuthLoginPairedSaysConnected(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")

	out := drive(t, dir, fb, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.login"}}`)

	text := toolText(t, out[0])
	if !strings.Contains(text, "already connected") {
		t.Errorf("paired auth.login = %q", text)
	}
}

func TestAuthLoginForceDropsCredentialsAndMintsCode(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")
	pendingJSON, _ := json.Marshal(pairing.PendingFile{
		Code:      "NEWC2345",
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Verifier:  "v",
	})
	os.WriteFile(filepath.Join(dir, "pairing.json"), pendingJSON, 0o600)

	var spawned [][]string
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.login","arguments":{"force":true}}}` + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{
		Profile:   "default",
		ConfigDir: dir,
		Client:    fb.srv.Client(),
		Spawn:     func(args ...string) { spawned = append(spawned, args) },
		LoginWait: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := api.LoadCredentials(dir); ok {
		t.Error("force login must drop the current credentials")
	}
	var replies, notifications []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if _, hasID := m["id"]; hasID {
			replies = append(replies, m)
		} else {
			notifications = append(notifications, m)
		}
	}
	if len(notifications) != 1 || notifications[0]["method"] != "notifications/tools/list_changed" {
		t.Errorf("force login must emit one list_changed, got %v", notifications)
	}
	text := toolText(t, replies[0])
	if !strings.Contains(text, "NEWC-2345") {
		t.Errorf("force login must return the fresh code: %q", text)
	}
	if len(spawned) != 1 {
		t.Errorf("force login must spawn the pairing poller, spawned = %v", spawned)
	}
}

func TestAuthLoginExpiredPendingTreatedAbsent(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	pendingJSON, _ := json.Marshal(pairing.PendingFile{
		Code:      "DEAD2345",
		ExpiresAt: time.Now().Add(-time.Minute),
		Verifier:  "v",
	})
	os.WriteFile(filepath.Join(dir, "pairing.json"), pendingJSON, 0o600)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"auth.login"}}` + "\n")
	var buf bytes.Buffer
	err := Serve(in, &buf, Deps{
		Profile:   "default",
		ConfigDir: dir,
		Client:    fb.srv.Client(),
		Spawn:     func(...string) {},
		LoginWait: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(buf.Bytes(), &m)
	text := toolText(t, m)
	if strings.Contains(text, "DEAD-2345") {
		t.Errorf("expired pending code must not be surfaced: %q", text)
	}
	if !strings.Contains(text, "not ready yet") {
		t.Errorf("expected the not-ready reply, got %q", text)
	}
}

func TestWatcherEmitsListChangedOnPairing(t *testing.T) {
	orig := pairingPollInterval
	pairingPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pairingPollInterval = orig })

	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(inR, outW, Deps{Profile: "default", ConfigDir: dir, Client: fb.srv.Client()})
	}()
	lines := make(chan string, 16)
	go pumpLines(outR, lines)

	// Handshake first, so the credentials file cannot appear before
	// Serve snapshots its initial unpaired state.
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	select {
	case <-lines:
	case <-time.After(3 * time.Second):
		t.Fatal("no ping reply")
	}

	// Pairing completes out-of-band: the poller writes credentials.
	if err := api.SaveCredentials(dir, fb.srv.URL, api.Credentials{AccessToken: "good-token"}); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		if !strings.Contains(line, "notifications/tools/list_changed") {
			t.Errorf("first message after pairing = %q, want list_changed", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no list_changed after credentials appeared")
	}

	// A definitive refresh rejection flips it back: force via a 401'd
	// request whose refresh dies definitively.
	fb.refreshStatus = 400
	api.SaveCredentials(dir, fb.srv.URL, api.Credentials{AccessToken: "bad-token"})
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":9,"method":"resources/list"}`)

	sawSecond := false
	deadline := time.After(3 * time.Second)
	for !sawSecond {
		select {
		case line := <-lines:
			if strings.Contains(line, "list_changed") {
				sawSecond = true
			}
		case <-deadline:
			t.Fatal("no list_changed after credential loss")
		}
	}

	inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	outW.Close()
}

// pumpLines forwards newline-delimited messages from r into ch.
func pumpLines(r io.Reader, ch chan<- string) {
	br := stdbufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			ch <- strings.TrimSpace(line)
		}
		if err != nil {
			close(ch)
			return
		}
	}
}
