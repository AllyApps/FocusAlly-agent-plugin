package pairing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/proc"
)

// Mirrors the backend's OAuthConsentPage.render output: the raw pairing
// code is embedded as a JS string literal `var code = "XXXXXXXX";`.
const consentPageExcerpt = `
<div class="code" aria-label="pairing code">ABCD-2345</div>
<script>
(function() {
    var code = "ABCD2345";
    var statusEl = document.getElementById('status');
})();
</script>`

func TestConsentPageCodeParsing(t *testing.T) {
	m := consentCodeRe.FindStringSubmatch(consentPageExcerpt)
	if m == nil || m[1] != "ABCD2345" {
		t.Fatalf("consent code parse = %v", m)
	}
}

func TestConsentCodeRegexRejectsAmbiguousAlphabet(t *testing.T) {
	// Backend alphabet excludes 0, 1, I, L, O, U.
	for _, bad := range []string{`var code = "ABCD1234"`, `var code = "ABCDO345"`, `var code = "ABC"`} {
		if consentCodeRe.MatchString(bad) {
			t.Fatalf("regex must reject %q", bad)
		}
	}
}

func TestFormatCode(t *testing.T) {
	if got := FormatCode("ABCD2345"); got != "ABCD-2345" {
		t.Fatalf("FormatCode = %q", got)
	}
	if got := FormatCode("short"); got != "short" {
		t.Fatalf("FormatCode non-8 = %q", got)
	}
}

// A pairing interrupted by process death must RESUME: poll the stored
// code and exchange with the STORED verifier — never mint a new code
// (minting is what pops a second approval window / deeplink).
func TestResumeUsesStoredVerifierAndMintsNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // neutralize `claude` / `open` lookups

	const storedVerifier = "stored-verifier-from-dead-process-123"
	var mintCalls, tokenCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/authorize" && r.Method == http.MethodGet:
			mintCalls.Add(1)
			t.Error("resume must not mint a new pairing code")
		case r.URL.Path == "/oauth/authorize/ABCD2345/status":
			fmt.Fprint(w, `{"status":"approved","redirectTo":"http://localhost/focusally-tracker/callback?code=raw-auth-77"}`)
		case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
			tokenCalls.Add(1)
			r.ParseForm()
			if got := r.PostForm.Get("code_verifier"); got != storedVerifier {
				t.Errorf("token exchange used verifier %q, want the stored one", got)
			}
			if got := r.PostForm.Get("code"); got != "raw-auth-77" {
				t.Errorf("token exchange code = %q", got)
			}
			if got := r.PostForm.Get("grant_type"); got != "authorization_code" {
				t.Errorf("grant_type = %q", got)
			}
			if got := r.PostForm.Get("redirect_uri"); got != RedirectURI {
				t.Errorf("redirect_uri = %q", got)
			}
			fmt.Fprint(w, `{"access_token":"fa_mcp_new","refresh_token":"fa_mcr_new","token_type":"Bearer","expires_in":3600,"scope":"agent:write"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	configDir := t.TempDir()
	if err := api.SaveConfig(configDir, api.Config{
		BaseURL:  srv.URL,
		ClientID: "6b1f8f2c-0000-4000-8000-000000000001",
	}); err != nil {
		t.Fatal(err)
	}
	pendingData, _ := json.Marshal(PendingFile{
		Code:      "ABCD2345",
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Verifier:  storedVerifier,
	})
	os.WriteFile(pendingPath(configDir), pendingData, 0o600)

	Run(configDir)

	if tokenCalls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenCalls.Load())
	}
	creds, ok := api.LoadCredentials(configDir)
	if !ok || creds.AccessToken != "fa_mcp_new" || creds.RefreshToken != "fa_mcr_new" {
		t.Fatalf("credentials after resume = %+v ok=%v", creds, ok)
	}
	if _, err := os.Stat(pendingPath(configDir)); !os.IsNotExist(err) {
		t.Fatal("pending file must be removed after successful exchange")
	}
}

// A lock left by a dead poller must be reclaimed immediately (PID
// liveness probe), not after the 16-minute time window.
func TestStaleLockWithDeadPidIsReclaimed(t *testing.T) {
	configDir := t.TempDir()
	dead := exec.Command(os.Args[0], "-test.run=DefinitelyNoSuchTest")
	if err := dead.Run(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	deadPid := dead.ProcessState.Pid()
	if proc.PidAlive(deadPid) {
		t.Skipf("pid %d unexpectedly alive (reused?)", deadPid)
	}
	os.WriteFile(lockPath(configDir), []byte(fmt.Sprint(deadPid)), 0o600) // fresh mtime

	if !acquireLock(configDir) {
		t.Fatal("lock held by a dead PID must be reclaimed")
	}
	os.Remove(lockPath(configDir))
}

func TestLockWithLivePidIsRespected(t *testing.T) {
	configDir := t.TempDir()
	os.WriteFile(lockPath(configDir), []byte(fmt.Sprint(os.Getpid())), 0o600)

	if acquireLock(configDir) {
		t.Fatal("fresh lock held by a live PID must not be stolen")
	}
}

func TestLoadPendingSurvivesProcessDeathAndKeepsSameCode(t *testing.T) {
	configDir := t.TempDir()
	pendingData, _ := json.Marshal(PendingFile{
		Code:      "WXYZ7654",
		ExpiresAt: time.Now().Add(5 * time.Minute),
		Verifier:  "v",
	})
	os.WriteFile(filepath.Join(configDir, "pairing.json"), pendingData, 0o600)

	// Two loads (— two SessionStarts on a rebooted machine) must show
	// the same code.
	first, ok1 := LoadPending(configDir)
	second, ok2 := LoadPending(configDir)
	if !ok1 || !ok2 || first.Code != "WXYZ7654" || second.Code != first.Code {
		t.Fatalf("pending must persist and be stable: %+v / %+v", first, second)
	}
	if first.Verifier != "v" {
		t.Fatal("verifier must round-trip through the pending file")
	}
}

func TestLoadPendingRejectsExpired(t *testing.T) {
	configDir := t.TempDir()
	pendingData, _ := json.Marshal(PendingFile{
		Code:      "WXYZ7654",
		ExpiresAt: time.Now().Add(-time.Minute),
		Verifier:  "v",
	})
	os.WriteFile(filepath.Join(configDir, "pairing.json"), pendingData, 0o600)
	if _, ok := LoadPending(configDir); ok {
		t.Fatal("expired pending must not load")
	}
}

func TestExtractAuthCode(t *testing.T) {
	code := extractAuthCode("http://localhost/focusally-tracker/callback?code=raw-auth-code&state=x")
	if code != "raw-auth-code" {
		t.Fatalf("extractAuthCode = %q", code)
	}
	if extractAuthCode("://bad url") != "" {
		t.Fatal("bad url must yield empty code")
	}
}
