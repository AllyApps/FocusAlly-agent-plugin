package mcpproxy

import (
	stdbufio "bufio"
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

// elicitClient drives Serve interactively over pipes, like a real MCP
// client with the elicitation capability.
type elicitClient struct {
	t     *testing.T
	inW   *io.PipeWriter
	lines chan map[string]any
	done  chan error
}

func startElicitClient(t *testing.T, dir string, fb *fakeBackend, withCapability bool) *elicitClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &elicitClient{t: t, inW: inW, lines: make(chan map[string]any, 16), done: make(chan error, 1)}
	go func() {
		c.done <- Serve(inR, outW, Deps{
			Profile:   "default",
			ConfigDir: dir,
			Client:    fb.srv.Client(),
			Spawn:     func(...string) {},
			LoginWait: 200 * time.Millisecond,
		})
		outW.Close()
	}()
	go func() {
		br := stdbufio.NewReader(outR)
		for {
			line, err := br.ReadString('\n')
			if strings.TrimSpace(line) != "" {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil {
					t.Errorf("non-JSON output: %q", line)
				} else {
					c.lines <- m
				}
			}
			if err != nil {
				close(c.lines)
				return
			}
		}
	}()

	caps := `{}`
	if withCapability {
		caps = `{"elicitation":{}}`
	}
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":%s}}`, caps))
	if init := c.recv(); init["id"] != float64(1) {
		t.Fatalf("expected initialize reply, got %v", init)
	}
	return c
}

func (c *elicitClient) send(line string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.inW, line); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *elicitClient) recv() map[string]any {
	c.t.Helper()
	select {
	case m, ok := <-c.lines:
		if !ok {
			c.t.Fatal("output closed early")
		}
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for a message")
	}
	return nil
}

func (c *elicitClient) close() {
	c.inW.Close()
	if err := <-c.done; err != nil {
		c.t.Fatalf("Serve: %v", err)
	}
}

func injectPending(t *testing.T, dir, code string) {
	t.Helper()
	pendingJSON, _ := json.Marshal(pairing.PendingFile{
		Code:      code,
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Verifier:  "v",
	})
	if err := os.WriteFile(filepath.Join(dir, "pairing.json"), pendingJSON, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestElicitLoginAcceptRetriesOriginalCall(t *testing.T) {
	origPoll := elicitCredsPoll
	elicitCredsPoll = 20 * time.Millisecond
	t.Cleanup(func() { elicitCredsPoll = origPoll })

	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	injectPending(t, dir, "ABCD2345")
	c := startElicitClient(t, dir, fb, true)
	defer c.close()

	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks.list","arguments":{}}}`
	c.send(call)

	elicit := c.recv()
	if elicit["method"] != "elicitation/create" {
		t.Fatalf("expected elicitation/create, got %v", elicit)
	}
	msg := elicit["params"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "ABCD-2345") || !strings.Contains(msg, "MCP keys") {
		t.Errorf("dialog message must carry the code and instructions: %q", msg)
	}

	// Approval lands out-of-band, then the user presses Accept.
	if err := api.SaveCredentials(dir, fb.srv.URL, api.Credentials{AccessToken: "good-token"}); err != nil {
		t.Fatal(err)
	}
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"action":"accept","content":{}}}`, jsonID(t, elicit)))

	// list_changed (pairing transition inside the retry) and the actual
	// tool result, in either order.
	var toolReply map[string]any
	for toolReply == nil {
		m := c.recv()
		if m["method"] == "notifications/tools/list_changed" {
			continue
		}
		toolReply = m
	}
	if toolReply["id"] != float64(7) || toolReply["result"] == nil {
		t.Fatalf("expected the original call's forwarded result, got %v", toolReply)
	}
	if res := toolReply["result"].(map[string]any); res["isError"] == true {
		t.Fatalf("retry must succeed, got isError: %v", res)
	}
	if len(fb.mcpBodies) == 0 || string(fb.mcpBodies[len(fb.mcpBodies)-1]) != call {
		t.Errorf("original call must be re-forwarded byte-identical")
	}
}

func TestElicitLoginDeclineFallsBackAndMutes(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	injectPending(t, dir, "ABCD2345")
	c := startElicitClient(t, dir, fb, true)
	defer c.close()

	c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks.list"}}`)
	elicit := c.recv()
	if elicit["method"] != "elicitation/create" {
		t.Fatalf("expected elicitation/create, got %v", elicit)
	}
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"action":"decline"}}`, jsonID(t, elicit)))

	reply := c.recv()
	res, ok := reply["result"].(map[string]any)
	if !ok || res["isError"] != true {
		t.Fatalf("decline must fall back to the unpaired isError reply, got %v", reply)
	}

	// A second call must NOT pop another dialog — muted until the
	// pairing state changes.
	c.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"tasks.list"}}`)
	second := c.recv()
	if second["method"] == "elicitation/create" {
		t.Fatal("declined dialog must mute further elicitation")
	}
	if res := second["result"].(map[string]any); res["isError"] != true {
		t.Errorf("expected the plain unpaired reply, got %v", second)
	}
}

func TestNoElicitationWithoutCapability(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "")
	injectPending(t, dir, "ABCD2345")
	c := startElicitClient(t, dir, fb, false)
	defer c.close()

	c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tasks.list"}}`)
	reply := c.recv()
	if reply["method"] == "elicitation/create" {
		t.Fatal("must not elicit when the client lacks the capability")
	}
	if res := reply["result"].(map[string]any); res["isError"] != true {
		t.Errorf("expected the unpaired isError reply, got %v", reply)
	}
}

func TestStrayResponseIsNotForwarded(t *testing.T) {
	fb := newFakeBackend(t)
	dir := setupProfile(t, fb, "good-token")
	c := startElicitClient(t, dir, fb, true)
	defer c.close()

	before := len(fb.mcpBodies)
	c.send(`{"jsonrpc":"2.0","id":"focusally-req-999","result":{"action":"accept"}}`)
	c.send(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if ping := c.recv(); ping["id"] != float64(9) {
		t.Fatalf("expected ping reply, got %v", ping)
	}
	if len(fb.mcpBodies) != before {
		t.Errorf("a stray client response must never reach the backend: %d new bodies", len(fb.mcpBodies)-before)
	}
}

func jsonID(t *testing.T, msg map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(msg["id"])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
