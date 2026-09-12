package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"

	"mcpx/internal/logging"
)

const maxOAuthBody = 8192

// Handler serves OAuth HTTP endpoints.
type Handler struct {
	S *Server
}

// OriginFromRequest builds scheme://host from the request.
func OriginFromRequest(r *http.Request, trustProxy bool) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if trustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = strings.Split(p, ",")[0]
			scheme = strings.TrimSpace(scheme)
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return scheme + "://" + host
}

// HandleProtectedResourceMetadata RFC9728.
func (h *Handler) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	origin := h.S.EffectiveIssuer(OriginFromRequest(r, false))
	resource := h.S.ResourceURL(origin)
	body := map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{origin},
		"scopes_supported":         []string{DefaultScope},
		"bearer_methods_supported": []string{"header"},
	}
	writeJSON(w, http.StatusOK, body)
}

// OAuth path prefix under /mcp — preferred for reverse proxies that only
// forward the MCP path tree cleanly (Cloudflare/Caddy path rules).
const MCPOAuthPrefix = "/mcp/oauth"

// HandleAuthorizationServerMetadata RFC8414.
// Endpoints are advertised under /mcp/oauth/* so DCR/authorize/token share the
// same public path prefix as the MCP resource (helps 内网穿透 / CDN allowlists).
func (h *Handler) HandleAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	origin := h.S.EffectiveIssuer(OriginFromRequest(r, false))
	body := map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                origin + MCPOAuthPrefix + "/authorize",
		"token_endpoint":                        origin + MCPOAuthPrefix + "/token",
		"registration_endpoint":                 origin + MCPOAuthPrefix + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{DefaultScope},
		// Explicit capability flag some clients check before attempting DCR.
		"registration_endpoint_auth_methods_supported": []string{"none"},
		// OpenAI ChatGPT / MCP clients prefer CIMD when advertised (client_id is an HTTPS URL).
		"client_id_metadata_document_supported": true,
	}
	writeJSON(w, http.StatusOK, body)
}

// HandleRegister handles POST /mcp/oauth/register.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBody))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "cannot read body")
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	out, err := h.S.Registry.Register(meta)
	if err != nil {
		logging.L().Info("oauth register",
			"component", "oauth", "error", err.Error())
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	cid, _ := out["client_id"].(string)
	nURIs := 0
	if uris, ok := out["redirect_uris"].([]string); ok {
		nURIs = len(uris)
	}
	logging.L().Info("oauth register",
		"component", "oauth", "client_id", cid,
		"redirect_uris_count", nURIs, "ok", true)
	writeJSON(w, http.StatusCreated, out)
}

// HandleAuthorize handles GET/POST /mcp/oauth/authorize.
func (h *Handler) HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.authorizeGet(w, r)
	case http.MethodPost:
		h.authorizePost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) authorizeGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	state := q.Get("state")
	resource := q.Get("resource")
	scope := q.Get("scope")
	if scope == "" {
		scope = DefaultScope
	}
	if err := h.validateAuthorizeParams(clientID, redirectURI, challenge, method); err != nil {
		logging.L().Info("oauth authorize",
			"component", "oauth", "method", "GET", "client_id", clientID,
			"redirect_uri", redirectURI, "has_state", q.Get("state") != "",
			"state_len", len(q.Get("state")), "resource", resource,
			"scope", scope, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logging.L().Info("oauth authorize",
		"component", "oauth", "method", "GET", "client_id", clientID,
		"redirect_uri", redirectURI, "has_state", state != "",
		"state_len", len(state), "resource", resource,
		"scope", scope, "ok", true)
	c, _ := h.S.ResolveClient(clientID)
	name := clientID
	if c != nil && c.ClientName != "" {
		name = c.ClientName
	}
	formAction := r.URL.Path
	if formAction == "" {
		formAction = MCPOAuthPrefix + "/authorize"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, authorizeHTML,
		html.EscapeString(name),
		html.EscapeString(redirectURI),
		html.EscapeString(scope),
		html.EscapeString(formAction),
		html.EscapeString(clientID),
		html.EscapeString(redirectURI),
		html.EscapeString(challenge),
		html.EscapeString(method),
		html.EscapeString(state),
		html.EscapeString(resource),
		html.EscapeString(scope),
	)
}

func (h *Handler) authorizePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	challenge := r.FormValue("code_challenge")
	method := r.FormValue("code_challenge_method")
	state := r.FormValue("state")
	resource := r.FormValue("resource")
	scope := r.FormValue("scope")
	if err := h.validateAuthorizeParams(clientID, redirectURI, challenge, method); err != nil {
		logging.L().Info("oauth authorize",
			"component", "oauth", "method", "POST", "client_id", clientID,
			"redirect_uri", redirectURI, "has_state", state != "",
			"state_len", len(state), "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	passwordOK := h.S.CheckPassword(password)
	if !passwordOK {
		logging.L().Info("oauth authorize",
			"component", "oauth", "method", "POST", "client_id", clientID,
			"redirect_uri", redirectURI, "has_state", state != "",
			"state_len", len(state), "password_ok", false)
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	logging.L().Info("oauth authorize",
		"component", "oauth", "method", "POST", "client_id", clientID,
		"redirect_uri", redirectURI, "has_state", state != "",
		"state_len", len(state), "password_ok", true)
	if resource == "" {
		origin := h.S.EffectiveIssuer(OriginFromRequest(r, false))
		resource = h.S.ResourceURL(origin)
	}
	code, err := h.S.IssueCode(clientID, redirectURI, challenge, method, resource, scope)
	if err != nil {
		logging.L().Info("oauth authorize",
			"component", "oauth", "method", "POST", "client_id", clientID,
			"redirect_uri", redirectURI, "has_state", state != "",
			"state_len", len(state), "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	logging.L().Info("oauth authorize redirect",
		"component", "oauth", "client_id", clientID,
		"redirect_target", u.String(), "has_state", state != "",
		"state_len", len(state), "code_len", len(code))
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *Handler) validateAuthorizeParams(clientID, redirectURI, challenge, method string) error {
	if clientID == "" || redirectURI == "" || challenge == "" {
		return fmt.Errorf("missing client_id, redirect_uri, or code_challenge")
	}
	if method != "" && method != "S256" {
		return fmt.Errorf("code_challenge_method must be S256")
	}
	if !h.S.AcceptsClientRedirect(clientID, redirectURI) {
		return fmt.Errorf("unknown client or redirect_uri")
	}
	if !ValidChallenge(challenge) {
		return fmt.Errorf("invalid code_challenge")
	}
	return nil
}

// HandleToken handles POST /mcp/oauth/token.
func (h *Handler) HandleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	grant := r.FormValue("grant_type")
	if grant != "authorization_code" && grant != "refresh_token" {
		logging.L().Info("oauth token",
			"component", "oauth", "grant_type", grant,
			"error", "unsupported grant type")
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code or refresh_token")
		return
	}
	clientID, clientSecret, authMethod := h.clientAuth(r)
	if clientID == "" {
		logging.L().Info("oauth token",
			"component", "oauth", "error", "missing client_id")
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "missing client_id")
		return
	}
	if !h.S.AuthenticatesClient(clientID, clientSecret, authMethod) {
		// try registered method from DCR/CIMD when basic/post mismatch
		c, err := h.S.ResolveClient(clientID)
		if err != nil || c == nil {
			logging.L().Info("oauth token",
				"component", "oauth", "client_id", clientID,
				"auth_method", authMethod, "error", "unknown client")
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
			return
		}
		if !h.S.AuthenticatesClient(clientID, clientSecret, c.TokenEndpointAuthMethod) {
			logging.L().Info("oauth token",
				"component", "oauth", "client_id", clientID,
				"auth_method", authMethod, "error", "client authentication failed")
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}
	}
	if grant == "refresh_token" {
		refreshToken := r.FormValue("refresh_token")
		resource := r.FormValue("resource")
		tok, ttl, nextRefresh, err := h.S.ExchangeRefreshToken(refreshToken, clientID, resource)
		if err != nil {
			logging.L().Info("oauth token",
				"component", "oauth", "client_id", clientID,
				"auth_method", authMethod, "grant_type", "refresh_token",
				"error", err.Error())
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
			return
		}
		logging.L().Info("oauth token",
			"component", "oauth", "client_id", clientID,
			"auth_method", authMethod, "grant_type", "refresh_token", "ok", true)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  tok,
			"token_type":    "Bearer",
			"expires_in":    ttl,
			"scope":         DefaultScope,
			"refresh_token": nextRefresh,
		})
		return
	}
	code := r.FormValue("code")
	redirectURI := r.FormValue("redirect_uri")
	verifier := r.FormValue("code_verifier")
	resource := r.FormValue("resource")
	tok, ttl, err := h.S.ExchangeCode(code, redirectURI, clientID, verifier, resource)
	if err != nil {
		logging.L().Info("oauth token",
			"component", "oauth", "client_id", clientID,
			"auth_method", authMethod, "error", err.Error())
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	logging.L().Info("oauth token",
		"component", "oauth", "client_id", clientID,
		"auth_method", authMethod, "ok", true)
	res := resource
	if res == "" {
		res = h.S.ResourceURL(h.S.EffectiveIssuer(OriginFromRequest(r, false)))
	}
	refreshToken, err := h.S.IssueRefreshToken(clientID, res, DefaultScope)
	if err != nil {
		logging.L().Error("oauth token",
			"component", "oauth", "client_id", clientID,
			"auth_method", authMethod, "error", err.Error())
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to persist refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  tok,
		"token_type":    "Bearer",
		"expires_in":    ttl,
		"scope":         DefaultScope,
		"refresh_token": refreshToken,
	})
}

func (h *Handler) clientAuth(r *http.Request) (clientID, clientSecret, method string) {
	clientID = r.FormValue("client_id")
	clientSecret = r.FormValue("client_secret")
	if clientSecret != "" {
		return clientID, clientSecret, "client_secret_post"
	}
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "basic ") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authz[6:]))
		if err == nil {
			parts := strings.SplitN(string(raw), ":", 2)
			if len(parts) == 2 {
				id, _ := url.QueryUnescape(parts[0])
				sec, _ := url.QueryUnescape(parts[1])
				return id, sec, "client_secret_basic"
			}
		}
	}
	if clientID != "" {
		return clientID, "", "none"
	}
	return "", "", ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

const authorizeHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>MCPX 授权</title>
<style>
body{font-family:system-ui,sans-serif;max-width:28rem;margin:2rem auto;padding:0 1rem;line-height:1.5}
label{display:block;margin:.75rem 0 .25rem}
input[type=password]{width:100%%;padding:.5rem;box-sizing:border-box}
button{margin-top:1rem;padding:.5rem 1rem;width:100%%}
.meta{color:#555;font-size:.9rem;word-break:break-all}
</style>
</head>
<body>
<h1>MCPX 授权</h1>
<p>客户端 <strong>%s</strong> 请求访问本 Runtime。</p>
<p class="meta">回调：%s<br/>权限：%s</p>
<form method="POST" action="%s">
<label for="password">运维口令</label>
<input id="password" name="password" type="password" required autocomplete="current-password"/>
<input type="hidden" name="client_id" value="%s"/>
<input type="hidden" name="redirect_uri" value="%s"/>
<input type="hidden" name="code_challenge" value="%s"/>
<input type="hidden" name="code_challenge_method" value="%s"/>
<input type="hidden" name="state" value="%s"/>
<input type="hidden" name="resource" value="%s"/>
<input type="hidden" name="scope" value="%s"/>
<button type="submit">授权</button>
</form>
</body>
</html>
`
