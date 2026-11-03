package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/provisioning"
	"github.com/Udhayaboopathi/hPanel-wp/internal/secrets"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// A domain change must do the whole job in one action: the record
// moves, a fresh configuration is generated under the new domain,
// and a single apply_config job is queued carrying the old domain
// (the agent rewrites the site's internal URLs from it).
func TestSiteChangeDomainFullFlow(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secrets.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.Encrypt("db-password-for-test-123")
	if err != nil {
		t.Fatal(err)
	}
	site := &store.Website{
		SiteID: "wp-test0001", Name: "School", Domain: "cloud.example.com",
		AdminDomain: "wpadmin.cloud.example.com", Title: "School", Status: "ready",
		PHPVersion: "8.3", ResourceProfile: "standard", CPULimit: 1, MemLimitMB: 768,
		ContainerName: "own-hpanel-wp-test0001", DBName: "wp_test0001_db", DBUser: "wp_test0001_usr",
		DBPasswordEnc: enc, TablePrefix: "fc7_", AdminUsername: "chef",
		AdminEmail: "a@b.co", SiteDir: filepath.Join(dir, "wp-test0001"),
	}
	if _, err := st.CreateWebsite(site); err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:   Config{SitesDir: dir, SiteDBHost: "wordpress-mariadb"},
		store: st, box: box,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		runner: provisioning.New(st, nil),
	}

	form := url.Values{"new_domain": {"school.example.org"}}
	req := httptest.NewRequest("POST", "/websites/wp-test0001/change-domain", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("site", "wp-test0001")
	ctx := context.WithValue(req.Context(), ctxAdmin, &store.Admin{Username: "root"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.siteChangeDomain(rec, req)

	if rec.Code != 303 {
		t.Fatalf("status = %d, want 303 (body %.200s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Errorf("redirect = %q, want the apply_config job page", loc)
	}
	got, err := st.GetWebsiteByDomain("school.example.org")
	if err != nil {
		t.Fatalf("site not found under the new domain: %v", err)
	}
	if got.AdminDomain != "wpadmin.school.example.org" {
		t.Errorf("admin domain = %q", got.AdminDomain)
	}
	latest, err := st.LatestConfig(got.ID)
	if err != nil {
		t.Fatalf("no config version saved for the new domain: %v", err)
	}
	if !strings.Contains(latest.ComposeYAML, "school.example.org") {
		t.Error("generated compose does not route the new domain")
	}
	if strings.Contains(latest.ComposeYAML, "cloud.example.com") {
		t.Error("generated compose still routes the old domain")
	}
}
