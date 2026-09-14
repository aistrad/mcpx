package file

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve ensures an existing path is under workspace root after resolving
// symbolic links. Checking only the lexical path would allow a workspace link
// such as outside -> /etc to escape the boundary.
func Resolve(workspaceRoot, rel string) (abs string, err error) {
	if rel == "" {
		return "", fmt.Errorf("path required")
	}
	rootLexical, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	rootLexical = filepath.Clean(rootLexical)
	root, err := physicalPath(rootLexical)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	joined := filepath.Clean(filepath.Join(rootLexical, rel))
	if !withinRoot(rootLexical, joined) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	resolved, err := physicalPath(joined)
	if err != nil {
		if os.IsNotExist(err) {
			// Resolve the nearest existing ancestor. This preserves support for
			// future files while still detecting a symlinked parent escape.
			ancestor := filepath.Dir(joined)
			for {
				realParent, parentErr := physicalPath(ancestor)
				if parentErr == nil {
					if !withinRoot(root, realParent) {
						return "", fmt.Errorf("path escapes workspace: %s", rel)
					}
					suffix, suffixErr := filepath.Rel(ancestor, joined)
					if suffixErr != nil {
						return "", suffixErr
					}
					return filepath.Join(realParent, suffix), nil
				}
				parent := filepath.Dir(ancestor)
				if parent == ancestor {
					return "", parentErr
				}
				ancestor = parent
			}
		}
		return "", err
	}
	if !withinRoot(root, resolved) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return resolved, nil
}

// LexicalPath returns the cleaned absolute path without following the final
// symbolic link. Destructive callers should use it for Lstat, then use Resolve
// separately to validate the resolved target remains inside the workspace.
func LexicalPath(workspaceRoot, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("path required")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(root, rel))
	if !withinRoot(root, joined) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return joined, nil
}

func withinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Rel returns path relative to workspace for display.
func Rel(workspaceRoot, abs string) (string, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}
	return filepath.Rel(root, abs)
}
