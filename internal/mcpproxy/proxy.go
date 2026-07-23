// Package mcpproxy is the `tracker mcp` stdio transport: a transparent
// JSON-RPC pipe between a local MCP client and the backend's stateless
// POST /mcp endpoint. It injects the profile's Bearer token, refreshes
// it on 401 (serialized via credentials.lock), and never models the
// server's tools — requests and replies pass through byte-identical.
package mcpproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
)

const (
	// maxMessage matches the hook payload cap.
	maxMessage = 4 << 20
	// ProtocolVersion is the MCP revision the backend speaks.
	ProtocolVersion = "2025-06-18"
)

// Deps carries everything Serve needs; the HTTP client, spawn
// function, and wait times are injectable for tests.
type Deps struct {
	Profile   string
	ConfigDir string
	Client    *http.Client
	// RootConfigDir holds tracker.json (the trackingProfile setting
	// auth.status reports).
	RootConfigDir string
	// StateDirFor resolves a profile's state dir for the dirty-session
	// count.
	StateDirFor func(profile string) (string, error)
	// Spawn re-execs the tracker binary detached (auth.login uses it to
	// start the pairing poller).
	Spawn func(args ...string)
	// LoginWait bounds how long auth.login waits for a freshly minted
	// pairing code; zero means the default.
	LoginWait time.Duration
}

// pairingPollInterval is how often the watcher re-checks credentials
// while unpaired (a var so tests can shrink it).
var pairingPollInterval = 3 * time.Second

type server struct {
	deps     Deps
	base     string
	clientID string
	out      *syncWriter

	// paired mirrors the last observed credential state; transitions
	// emit notifications/tools/list_changed exactly once per flip.
	paired atomic.Bool
	// wake nudges the watcher out of its paused (paired) state when the
	// forward path detects credential loss.
	wake chan struct{}

	// elicitationOK records whether the client declared the elicitation
	// capability at initialize — only then may the proxy pop a native
	// login dialog.
	elicitationOK atomic.Bool
	// elicitMuted suppresses further login dialogs after the user
	// declined one; cleared on the next pairing transition.
	elicitMuted atomic.Bool
	// streamDown flips when stdin is gone — server→client requests must
	// fail fast instead of waiting out their timeout.
	streamDown atomic.Bool

	// pending holds waiters for in-flight server→client requests
	// (elicitation), keyed by the raw JSON id. The reader goroutine
	// routes client responses here directly (bypassing the dispatch
	// queue), so a handler blocked on a dialog still receives its
	// response; in the worst case (a client pipelining a full queue of
	// requests ahead of the response) the dialog's own timeout bounds
	// the stall.
	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage
	reqSeq    atomic.Int64
}

// Serve pumps newline-delimited JSON-RPC messages from in until EOF.
// Every failure is answered on the wire (or dropped, for
// notifications) — the loop itself never dies on bad input. A reader
// goroutine feeds a dispatch queue and short-circuits client responses
// to their waiters; requests are still handled strictly in arrival
// order.
func Serve(in io.Reader, out io.Writer, deps Deps) error {
	if deps.Client == nil {
		deps.Client = http.DefaultClient
	}
	cfg, _ := api.LoadConfig(deps.ConfigDir)
	s := &server{
		deps:     deps,
		base:     cfg.ResolvedBaseURL(),
		clientID: cfg.ClientID,
		out:      &syncWriter{w: out},
		wake:     make(chan struct{}, 1),
		pending:  make(map[string]chan json.RawMessage),
	}
	s.paired.Store(s.credentialsPresent())

	stop := make(chan struct{})
	var watcherDone sync.WaitGroup
	watcherDone.Add(1)
	defer watcherDone.Wait()
	defer close(stop)
	go func() {
		defer watcherDone.Done()
		s.watchPairing(stop)
	}()

	requests := make(chan []byte, 64)
	var readErr error
	go func() {
		defer func() {
			s.streamDown.Store(true)
			s.failPending()
			close(requests)
		}()
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			line, tooLong, err := readLine(r)
			if tooLong {
				s.writeError(nil, -32700, "message exceeds size limit")
			} else if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				if !s.routeResponse(trimmed) {
					requests <- trimmed
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				return
			}
		}
	}()

	for raw := range requests {
		s.handle(raw)
	}
	return readErr
}

// routeResponse claims messages that are RESPONSES to server-initiated
// requests (an id but no method) and delivers them to their waiter.
// Stray responses are swallowed — they must never be forwarded to the
// backend as if they were requests.
func (s *server) routeResponse(raw []byte) bool {
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		return false
	}
	if env.Method != "" || len(env.ID) == 0 || string(env.ID) == "null" {
		return false
	}
	s.pendingMu.Lock()
	ch, ok := s.pending[string(env.ID)]
	if ok {
		delete(s.pending, string(env.ID))
	}
	s.pendingMu.Unlock()
	if ok {
		ch <- raw
	}
	return true
}

// request sends a server→client JSON-RPC request and waits for the
// response. The reader goroutine delivers it, so blocking here does
// not stall the read loop.
func (s *server) request(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if s.streamDown.Load() {
		return nil, errors.New("client stream closed")
	}
	id := fmt.Sprintf(`"focusally-req-%d"`, s.reqSeq.Add(1))
	ch := make(chan json.RawMessage, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	cleanup := func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	s.out.writeMessage(msg)

	select {
	case raw, ok := <-ch:
		if !ok {
			return nil, errors.New("client stream closed")
		}
		return raw, nil
	case <-time.After(timeout):
		cleanup()
		return nil, errors.New("client response timed out")
	}
}

// failPending closes every waiter when stdin goes away, so a handler
// blocked on a dialog unwinds immediately instead of waiting out its
// timeout.
func (s *server) failPending() {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, ch := range s.pending {
		delete(s.pending, id)
		close(ch)
	}
}

// readLine reads one newline-delimited message, tolerating (and
// discarding) lines above maxMessage instead of killing the loop.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, e := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > maxMessage {
				tooLong = true
				line = nil
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		return line, tooLong, e
	}
}

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (s *server) handle(raw []byte) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.writeError(nil, -32700, "parse error")
		return
	}
	if len(env.ID) == 0 || string(env.ID) == "null" {
		// Client notification (initialized, cancelled, …) — the
		// stateless backend has no use for it.
		return
	}
	switch env.Method {
	case "ping":
		s.writeResult(env.ID, json.RawMessage(`{}`))
	case "initialize":
		s.handleInitialize(env.ID, raw)
	case "tools/list":
		s.handleToolsList(env.ID, raw)
	case "tools/call":
		var p struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &p)
		if localToolNames[p.Params.Name] {
			s.handleLocalTool(env.ID, p.Params.Name, raw)
			return
		}
		s.forwardAndReply(env.ID, env.Method, raw)
	default:
		s.forwardAndReply(env.ID, env.Method, raw)
	}
}

// credentialsPresent is the cheap paired probe (stat + read + compare).
func (s *server) credentialsPresent() bool {
	_, ok := api.LoadCredentialsBound(s.deps.ConfigDir, s.base)
	return ok
}

// watchPairing emits notifications/tools/list_changed when the profile
// transitions unpaired → paired (the detached poller finished the
// exchange), so the client re-fetches the now-full tool list. While
// paired it does not poll at all — the 401 path detects loss and wakes
// it back up.
func (s *server) watchPairing(stop <-chan struct{}) {
	for {
		if s.paired.Load() {
			select {
			case <-stop:
				return
			case <-s.wake:
			}
			continue
		}
		select {
		case <-stop:
			return
		case <-time.After(pairingPollInterval):
		}
		if s.credentialsPresent() {
			s.notePaired()
		}
	}
}

// notePaired / noteUnpaired flip the shared state; the transition edge
// (and only the edge) emits list_changed.
func (s *server) notePaired() {
	if !s.paired.Swap(true) {
		s.elicitMuted.Store(false)
		s.emitListChanged()
	}
}

func (s *server) noteUnpaired() {
	if s.paired.Swap(false) {
		s.elicitMuted.Store(false)
		s.emitListChanged()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *server) emitListChanged() {
	s.out.writeMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
}

// handleInitialize forwards first — an expired-but-refreshable token
// must refresh-and-retry rather than present as offline. Only when the
// backend is unreachable or the profile is unpaired does the local
// result take over.
func (s *server) handleInitialize(id json.RawMessage, raw []byte) {
	var init struct {
		Params struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &init)
	_, hasElicitation := init.Params.Capabilities["elicitation"]
	s.elicitationOK.Store(hasElicitation)

	body, outcome, _ := s.forward(raw)
	if outcome == fwdOK {
		s.out.writeMessage(patchInitializeResult(body))
		return
	}
	s.writeResult(id, s.localInitializeResult(raw, outcome == fwdUnpaired))
}

// handleToolsList overlays the two local auth tools onto the server's
// list. Unpaired, the local tools ARE the list; a paired transport
// failure stays an error so a flaky backend is visible, not masked.
func (s *server) handleToolsList(id json.RawMessage, raw []byte) {
	body, outcome, cause := s.forward(raw)
	switch outcome {
	case fwdOK:
		s.out.writeMessage(appendLocalTools(body))
	case fwdUnpaired:
		s.writeResult(id, map[string]any{"tools": localToolDefs})
	case fwdError:
		s.writeError(id, -32000, "focusally proxy: "+cause)
	}
}

// appendLocalTools splices the local auth tool definitions into the
// server's tools/list result, keeping the server's tool objects
// byte-preserved. A server duplicate of a local tool name is dropped —
// the local implementation wins. Any shape surprise relays verbatim.
func appendLocalTools(body []byte) []byte {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return body
	}
	resRaw, ok := env["result"]
	if !ok {
		return body
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(resRaw, &result) != nil || result == nil {
		return body
	}
	var tools []json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil {
		return body
	}
	kept := tools[:0]
	for _, tool := range tools {
		var def struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(tool, &def) == nil && localToolNames[def.Name] {
			continue
		}
		kept = append(kept, tool)
	}
	kept = append(kept, localToolDefs...)

	toolsOut, err := marshalNoEscape(kept)
	if err != nil {
		return body
	}
	result["tools"] = toolsOut
	resOut, err := marshalNoEscape(result)
	if err != nil {
		return body
	}
	env["result"] = resOut
	out, err := marshalNoEscape(env)
	if err != nil {
		return body
	}
	return out
}

type fwdOutcome int

const (
	fwdOK fwdOutcome = iota
	fwdUnpaired
	fwdError
)

// forward POSTs the raw message to <base>/mcp with the profile's
// Bearer token, refreshing once on 401. Outcomes feed the paired-state
// tracker so tool-list change notifications fire on transitions.
func (s *server) forward(raw []byte) ([]byte, fwdOutcome, string) {
	body, outcome, cause := s.forwardOnce(raw)
	switch outcome {
	case fwdOK:
		s.notePaired()
	case fwdUnpaired:
		s.noteUnpaired()
	}
	return body, outcome, cause
}

func (s *server) forwardOnce(raw []byte) ([]byte, fwdOutcome, string) {
	creds, ok := api.LoadCredentialsBound(s.deps.ConfigDir, s.base)
	if !ok {
		return nil, fwdUnpaired, ""
	}
	body, status, err := s.post(raw, creds.AccessToken)
	if err != nil {
		return nil, fwdError, err.Error()
	}
	if status == http.StatusUnauthorized {
		refreshed, outcome := api.RefreshUnderLock(s.deps.ConfigDir, s.base, s.clientID, creds.AccessToken)
		switch outcome {
		case api.RefreshRejected:
			return nil, fwdUnpaired, ""
		case api.RefreshTransient:
			return nil, fwdError, "token refresh failed, will retry"
		}
		body, status, err = s.post(raw, refreshed.AccessToken)
		if err != nil {
			return nil, fwdError, err.Error()
		}
		if status == http.StatusUnauthorized {
			return nil, fwdUnpaired, ""
		}
	}
	if status != http.StatusOK {
		return nil, fwdError, fmt.Sprintf("backend returned HTTP %d", status)
	}
	return body, fwdOK, ""
}

func (s *server) post(raw []byte, accessToken string) (body []byte, status int, err error) {
	req, err := http.NewRequest(http.MethodPost, s.base+"/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.deps.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

func (s *server) forwardAndReply(id json.RawMessage, method string, raw []byte) {
	body, outcome, cause := s.forward(raw)
	switch outcome {
	case fwdOK:
		s.out.writeMessage(body)
	case fwdUnpaired:
		if method == "tools/call" && s.elicitLoginAndRetry(id, raw) {
			return
		}
		s.replyUnpaired(id, method)
	case fwdError:
		s.writeError(id, -32000, "focusally proxy: "+cause)
	}
}

// replyUnpaired answers a request that cannot reach the backend for
// lack of valid credentials. Tool calls get an isError RESULT — agents
// read the text and can self-recover via auth.login; everything else
// gets a JSON-RPC error.
func (s *server) replyUnpaired(id json.RawMessage, method string) {
	msg := s.unpairedMessage()
	if method == "tools/call" {
		s.writeResult(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": msg}},
			"isError": true,
		})
		return
	}
	s.writeError(id, -32002, msg)
}

func (s *server) unpairedMessage() string {
	return fmt.Sprintf(
		"FocusAlly is not connected for profile %s. Call the auth.login tool to connect.",
		s.deps.Profile,
	)
}

// patchInitializeResult sets result.capabilities.tools.listChanged =
// true on the relayed initialize result — the proxy emits
// notifications/tools/list_changed on pairing transitions, which the
// bare backend does not advertise. Everything else stays byte-intact;
// any shape surprise falls back to relaying verbatim.
func patchInitializeResult(body []byte) []byte {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return body
	}
	resRaw, ok := env["result"]
	if !ok {
		return body
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(resRaw, &result) != nil || result == nil {
		return body
	}
	capsMap := map[string]json.RawMessage{}
	if capsRaw, ok := result["capabilities"]; ok {
		if json.Unmarshal(capsRaw, &capsMap) != nil || capsMap == nil {
			capsMap = map[string]json.RawMessage{}
		}
	}
	toolsMap := map[string]json.RawMessage{}
	if toolsRaw, ok := capsMap["tools"]; ok {
		if json.Unmarshal(toolsRaw, &toolsMap) != nil || toolsMap == nil {
			toolsMap = map[string]json.RawMessage{}
		}
	}
	toolsMap["listChanged"] = json.RawMessage("true")

	toolsOut, err := marshalNoEscape(toolsMap)
	if err != nil {
		return body
	}
	capsMap["tools"] = toolsOut
	capsOut, err := marshalNoEscape(capsMap)
	if err != nil {
		return body
	}
	result["capabilities"] = capsOut
	resOut, err := marshalNoEscape(result)
	if err != nil {
		return body
	}
	env["result"] = resOut
	out, err := marshalNoEscape(env)
	if err != nil {
		return body
	}
	return out
}

// localInitializeResult is the offline/unpaired initialize answer.
// While unpaired it also carries instructions pointing the agent at
// auth.login.
func (s *server) localInitializeResult(raw []byte, unpaired bool) map[string]any {
	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &req)
	version := ProtocolVersion
	if req.Params.ProtocolVersion == ProtocolVersion {
		version = req.Params.ProtocolVersion
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "FocusAlly", "version": "tracker"},
	}
	if unpaired {
		result["instructions"] = fmt.Sprintf(
			"FocusAlly is not connected for profile %s. Call the auth.login tool to get a pairing code, then have the user approve it in the FocusAlly app (Profile → MCP keys → enter code). The full tool list appears automatically after approval.",
			s.deps.Profile,
		)
	}
	return result
}

func (s *server) writeResult(id json.RawMessage, result any) {
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      normalizeID(id),
		"result":  result,
	})
	if err != nil {
		return
	}
	s.out.writeMessage(msg)
}

func (s *server) writeError(id json.RawMessage, code int, message string) {
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      normalizeID(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return
	}
	s.out.writeMessage(msg)
}

// marshalNoEscape marshals without HTML escaping, so server-authored
// bytes spliced through the patch paths stay byte-identical (plain
// json.Marshal would rewrite <, >, & even inside RawMessage).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func normalizeID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// syncWriter serializes writes so a message can never interleave with
// another (the pairing watcher writes from its own goroutine), and
// compacts every payload so one message is always exactly one line.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) writeMessage(raw []byte) {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return
	}
	buf.WriteByte('\n')
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.w.Write(buf.Bytes())
}
