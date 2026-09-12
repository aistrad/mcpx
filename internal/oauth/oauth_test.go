package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPKCEAndTokenRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	s := NewServer("op-pass", "https://mcp.example.com", secret, 3600)
	if err := s.Registry.AddPreregistered("cli", []string{"http://127.0.0.1/cb"}, ""); err != nil {
		t.Fatal(err)
	}
	verifier := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	if len(verifier) < 43 {
		t.Fatal("verifier too short")
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !VerifyS256(verifier, challenge) {
		t.Fatal("pkce")
	}
	code, err := s.IssueCode("cli", "http://127.0.0.1/cb", challenge, "S256", "https://mcp.example.com/mcp", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	tok, ttl, err := s.ExchangeCode(code, "http://127.0.0.1/cb", "cli", verifier, "")
	if err != nil || ttl <= 0 || tok == "" {
		t.Fatalf("exchange: %v ttl=%d", err, ttl)
	}
	if !s.ValidateAccessToken(tok, "https://mcp.example.com", "https://mcp.example.com/mcp") {
		t.Fatal("validate")
	}
	// replay code
	if _, _, err := s.ExchangeCode(code, "http://127.0.0.1/cb", "cli", verifier, ""); err == nil {
		t.Fatal("expected replay fail")
	}
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	s := NewServer("op-pass", "https://mcp.example.com", secret, 3600)
	if err := s.Registry.AddPreregistered("cli", []string{"http://127.0.0.1/cb"}, ""); err != nil {
		t.Fatal(err)
	}
	refresh, err := s.IssueRefreshToken("cli", "https://mcp.example.com/mcp", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if refresh == "" {
		t.Fatal("no refresh token")
	}
	tok, ttl, next, err := s.ExchangeRefreshToken(refresh, "cli", "")
	if err != nil || ttl <= 0 || tok == "" || next == "" {
		t.Fatalf("refresh: %v ttl=%d", err, ttl)
	}
	if !s.ValidateAccessToken(tok, "https://mcp.example.com", "https://mcp.example.com/mcp") {
		t.Fatal("validate refreshed token")
	}
	// rotated: old refresh token must be single-use
	if _, _, _, err := s.ExchangeRefreshToken(refresh, "cli", ""); err == nil {
		t.Fatal("expected old refresh token replay to fail")
	}
	// client binding
	other, err := s.IssueRefreshToken("cli", "https://mcp.example.com/mcp", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ExchangeRefreshToken(other, "not-cli", ""); err == nil {
		t.Fatal("expected client mismatch to fail")
	}
}

func TestRedirectRejectsPublicHTTP(t *testing.T) {
	_, err := ValidateRedirectURIs([]string{"http://evil.example/cb"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDCR(t *testing.T) {
	r := NewRegistry()
	out, err := r.Register(map[string]any{
		"redirect_uris":              []any{"https://app.example/callback"},
		"token_endpoint_auth_method": "none",
		"client_name":                "Web Chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := out["client_id"].(string)
	if id == "" {
		t.Fatal("no client_id")
	}
	if !r.AcceptsRedirect(id, "https://app.example/callback") {
		t.Fatal("redirect")
	}
}

func TestCheckPassword(t *testing.T) {
	s := NewServer("secret", "", make([]byte, 32), 60)
	if !s.CheckPassword("secret") || s.CheckPassword("wrong") {
		t.Fatal("password")
	}
}

func TestCIMDAccessTokenValidatesWithoutDCR(t *testing.T) {
	// Regression: ChatGPT OAuth issues tokens with client_id = CIMD URL; validation
	// must not require a DCR registry entry (would 401 after successful login).
	mux := http.NewServeMux()
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	docURL := ts.URL + "/oauth/ok.json"
	mux.HandleFunc("/oauth/ok.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":   docURL,
			"client_name": "ChatGPT",
			"redirect_uris": []string{
				"https://chatgpt.com/connector_platform_oauth_redirect",
			},
			"token_endpoint_auth_method":            "none",
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 3)
	}
	s := NewServer("pw", "https://mcp.example.com", secret, 3600)
	s.CIMD = NewCIMDResolver()
	s.CIMD.client = ts.Client()

	issuer := "https://mcp.example.com"
	aud := "https://mcp.example.com/mcp"
	tok, err := s.CreateAccessToken(docURL, aud, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidateAccessToken(tok, issuer, aud) {
		t.Fatal("CIMD-issued token must validate without DCR registration")
	}
	// Opaque / unknown client must still fail
	tok2, err := s.CreateAccessToken("unknown-client-id", aud, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if s.ValidateAccessToken(tok2, issuer, aud) {
		t.Fatal("unknown client token must not validate")
	}
}
