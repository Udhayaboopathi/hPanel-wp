package security

import (
	"fmt"
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
	return joined, nil
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
