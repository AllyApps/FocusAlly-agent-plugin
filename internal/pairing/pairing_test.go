package pairing

import "testing"

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

func TestExtractAuthCode(t *testing.T) {
	code := extractAuthCode("http://localhost/focusally-tracker/callback?code=raw-auth-code&state=x")
	if code != "raw-auth-code" {
		t.Fatalf("extractAuthCode = %q", code)
	}
	if extractAuthCode("://bad url") != "" {
		t.Fatal("bad url must yield empty code")
	}
}
