package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/provisioning"
	"github.com/Udhayaboopathi/hPanel-wp/internal/secrets"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/web"
)

// The one-upload import handler, end to end at the HTTP boundary:
// a WPvivid-shaped ZIP (wp-content tree + the plugin's *-db file,
// gzipped SQL with a custom table prefix and the old siteurl) must
// be staged into the new site's directory, analysed, recorded with
// the detected prefix, and queued as an import_site job.
func TestImportSubmitWPvividLayout(t *testing.T) {
	// The handler applies the same host-capacity guard as site
	// creation (refuse when < 512 MB RAM is available). On a small
	// or busy test machine that guard legitimately fires, which is
	// not what this test is about - skip there instead of failing.
	if host := monitoring.Sample("/"); host.MemTotal > 0 && host.MemAvail < 512<<20 {
		t.Skipf("host has only %d MB RAM available; the capacity guard would refuse the import on this machine", host.MemAvail>>20)
	}
	dir := t.TempDir()
	sitesDir := filepath.Join(dir, "websites")
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secrets.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{
		"list":       func(items ...string) []string { return items },
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"humanBytes": humanBytes,
		"pct": func(used, total uint64) float64 {
			if total == 0 {
				return 0
			}
			return float64(used) / float64(total) * 100
		},
		"since": func(ts string) string {
			tm, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			return tm.Local().Format("2006-01-02 15:04")
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:    Config{SitesDir: sitesDir, SiteDBHost: "wordpress-mariadb", ServerIP: "130.210.42.20"},
		store:  st,
		box:    box,
		tmpl:   tmpl,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		runner: provisioning.New(st, nil),
	}

	// Build the WPvivid-shaped ZIP.
	dump := "-- WPvivid database backup\n" +
		"CREATE TABLE `fc7_options` (`option_id` bigint);\n" +
		"INSERT INTO `fc7_options` VALUES (1,'siteurl','https://old.example.org','yes'),(2,'home','https://old.example.org','yes');\n" +
		"CREATE TABLE `fc7_posts` (`ID` bigint);\n"
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write([]byte(dump)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	zipFiles := map[string][]byte{
		"First Crops/plugins/akismet/akismet.php":          []byte("<?php"),
		"First Crops/themes/oceanwp/style.css":             []byte("/* t */"),
		"First Crops/uploads/2025/01/a.jpg":                []byte("jpg"),
		"First Crops/backup_2026-10-31-0813_X_hashhash-db": gzBuf.Bytes(),
		"First Crops/litespeed/debug/log.txt":              []byte("skip"),
	}
	for name, body := range zipFiles {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"domain": "imported.example.com", "title": "First Crops",
		"admin_user": "chef", "admin_email": "a@b.co",
		"admin_password": "a-very-long-password", "php_version": "8.3",
		"resource_profile": "standard",
	} {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("files_zip", "backup.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/websites/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := context.WithValue(req.Context(), ctxKey(2), &store.Session{CSRFToken: "tok"})
	ctx = context.WithValue(ctx, ctxAdmin, &store.Admin{Username: "root"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.importSubmit(rec, req)

	if rec.Code != 303 {
		body := rec.Body.String()
		i := strings.Index(body, "error-box")
		seg := ""
		if i >= 0 {
			seg = body[i:min(len(body), i+220)]
		}
		t.Fatalf("status = %d, want 303; error segment: %s", rec.Code, seg)
	}
	site, err := st.GetWebsiteByDomain("imported.example.com")
	if err != nil {
		t.Fatalf("website not recorded: %v", err)
	}
	if site.TablePrefix != "fc7_" {
		t.Errorf("site prefix = %q, want fc7_", site.TablePrefix)
	}
	staged := filepath.Join(sitesDir, site.SiteID, "import")
	if _, err := os.Stat(filepath.Join(staged, "upload.zip")); err != nil {
		t.Errorf("staged zip missing: %v", err)
	}
	sqlBytes, err := os.ReadFile(filepath.Join(staged, "db.sql"))
	if err != nil {
		t.Fatalf("staged db.sql missing: %v", err)
	}
	if !strings.Contains(string(sqlBytes), "fc7_options") {
		t.Error("db.sql was not normalised to the plain dump")
	}
	jobs, err := st.ListJobsForSite(site.SiteID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v, %v; want exactly one", jobs, err)
	}
	if jobs[0].Kind != "import_site" {
		t.Errorf("job kind = %q, want import_site", jobs[0].Kind)
	}
}

// WPvivid multi-part mode: an unknown file in the set, or a set
// missing a required part, must be rejected with a precise error
// before anything is created.
func TestImportSubmitPartsModeRejections(t *testing.T) {
	build := func(t *testing.T) (*App, string) {
		dir := t.TempDir()
		sitesDir := filepath.Join(dir, "websites")
		st, err := store.Open(filepath.Join(dir, "panel.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		box, err := secrets.LoadOrCreateKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		tmpl, err := template.New("").Funcs(template.FuncMap{
			"list":       func(items ...string) []string { return items },
			"add":        func(a, b int) int { return a + b },
			"sub":        func(a, b int) int { return a - b },
			"humanBytes": humanBytes,
			"pct":        func(used, total uint64) float64 { return 0 },
			"since":      func(ts string) string { return ts },
		}).ParseFS(web.FS, "templates/*.html")
		if err != nil {
			t.Fatal(err)
		}
		return &App{
			cfg:   Config{SitesDir: sitesDir, SiteDBHost: "wordpress-mariadb"},
			store: st, box: box, tmpl: tmpl,
			log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			runner: provisioning.New(st, nil),
		}, sitesDir
	}
	zipBytes := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range files {
			w, _ := zw.Create(name)
			_, _ = w.Write([]byte(body))
		}
		_ = zw.Close()
		return buf.Bytes()
	}
	post := func(t *testing.T, app *App, parts map[string][]byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("domain", "parts.example.com")
		_ = mw.WriteField("admin_user", "chef")
		_ = mw.WriteField("admin_email", "a@b.co")
		_ = mw.WriteField("admin_password", "a-very-long-password")
		for name, data := range parts {
			fw, _ := mw.CreateFormFile("files_zip", name)
			_, _ = fw.Write(data)
		}
		_ = mw.Close()
		req := httptest.NewRequest("POST", "/websites/import", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		ctx := context.WithValue(req.Context(), ctxKey(2), &store.Session{CSRFToken: "tok"})
		ctx = context.WithValue(ctx, ctxAdmin, &store.Admin{Username: "root"})
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		app.importSubmit(rec, req)
		return rec
	}

	plugins := zipBytes(map[string]string{"plugins/akismet/akismet.php": "<?php"})
	themes := zipBytes(map[string]string{"themes/oceanwp/style.css": "/* t */"})

	t.Run("unknown file in set", func(t *testing.T) {
		app, sitesDir := build(t)
		rec := post(t, app, map[string][]byte{
			"backup_x-plugins.zip": plugins,
			"holiday.mp4":          []byte("video"),
		})
		if !strings.Contains(rec.Body.String(), "not a WPvivid backup part") {
			t.Errorf("want part-classification error; body: %.300s", rec.Body.String())
		}
		if _, err := app.store.GetWebsiteByDomain("parts.example.com"); err == nil {
			t.Error("no website may be created")
		}
		entries, _ := os.ReadDir(sitesDir)
		if len(entries) != 0 {
			t.Errorf("staging not cleaned: %v", entries)
		}
	})

	t.Run("themes part missing", func(t *testing.T) {
		app, _ := build(t)
		rec := post(t, app, map[string][]byte{
			"backup_x-plugins.zip": plugins,
			"backup_x-uploads.zip": zipBytes(map[string]string{"uploads/a.jpg": "x"}),
		})
		_ = themes
		if !strings.Contains(rec.Body.String(), "incomplete") {
			t.Errorf("want incompleteness error; body: %.300s", rec.Body.String())
		}
	})
}

func newImportTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	sitesDir := filepath.Join(dir, "websites")
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secrets.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"list":       func(items ...string) []string { return items },
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"humanBytes": humanBytes,
		"pct":        func(used, total uint64) float64 { return 0 },
		"since":      func(ts string) string { return ts },
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		cfg:   Config{SitesDir: sitesDir, SiteDBHost: "wordpress-mariadb"},
		store: st, box: box, tmpl: tmpl,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		runner: provisioning.New(st, nil),
	}, sitesDir
}

func postImport(t *testing.T, app *App, fields map[string]string, zipData []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("files_zip", "site.zip")
	_, _ = fw.Write(zipData)
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/websites/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := context.WithValue(req.Context(), ctxKey(2), &store.Session{CSRFToken: "tok"})
	ctx = context.WithValue(ctx, ctxAdmin, &store.Admin{Username: "root"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.importSubmit(rec, req)
	return rec
}

// Filling only SOME administrator fields is a mistake the operator
// must hear about: all three, or none (keep the original admin).
func TestImportSubmitPartialAdminRejected(t *testing.T) {
	app, _ := newImportTestApp(t)
	rec := postImport(t, app, map[string]string{
		"domain":     "x.example.com",
		"admin_user": "chef",
	}, []byte("not even a zip"))
	if !strings.Contains(rec.Body.String(), "leave all three empty") {
		t.Errorf("want all-or-none admin error; body: %.300s", rec.Body.String())
	}
}

// With the domain left empty, the importer must resolve it from
// the backup's own database. Proof: the dump's siteurl points at
// a domain that already exists in the panel, so the import must
// stop at the collision check naming that domain.
func TestImportSubmitKeepsOriginalDomain(t *testing.T) {
	app, _ := newImportTestApp(t)
	existing := &store.Website{
		SiteID: "wp-existing1", Name: "Existing", Domain: "school.example.org",
		AdminDomain: "wpadmin.school.example.org", Title: "Existing", Status: "ready",
		PHPVersion: "8.3", ResourceProfile: "standard", CPULimit: 1, MemLimitMB: 768,
		ContainerName: "own-hpanel-wp-existing1", DBName: "wp_existing1_db", DBUser: "wp_existing1_usr",
		DBPasswordEnc: "enc", TablePrefix: "wp_", AdminUsername: "chef",
		AdminEmail: "a@b.co", SiteDir: "/tmp/wp-existing1",
	}
	if _, err := app.store.CreateWebsite(existing); err != nil {
		t.Fatal(err)
	}
	dump := "CREATE TABLE `fc7_options` (`option_id` bigint);\n" +
		"INSERT INTO `fc7_options` VALUES (1,'siteurl','https://school.example.org','yes');\n"
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for name, data := range map[string]string{
		"wp-content/plugins/akismet/akismet.php": "<?php",
		"wp-content/themes/oceanwp/style.css":    "/* t */",
		"wp-content/backup_2026-01-01_x-db":      dump,
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(data))
	}
	_ = zw.Close()
	// No domain, no admin fields at all.
	rec := postImport(t, app, map[string]string{}, zipBuf.Bytes())
	if !strings.Contains(rec.Body.String(), "school.example.org") || !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("want collision error naming the detected original domain; body: %.400s", rec.Body.String())
	}
}

// A ZIP without a wp-content tree must be rejected with a clear
// error and leave nothing behind.
func TestImportSubmitRejectsNonWordPressZip(t *testing.T) {
	dir := t.TempDir()
	sitesDir := filepath.Join(dir, "websites")
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secrets.LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"list":       func(items ...string) []string { return items },
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"humanBytes": humanBytes,
		"pct":        func(used, total uint64) float64 { return 0 },
		"since":      func(ts string) string { return ts },
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:   Config{SitesDir: sitesDir, SiteDBHost: "wordpress-mariadb"},
		store: st, box: box, tmpl: tmpl,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		runner: provisioning.New(st, nil),
	}
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("readme.txt")
	_, _ = w.Write([]byte("hello"))
	_ = zw.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("domain", "nope.example.com")
	_ = mw.WriteField("admin_user", "chef")
	_ = mw.WriteField("admin_email", "a@b.co")
	_ = mw.WriteField("admin_password", "a-very-long-password")
	fw, _ := mw.CreateFormFile("files_zip", "x.zip")
	_, _ = fw.Write(zipBuf.Bytes())
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/websites/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := context.WithValue(req.Context(), ctxKey(2), &store.Session{CSRFToken: "tok"})
	ctx = context.WithValue(ctx, ctxAdmin, &store.Admin{Username: "root"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.importSubmit(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (form re-rendered with error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "wp-content") {
		t.Errorf("error page should explain the wp-content requirement; body: %.300s", rec.Body.String())
	}
	if _, err := st.GetWebsiteByDomain("nope.example.com"); err == nil {
		t.Error("no website record may be created for a rejected import")
	}
	entries, _ := os.ReadDir(sitesDir)
	if len(entries) != 0 {
		t.Errorf("staging directory was not cleaned up: %v", entries)
	}
}
