package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoin joins rel under root and guarantees the result stays inside root,
// rejecting absolute paths, ".." traversal and empty results.
func SafeJoin(root, rel string) (string, error) {
	if rel == "" {
		return filepath.Clean(root), nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(cleanRoot, rel)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the site directory", rel)
	}
	// Symlink resolution: the lexical check alone is not enough —
	// a symlink planted inside the root (a WordPress plugin can
	// create one under uploads) would let a later open/read/write
	// escape to anywhere on the filesystem. Resolve the deepest
	// existing ancestor of the target and re-verify containment;
	// not-yet-existing trailing components are re-appended so
	// creating new files still works.
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		resolvedRoot = cleanRoot // root does not exist yet; lexical result stands
	}
	target, rest := joined, ""
	for {
		if _, err := os.Lstat(target); err == nil {
			resolved, err := filepath.EvalSymlinks(target)
			if err != nil {
				return "", err
			}
			final := resolved
			if rest != "" {
				final = filepath.Join(resolved, rest)
			}
			if final != resolvedRoot && !strings.HasPrefix(final, resolvedRoot+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q escapes the site directory through a symbolic link", rel)
			}
			return final, nil
		}
		parent := filepath.Dir(target)
		if parent == target {
			return joined, nil
		}
		if rest == "" {
			rest = filepath.Base(target)
		} else {
			rest = filepath.Join(filepath.Base(target), rest)
		}
		target = parent
	}
}

// IsSensitiveFile reports files hidden by default in the file manager.
func IsSensitiveFile(name string) bool {
	base := filepath.Base(name)
	switch base {
	case ".env", "wp-config.php", "compose.yaml", "docker-compose.yaml", "docker-compose.yml":
		return true
	}
	return strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem")
}

// EditableTextFile allows editing only small text-like files.
func EditableTextFile(name string, size int64) bool {
	if size > 1<<20 {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".php", ".txt", ".md", ".css", ".js", ".html", ".htm", ".json", ".xml", ".ini", ".htaccess", "":
		return true
	}
	return filepath.Base(name) == ".htaccess"
}
