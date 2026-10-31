package backups

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateVerifyExtract(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(src, "wp-content/uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.php"), []byte("<?php // wp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "wp-content/uploads/a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(dir, "db.sql")
	if err := os.WriteFile(dump, []byte("CREATE TABLE t (i int);"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "backups", Filename("wp-test", time.Now()))
	sha, size, err := CreateTarGz(src, dump, dest)
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" || size == 0 {
		t.Fatal("expected sha and size")
	}
	ok, got, err := Verify(dest, sha)
	if err != nil || !ok || got != sha {
		t.Fatalf("verify failed ok=%v got=%q err=%v", ok, got, err)
	}
	out := filepath.Join(dir, "restore")
	if err := ExtractTarGz(dest, out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "wp-content/uploads/a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored file mismatch: %q %v", b, err)
	}
}

func TestRetentionNeverDeletesLastReady(t *testing.T) {
	items := []BackupLike{
		{ID: 1, Status: "ready", CreatedAt: "2026-01-03"},
		{ID: 2, Status: "ready", CreatedAt: "2026-01-02"},
		{ID: 3, Status: "ready", CreatedAt: "2026-01-01"},
		{ID: 4, Status: "failed", CreatedAt: "2026-01-04"},
	}
	got := RetentionCandidates(items, 2)
	if len(got) != 1 || got[0].ID != 3 {
		t.Errorf("expected only oldest ready backup as candidate, got %+v", got)
	}
	if got := RetentionCandidates(items[:1], 0); len(got) != 0 {
		t.Errorf("last ready backup must never be a deletion candidate, got %+v", got)
	}
}

func TestBackupMetadataFilename(t *testing.T) {
	if got := Filename("wp-ab12", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); got != "wp-ab12-20260102-030405.tar.gz" {
		t.Errorf("unexpected filename %q", got)
	}
}
