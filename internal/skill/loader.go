package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"mcpx/internal/config"
	"mcpx/internal/logging"
)

// Manifest describes a skill package (skill.yaml or SKILL.md frontmatter).
type Manifest struct {
	Name            string         `yaml:"name"`
	Description     string         `yaml:"description"`
	Runtime         string         `yaml:"runtime"`
	Entry           string         `yaml:"entry"`
	Permissions     []string       `yaml:"permissions"`
	ArgumentsSchema map[string]any `yaml:"arguments_schema"`
	// Format: yaml | skill_md
	Format string `yaml:"-"`
}

// Skill is a discovered skill package.
type Skill struct {
	Manifest Manifest
	Dir      string
	Source   string // scan root that contained it
}

// LoadAll scans dirs for skill packages.
// Recognizes:
//   - <name>/skill.yaml  (MCPX executable skill)
//   - <name>/SKILL.md or skill.md (Agent/Superpowers doc skill with YAML frontmatter)
//
// First occurrence of a name wins (callers should pass high-priority dirs first).
func LoadAll(dirs []string, workspacePath string) []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, rawDir := range dirs {
		d, skip := resolveScanDir(rawDir, workspacePath)
		if skip {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			logging.Debug("skill scan skip", "dir", d, "err", err)
			continue
		}
		for _, e := range entries {
			// skip hidden dirs except we already are inside skills root
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := filepath.Join(d, e.Name())
			if !e.IsDir() {
				if e.Type()&os.ModeSymlink == 0 {
					continue
				}
				info, err := os.Stat(dir)
				if err != nil || !info.IsDir() {
					continue
				}
			}
			m, ok := loadManifest(dir, e.Name())
			if !ok {
				continue
			}
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, Skill{Manifest: m, Dir: dir, Source: d})
		}
	}
	return out
}

// resolveScanDir expands ~ and maps relative .skills to workspace.
// Returns skip=true when .skills is listed but no workspace is bound.
func resolveScanDir(raw, workspacePath string) (string, bool) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return "", true
	}
	d = config.ExpandHome(d)
	base := filepath.Base(d)
	// bare ".skills" or "./.skills"
	if d == ".skills" || base == ".skills" && !filepath.IsAbs(raw) && !strings.HasPrefix(raw, "~") {
		if workspacePath == "" {
			return "", true
		}
		return filepath.Join(workspacePath, ".skills"), false
	}
	return d, false
}

func loadManifest(dir, fallbackName string) (Manifest, bool) {
	// 1) MCPX skill.yaml
	if raw, err := os.ReadFile(filepath.Join(dir, "skill.yaml")); err == nil {
		var m Manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			logging.Warn("skill.yaml parse", "path", dir, "err", err)
			return Manifest{}, false
		}
		m.Format = "yaml"
		normalizeManifest(&m, fallbackName)
		return acceptManifest(m, dir)
	}
	// 2) Agent SKILL.md / skill.md
	for _, name := range []string{"SKILL.md", "skill.md"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		m, ok := parseSkillMD(raw, fallbackName)
		if !ok {
			// still register by directory name as doc skill
			m = Manifest{
				Name:    fallbackName,
				Runtime: "markdown",
				Entry:   name,
				Format:  "skill_md",
			}
		}
		normalizeManifest(&m, fallbackName)
		if m.Format == "" {
			m.Format = "skill_md"
		}
		if m.Runtime == "" {
			m.Runtime = "markdown"
		}
		if m.Entry == "" {
			m.Entry = name
		}
		return acceptManifest(m, dir)
	}
	return Manifest{}, false
}

// acceptManifest rejects skills whose manifest entry would escape the skill
// directory. An unsafe entry is treated as an invalid package and skipped so
// read-only surfaces such as skill_tool describe can never expose files
// outside the skill directory.
func acceptManifest(m Manifest, dir string) (Manifest, bool) {
	if err := entryWithinDir(dir, m.Entry); err != nil {
		logging.Warn("skill entry rejected", "dir", dir, "entry", m.Entry, "err", err)
		return Manifest{}, false
	}
	return m, true
}

func normalizeManifest(m *Manifest, fallbackName string) {
	if m.Name == "" {
		m.Name = fallbackName
	}
	if m.Format == "yaml" {
		if m.Entry == "" {
			m.Entry = "main.py"
		}
		if m.Runtime == "" {
			m.Runtime = "python"
		}
	}
}

// parseSkillMD reads optional YAML frontmatter between --- lines.
func parseSkillMD(raw []byte, fallbackName string) (Manifest, bool) {
	text := string(raw)
	if !strings.HasPrefix(strings.TrimSpace(text), "---") {
		return Manifest{Name: fallbackName, Runtime: "markdown", Format: "skill_md"}, true
	}
	// find frontmatter
	rest := strings.TrimSpace(text)
	rest = strings.TrimPrefix(rest, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Manifest{}, false
	}
	fm := strings.TrimSpace(rest[:end])
	var m Manifest
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		logging.Debug("skill.md frontmatter", "err", err)
		return Manifest{Name: fallbackName, Runtime: "markdown", Format: "skill_md"}, true
	}
	m.Format = "skill_md"
	if m.Runtime == "" {
		m.Runtime = "markdown"
	}
	return m, true
}

// Find by name.
func Find(skills []Skill, name string) (Skill, bool) {
	for _, s := range skills {
		if s.Manifest.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// defaultEntry returns the conventional entry name for a skill that does not
// declare one.
func defaultEntry(sk Skill) string {
	if sk.Manifest.Runtime == "markdown" || sk.Manifest.Format == "skill_md" {
		return "SKILL.md"
	}
	return "main.py"
}

// entryWithinDir validates that a manifest-controlled entry cannot escape its
// skill directory. It rejects absolute paths, lexical ".." traversal, and
// existing symlinks whose target resolves outside the directory. A missing
// file passes the lexical check so skills that are still being authored are
// not dropped.
func entryWithinDir(dir, entry string) error {
	if entry == "" {
		return nil
	}
	if filepath.IsAbs(entry) {
		return fmt.Errorf("entry %q must be relative to the skill directory", entry)
	}
	clean := filepath.Clean(entry)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %q escapes the skill directory", entry)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, clean))
	if err != nil {
		// The file does not exist yet; the lexical check above is the best
		// containment guarantee available.
		return nil
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %q escapes the skill directory through a symlink", entry)
	}
	return nil
}

// ResolveEntry returns the absolute path of a skill's configured entry,
// guaranteeing the entry stays inside the skill directory (lexically and
// through symlinks).
func ResolveEntry(sk Skill) (string, error) {
	return ResolveEntryName(sk, sk.Manifest.Entry)
}

// ResolveEntryName resolves an explicit entry candidate (used for the
// SKILL.md / skill.md fallback) with the same containment guarantees as
// ResolveEntry.
func ResolveEntryName(sk Skill, entry string) (string, error) {
	if entry == "" {
		entry = defaultEntry(sk)
	}
	if err := entryWithinDir(sk.Dir, entry); err != nil {
		return "", err
	}
	return filepath.Join(sk.Dir, filepath.Clean(entry)), nil
}

// SafeEntry returns the cleaned, containment-checked entry relative to the
// skill directory. Shell-based runtimes use it to build the entry command.
func SafeEntry(sk Skill) (string, error) {
	entry := sk.Manifest.Entry
	if entry == "" {
		entry = defaultEntry(sk)
	}
	if err := entryWithinDir(sk.Dir, entry); err != nil {
		return "", err
	}
	return filepath.Clean(entry), nil
}

// DefaultScanDirs returns the default skill search paths (for logging/docs).
func DefaultScanDirs() []string {
	return []string{
		"~/.mcpx/skills",
		"~/.agents/skills",
		"~/.agent/skills",
		"~/.codex/skills",
		"~/.grok/skills",
		".skills",
	}
}
