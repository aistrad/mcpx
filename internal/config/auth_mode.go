package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ValidateSecurityRules rejects malformed regular expressions instead of
// silently skipping them during policy matching. A typo in a deny rule must
// never weaken the configured security boundary.
func ValidateSecurityRules(s SecurityConfig) error {
	groups := []struct {
		name     string
		patterns []string
	}{
		{"security.commands.allow", s.Commands.Allow},
		{"security.commands.confirm", s.Commands.Confirm},
		{"security.commands.deny", s.Commands.Deny},
		{"security.files.allow", s.Files.Allow},
		{"security.files.confirm", s.Files.Confirm},
		{"security.files.deny", s.Files.Deny},
	}
	for _, group := range groups {
		for i, pattern := range group.patterns {
			if pattern == "" {
				continue
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("%s[%d] invalid regexp %q: %w", group.name, i, pattern, err)
			}
		}
	}
	return nil
}

// EffectiveAuthMode resolves auth.mode with backward-compatible defaults.
func EffectiveAuthMode(a AuthConfig) string {
	m := strings.ToLower(strings.TrimSpace(a.Mode))
	switch m {
	case "open", "bearer", "oauth", "dual":
		return m
	case "":
		if strings.TrimSpace(a.Token) != "" {
			return "bearer"
		}
		return "open"
	default:
		return m
	}
}

// ValidateAuthMode returns error if mode is set to an unknown value.
func ValidateAuthMode(a AuthConfig) error {
	m := strings.ToLower(strings.TrimSpace(a.Mode))
	if m == "" {
		return nil
	}
	switch m {
	case "open", "bearer", "oauth", "dual":
		return nil
	default:
		return fmt.Errorf("auth.mode must be open|bearer|oauth|dual, got %q", a.Mode)
	}
}

// TransportSessionIdleTTL parses transport.session_idle_ttl; default 24h.
func TransportSessionIdleTTL(transport TransportConfig) time.Duration {
	if strings.TrimSpace(transport.SessionIdleTTL) == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(transport.SessionIdleTTL)
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// MaxResultBytes returns tool output cap; default 200000.
func MaxResultBytes(l LimitsConfig) int {
	if l.MaxResultBytes <= 0 {
		return 200_000
	}
	return l.MaxResultBytes
}

// OAuthTokenTTL seconds; default 86400; clamped 60..604800.
func OAuthTokenTTL(o OAuthConfig) int {
	ttl := o.TokenTTL
	if ttl <= 0 {
		ttl = 86400
	}
	if ttl < 60 {
		ttl = 60
	}
	if ttl > 604800 {
		ttl = 604800
	}
	return ttl
}
