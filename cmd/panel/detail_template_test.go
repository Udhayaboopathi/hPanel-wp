package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
	"github.com/Udhayaboopathi/hPanel-wp/web"
)

func monitoring_HostStats() monitoring.HostStats { return monitoring.HostStats{MemTotal: 1024} }

// The site detail page is the panel's main working screen; render it
// with fabricated data so template field errors are caught by tests
// instead of by the first live page load.
func TestDetailTemplateRenders(t *testing.T) {
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
		t.Fatalf("parse templates: %v", err)
	}
	site := &store.Website{
		SiteID: "wp-test1234", Domain: "example.com", AdminDomain: "wpadmin.example.com",
		Status: "failed", PHPVersion: "8.3", DBName: "wp_test_db", DBUser: "wp_test_usr",
		ContainerName: "own-hpanel-wp-test1234", ResourceProfile: "lightweight",
		CPULimit: 0.5, MemLimitMB: 512, CreatedAt: "2026-10-31T12:21:00Z",
	}
	d := detailData{
		Site: site, Tab: "overview", AgentUp: true, DBSize: 1234567,
		LatestJob: &store.Job{ID: "job_test", Kind: "create_site", Status: "failed", Error: "boom"},
		Live:      &liveStats{Running: true, CPUPercent: 3.25, MemUsage: 128 << 20, MemLimit: 512 << 20},
	}
	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "detail.html", pageData{Title: site.Domain, CSRF: "tok", Data: d})
	if err != nil {
		t.Fatalf("execute detail.html: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Open Website", "Open WordPress Admin", "Monitoring", "running", "Last job failed", "Performance", "Restore", "/websites/wp-test1234/compose", "subnav"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if strings.Contains(out, `class="tabs"`) {
		t.Error("rendered page still contains the old tab bar")
	}
}

// The other templates touched by the audit fixes (create-wizard
// input rebinding, websites sort retention, audit pagination, the
// pre-auth CSRF field) get the same render smoke test.
func TestOtherTemplatesRender(t *testing.T) {
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
		"since": func(ts string) string { return ts },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	site2 := &store.Website{
		SiteID: "wp-test1234", Domain: "example.com", AdminDomain: "wpadmin.example.com",
		Status: "ready", PHPVersion: "8.3", DBName: "wp_test_db", DBUser: "wp_test_usr",
		ContainerName: "own-hpanel-wp-test1234", ResourceProfile: "standard",
		CPULimit: 1, MemLimitMB: 512, CreatedAt: "2026-10-31T12:21:00Z",
	}
	cases := []struct {
		name string
		data pageData
		want string
	}{
		{"create.html", pageData{Title: "Create", CSRF: "tok", Data: map[string]any{
			"ServerIP": "130.210.42.20", "DefaultProfile": "standard",
			"Input": websites.CreateInput{Domain: "example.com", Profile: "standard"},
		}}, `value="example.com"`},
		{"websites.html", pageData{Title: "Websites", CSRF: "tok", Data: map[string]any{
			"Sites": []*store.Website{}, "Total": 0, "Page": 1, "Pages": 1, "Q": "", "Status": "missing", "Sort": "domain",
		}}, "missing"},
		{"audit.html", pageData{Title: "Audit", CSRF: "tok", Data: map[string]any{
			"Entries": []*store.AuditEntry{}, "Page": 1, "HasNext": false,
		}}, "No audit entries yet"},
		{"login.html", pageData{Title: "Login", CSRF: "pretok"}, `value="pretok"`},
		{"performance.html", pageData{Title: "Performance", CSRF: "tok", Data: detailData{
			Site: site2, Tab: "performance", AgentUp: true, DBSize: 2048,
			Live: &liveStats{Running: true, CPUPercent: 12.5, MemUsage: 64 << 20, MemLimit: 512 << 20},
			Bars: []sampleBar{{When: "2026-10-31T10:00:00Z", CPUPct: 12.5, MemPct: 25, CPUBH: 12, MemBH: 25}},
		}}, "Usage history"},
		{"restore.html", pageData{Title: "Restore", CSRF: "tok", Data: detailData{
			Site: site2, Tab: "restore",
			Backups: []*store.Backup{{ID: 7, Status: "ready", SizeBytes: 4096, SHA256: "abcdef0123456789", CreatedAt: "2026-10-31T09:00:00Z"}},
		}}, "Safety backup is taken"},
		{"site_settings.html", pageData{Title: "Settings", CSRF: "tok", Data: detailData{
			Site: site2, Tab: "settings",
		}}, "Delete website"},
		{"containers.html", pageData{Title: "Containers", CSRF: "tok", Data: containersData{
			AgentUp: true,
			Rows: []containerRow{
				{Name: "redberyl-traefik", Image: "traefik:v3.2", State: "running", Class: "protected", Protected: true},
				{Name: "own-hpanel-wp-test1234", Image: "wordpress:7.1.4-php8.3-apache", State: "running", Class: "managed", SiteID: "wp-test1234"},
			},
			Networks: []string{"redberyl-internal"},
		}}, "protected · read-only"},
		{"logs.html", pageData{Title: "Logs", CSRF: "tok", Data: logsData{
			Sites: []*store.Website{site2}, Selected: site2, Output: "apache started",
		}}, "apache started"},
		{"import.html", pageData{Title: "Migrate", CSRF: "tok", Data: map[string]any{
			"Sites": []*store.Website{site2},
		}}, "Migrate a WordPress Website"},
		{"dashboard.html", pageData{Title: "Dashboard", CSRF: "tok", Data: dashData{
			Bars: []sampleBar{{When: "2026-10-31T10:00:00Z", CPUPct: 20, MemPct: 40, CPUBH: 20, MemBH: 40}},
		}}, "Server load history"},
		{"monitoring.html", pageData{Title: "Monitoring", CSRF: "tok", Data: map[string]any{
			"Host": monitoring_HostStats(), "Sites": []*store.Website{}, "Discovery": "", "Alerts": []string{}, "AgentUp": false,
			"Bars": []sampleBar{{When: "2026-10-31T10:00:00Z", CPUPct: 20, MemPct: 40, CPUBH: 20, MemBH: 40}},
		}}, "Server load history"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, tc.name, tc.data); err != nil {
			t.Errorf("%s: execute: %v", tc.name, err)
			continue
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s: rendered output missing %q", tc.name, tc.want)
		}
	}
}
