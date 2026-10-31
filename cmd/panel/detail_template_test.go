package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/web"
)

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
	for _, want := range []string{"Open Website", "Open WordPress Admin", "Monitoring", "running", "Last job failed", "Advanced tools"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if strings.Contains(out, `class="tabs"`) {
		t.Error("rendered page still contains the old tab bar")
	}
}
