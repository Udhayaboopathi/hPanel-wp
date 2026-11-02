package wpimport

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindContentRoot(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  string
	}{
		{"bare wp-content", []string{"plugins/a/a.php", "themes/t/style.css", "uploads/x.jpg"}, ""},
		{"nested once", []string{"backup/plugins/a.php", "backup/themes/t/style.css"}, "backup/"},
		{"drive double nest", []string{"First Crops/First Crops/plugins/a.php", "First Crops/First Crops/themes/t/style.css", "First Crops/First Crops/uploads/2025/01/x.jpg"}, "First Crops/First Crops/"},
		{"full site zip", []string{"wp-admin/index.php", "wp-content/plugins/a.php", "wp-content/themes/t/style.css", "wp-config.php"}, "wp-content/"},
	}
	for _, c := range cases {
		got, err := FindContentRoot(c.names)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if _, err := FindContentRoot([]string{"readme.txt", "images/x.png"}); err == nil {
		t.Error("expected error when no wp-content tree exists")
	}
}

func TestFindDBEntry(t *testing.T) {
	names := []string{
		"backup/plugins/a.php",
		"backup/backup_2026-10-31-0813_Site_243d0f49addc-db",
		"backup/themes/t/style.css",
		"other/export.sql",
	}
	if got := FindDBEntry(names, "backup/"); got != "backup/backup_2026-10-31-0813_Site_243d0f49addc-db" {
		t.Errorf("got %q, want the in-root WPvivid dump", got)
	}
	if got := FindDBEntry([]string{"a/b.php"}, ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestIsDBBackupName(t *testing.T) {
	yes := []string{"backup_2026-10-31-0813_X_243d0f49addc-db", "dump.sql", "dump.sql.gz", "DB.SQL.GZ", "x-db.gz"}
	no := []string{"index.php", "db.sqlite", "backup.zip", "readme-db.txt"}
	for _, n := range yes {
		if !IsDBBackupName(n) {
			t.Errorf("%q should be a DB backup name", n)
		}
	}
	for _, n := range no {
		if IsDBBackupName(n) {
			t.Errorf("%q should NOT be a DB backup name", n)
		}
	}
}

func TestDetectPrefixAndSiteURL(t *testing.T) {
	dump := "-- WPvivid dump\n" +
		"CREATE TABLE `wp_options` (\n  `option_id` bigint unsigned NOT NULL AUTO_INCREMENT\n);\n" +
		"INSERT INTO `wp_options` VALUES (1,'siteurl','https://firstcrops.example.com','yes'),(2,'home','https://firstcrops.example.com','yes');\n"
	prefix, siteURL, err := DetectPrefixAndSiteURL(strings.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "wp_" || siteURL != "https://firstcrops.example.com" {
		t.Errorf("got prefix=%q siteURL=%q", prefix, siteURL)
	}

	custom := "CREATE TABLE `fc7_options` (`option_id` bigint);\nINSERT INTO `fc7_options` VALUES (1,'siteurl','http://old.example.org','yes');\n"
	prefix, siteURL, err = DetectPrefixAndSiteURL(strings.NewReader(custom))
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "fc7_" || siteURL != "http://old.example.org" {
		t.Errorf("custom: got prefix=%q siteURL=%q", prefix, siteURL)
	}

	prefix, siteURL, err = DetectPrefixAndSiteURL(strings.NewReader("SELECT 1;\n"))
	if err != nil || prefix != "" || siteURL != "" {
		t.Errorf("empty dump: got %q %q %v", prefix, siteURL, err)
	}
}

func TestExtractContent(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "site.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	files := map[string]string{
		"wp-content/plugins/akismet/akismet.php":      "<?php // plugin",
		"wp-content/themes/oceanwp/style.css":         "/* theme */",
		"wp-content/uploads/2025/01/photo.jpg":        "jpeg-bytes",
		"wp-content/mu-plugins/mu.php":                "<?php // mu",
		"wp-content/litespeed/debug/log.txt":          "skip me",
		"wp-content/wpvivid_uploads/backup.zip":       "skip me too",
		"wp-content/backup_2026-01-01-0000_X_abcd-db": "not sql, a marker",
		"wp-content/index.php":                        "<?php // silence",
		"wp-admin/index.php":                          "<?php // outside root",
		"wp-config.php":                               "<?php // outside root",
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dest := filepath.Join(dir, "wp-content")
	stats, err := ExtractContent(zipPath, dest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 5 {
		t.Errorf("extracted %d files, want 5 (stats %+v)", stats.Files, stats)
	}
	for _, want := range []string{"plugins/akismet/akismet.php", "themes/oceanwp/style.css", "uploads/2025/01/photo.jpg", "mu-plugins/mu.php", "index.php"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("missing extracted file %s", want)
		}
	}
	for _, unwanted := range []string{"litespeed", "wpvivid_uploads", "wp-admin"} {
		if _, err := os.Stat(filepath.Join(dest, unwanted)); err == nil {
			t.Errorf("should not have extracted %s", unwanted)
		}
	}
}
