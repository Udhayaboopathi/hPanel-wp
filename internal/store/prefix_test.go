package store

import (
	"path/filepath"
	"testing"
)

// Migration 002 (websites.table_prefix) must apply on first open,
// survive reopens, and round-trip through the website CRUD.
func TestTablePrefixMigrationAndRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w := &Website{
		SiteID: "wp-prefix01", Name: "Imported", Domain: "imported.example.com",
		AdminDomain: "wpadmin.imported.example.com", Title: "Imported", Status: "provisioning",
		PHPVersion: "8.3", ResourceProfile: "standard", CPULimit: 1, MemLimitMB: 768,
		ContainerName: "own-hpanel-wp-prefix01", DBName: "wp_prefix01_db", DBUser: "wp_prefix01_usr",
		DBPasswordEnc: "enc", TablePrefix: "fc7_", AdminUsername: "chef",
		AdminEmail: "a@b.co", SiteDir: "/tmp/wp-prefix01",
	}
	if _, err := s.CreateWebsite(w); err != nil {
		t.Fatalf("create website: %v", err)
	}
	got, err := s.GetWebsiteBySiteID("wp-prefix01")
	if err != nil {
		t.Fatal(err)
	}
	if got.TablePrefix != "fc7_" {
		t.Fatalf("prefix roundtrip: got %q want fc7_", got.TablePrefix)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen: the column probe must find the column and skip the DDL.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got2, err := s2.GetWebsiteBySiteID("wp-prefix01")
	if err != nil {
		t.Fatal(err)
	}
	if got2.TablePrefix != "fc7_" {
		t.Fatalf("prefix after reopen: got %q want fc7_", got2.TablePrefix)
	}
	// Default prefix for native sites.
	w2 := *w
	w2.SiteID = "wp-native01"
	w2.Domain = "native.example.com"
	w2.AdminDomain = "wpadmin.native.example.com"
	w2.DBName = "wp_native01_db"
	w2.DBUser = "wp_native01_usr"
	w2.TablePrefix = ""
	if _, err := s2.CreateWebsite(&w2); err != nil {
		t.Fatalf("create native website: %v", err)
	}
	got3, err := s2.GetWebsiteBySiteID("wp-native01")
	if err != nil {
		t.Fatal(err)
	}
	if got3.TablePrefix != "wp_" {
		t.Fatalf("native site prefix: got %q want wp_", got3.TablePrefix)
	}
}
