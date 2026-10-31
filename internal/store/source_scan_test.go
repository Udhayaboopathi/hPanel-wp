package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Regression guard for the 2026-10-31 production outage: a stray
// argument passed to a placeholder-free Exec statement
// (`UPDATE backups ... `, Now()) is silently tolerated by the
// SQLite driver but rejected by MySQL ("sql: expected 0 arguments,
// got 1"), which crash-looped the panel at "open store" on the
// production MariaDB store while every local test passed. Local
// tests cannot exercise the MySQL driver, so this test statically
// forbids the pattern in this package: an Exec/ExecContext call
// whose SQL literal contains no '?' must not pass extra arguments.
func TestNoZeroPlaceholderExecWithArgs(t *testing.T) {
	backtick := regexp.MustCompile(`(?s)\.Exec(Context)?\(\s*` + "`([^`]*)`" + `\s*,`)
	quoted := regexp.MustCompile(`\.Exec(Context)?\(\s*"([^"]*)"\s*,`)
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		for _, re := range []*regexp.Regexp{backtick, quoted} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				if !strings.Contains(m[2], "?") {
					t.Errorf("%s: Exec with arguments but no placeholder in SQL: %.60q", f, m[2])
				}
			}
		}
	}
}
