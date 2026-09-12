package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const refreshStoreVersion = 1

type refreshFileStore struct {
	Version int                     `json:"version"`
	Grants  []persistedRefreshGrant `json:"grants"`
}

type persistedRefreshGrant struct {
	TokenHash string    `json:"token_hash"`
	ClientID  string    `json:"client_id"`
	Resource  string    `json:"resource"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SetRefreshPersistPath enables durable refresh grants and loads any existing
// unexpired grants immediately. Only SHA-256 token hashes are persisted.
func (s *Server) SetRefreshPersistPath(path string) error {
	path = strings.TrimSpace(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshPersistPath = path
	if path == "" {
		return nil
	}
	return s.loadRefreshLocked()
}

func (s *Server) loadRefreshLocked() error {
	b, err := os.ReadFile(s.refreshPersistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var store refreshFileStore
	if err := json.Unmarshal(b, &store); err != nil {
		return fmt.Errorf("oauth refresh grants file: %w", err)
	}
	if store.Version != refreshStoreVersion {
		return fmt.Errorf("oauth refresh grants file version %d unsupported", store.Version)
	}

	now := time.Now()
	loaded := make(map[string]*refreshGrant, len(store.Grants))
	pruned := false
	for _, item := range store.Grants {
		if !validRefreshTokenHash(item.TokenHash) {
			return fmt.Errorf("oauth refresh grants file: invalid token hash")
		}
		if strings.TrimSpace(item.ClientID) == "" {
			return fmt.Errorf("oauth refresh grants file: missing client_id")
		}
		if !item.ExpiresAt.After(now) {
			pruned = true
			continue
		}
		if _, exists := loaded[item.TokenHash]; exists {
			return fmt.Errorf("oauth refresh grants file: duplicate token hash")
		}
		loaded[item.TokenHash] = &refreshGrant{
			ClientID:  item.ClientID,
			Resource:  item.Resource,
			Scope:     item.Scope,
			ExpiresAt: item.ExpiresAt,
		}
	}
	s.refresh = loaded
	if pruned {
		return s.saveRefreshLocked()
	}
	return nil
}

func (s *Server) saveRefreshLocked() error {
	if s.refreshPersistPath == "" {
		return nil
	}
	grants := make([]persistedRefreshGrant, 0, len(s.refresh))
	for tokenHash, grant := range s.refresh {
		if grant == nil {
			continue
		}
		grants = append(grants, persistedRefreshGrant{
			TokenHash: tokenHash,
			ClientID:  grant.ClientID,
			Resource:  grant.Resource,
			Scope:     grant.Scope,
			ExpiresAt: grant.ExpiresAt,
		})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].TokenHash < grants[j].TokenHash })
	b, err := json.MarshalIndent(refreshFileStore{Version: refreshStoreVersion, Grants: grants}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.refreshPersistPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := s.refreshPersistPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.refreshPersistPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(s.refreshPersistPath, 0o600)
}

func refreshTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validRefreshTokenHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func cloneRefreshGrants(src map[string]*refreshGrant) map[string]*refreshGrant {
	out := make(map[string]*refreshGrant, len(src))
	for key, grant := range src {
		if grant == nil {
			continue
		}
		copyGrant := *grant
		out[key] = &copyGrant
	}
	return out
}
