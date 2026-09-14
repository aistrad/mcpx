package oauth

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryPersistReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth-clients.json")

	r1 := NewRegistry()
	if err := r1.SetPersistPath(path); err != nil {
		t.Fatal(err)
	}
	out, err := r1.Register(map[string]any{
		"redirect_uris":              []any{"https://chatgpt.com/connector/oauth/abc"},
		"token_endpoint_auth_method": "none",
		"client_name":                "ChatGPT",
	})
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := out["client_id"].(string)
	if cid == "" {
		t.Fatal("no client_id")
	}

	r2 := NewRegistry()
	if err := r2.SetPersistPath(path); err != nil {
		t.Fatal(err)
	}
	if !r2.AcceptsRedirect(cid, "https://chatgpt.com/connector/oauth/abc") {
		t.Fatal("persisted client not loaded")
	}
	c, ok := r2.Get(cid)
	if !ok || c.ClientName != "ChatGPT" {
		t.Fatalf("got %+v", c)
	}
}

func TestRefreshGrantPersistReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth-refresh-grants.json")
	secret := bytes.Repeat([]byte{0x42}, 32)

	first := NewServer("pw", "https://mcp.example.com", secret, 3600)
	if err := first.Registry.AddPreregistered("cli", []string{"http://127.0.0.1/cb"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := first.SetRefreshPersistPath(path); err != nil {
		t.Fatal(err)
	}
	refresh, err := first.IssueRefreshToken("cli", "https://mcp.example.com/mcp", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(refresh)) {
		t.Fatal("refresh token must not be persisted in plaintext")
	}
	second := NewServer("pw", "https://mcp.example.com", secret, 3600)
	if err := second.Registry.AddPreregistered("cli", []string{"http://127.0.0.1/cb"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := second.SetRefreshPersistPath(path); err != nil {
		t.Fatal(err)
	}
	tok, ttl, next, err := second.ExchangeRefreshToken(refresh, "cli", "")
	if err != nil || ttl <= 0 || tok == "" || next == "" {
		t.Fatalf("refresh after reload: err=%v ttl=%d", err, ttl)
	}
	if !second.ValidateAccessToken(tok, "https://mcp.example.com", "https://mcp.example.com/mcp") {
		t.Fatal("refreshed access token must validate after reload")
	}
	if _, _, _, err := second.ExchangeRefreshToken(refresh, "cli", ""); err == nil {
		t.Fatal("rotated refresh token must not survive reload as reusable")
	}
}
