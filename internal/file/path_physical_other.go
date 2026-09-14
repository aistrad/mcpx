//go:build !windows

package file

import "path/filepath"

func physicalPath(path string) (string, error) { return filepath.EvalSymlinks(path) }
