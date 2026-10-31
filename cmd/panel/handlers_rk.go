package main

// RK Host pages added for the Stitch-based UI: performance, restore
// wizard, site settings, container inventory, global logs and the
// migration guide. Every page is backed by existing store/agent
// capabilities only — nothing here invents data.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// ---------- performance ----------

func (a *App) performancePage(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	d := detailData{Site: site, Tab: "performance", AgentUp: a.agentUp(), DBSize: a.store.DBSize(site.SiteID)}
	if d.AgentUp && site.ContainerName != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpStats}); err == nil && resp.OK {
			d.Live = parseSiteStats(resp.Output, site.SiteID)
			if d.Live != nil {
				_ = a.store.AddResourceSample("site", site.SiteID, d.Live.CPUPercent, int64(d.Live.MemUsage))
			}
		}
		cancel()
	}
	d.Samples, _ = a.store.ListResourceSamples("site", site.SiteID, 120)
	d.Bars = makeBars(d.Samples, float64(site.MemLimitMB)*1048576)
	a.render(w, "performance.html", pageData{Title: site.Domain + " · Performance", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d})
}

// makeBars normalizes samples into chart bars: CPU as % of one core,
// memory as % of the given limit (bytes).
func makeBars(samples []store.ResourceSample, limitBytes float64) []sampleBar {
	bars := make([]sampleBar, 0, len(samples))
	for _, sm := range samples {
		bar := sampleBar{When: sm.CreatedAt, CPUPct: sm.CPUPercent}
		if limitBytes > 0 {
			bar.MemPct = float64(sm.MemBytes) / limitBytes * 100
		}
		bar.CPUBH = int(min(bar.CPUPct, 100))
		bar.MemBH = int(min(bar.MemPct, 100))
		if bar.CPUBH < 2 {
			bar.CPUBH = 2
		}
		if bar.MemBH < 2 {
			bar.MemBH = 2
		}
		bars = append(bars, bar)
	}
	return bars
}

// sampleLoop records host and per-site resource samples every minute
// so the dashboard, server and performance graphs show a continuous
// history instead of only moments when a page happened to be opened.
// It only reads: one host sample plus one agent OpStats call.
func (a *App) sampleLoop() {
	tick := func() {
		host := monitoring.Sample("/")
		_ = a.store.AddResourceSample("host", "", host.CPUPercent, int64(host.MemUsed))
		if a.agent == nil || !a.agent.Available() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpStats})
		cancel()
		if err != nil || !resp.OK {
			return
		}
		for siteID, st := range parseAllStats(resp.Output) {
			_ = a.store.AddResourceSample("site", siteID, st.CPUPercent, int64(st.MemUsage))
		}
	}
	time.Sleep(5 * time.Second)
	tick()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		tick()
	}
}

// ---------- restore wizard ----------

func (a *App) restorePage(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	d := detailData{Site: site, Tab: "restore", AgentUp: a.agentUp()}
	d.Backups, _ = a.store.ListBackups(site.SiteID, 50)
	a.render(w, "restore.html", pageData{Title: site.Domain + " · Restore", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d})
}

// ---------- site settings & lifecycle ----------

func (a *App) siteSettingsPage(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	d := detailData{Site: site, Tab: "settings", AgentUp: a.agentUp(), DBSize: a.store.DBSize(site.SiteID)}
	if chk, err := a.store.LatestSSLCheck(site.Domain); err == nil {
		d.SSL = chk
	}
	a.render(w, "site_settings.html", pageData{Title: site.Domain + " · Settings", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d})
}

// ---------- docker inventory ----------

type containerRow struct {
	Name      string
	Image     string
	State     string
	Class     string // protected | managed | other
	SiteID    string
	Protected bool
}

type containersData struct {
	Rows     []containerRow
	Networks []string
	AgentUp  bool
	Error    string
}

func (a *App) containersPage(w http.ResponseWriter, r *http.Request) {
	d := containersData{AgentUp: a.agentUp()}
	sites, _, _ := a.store.ListWebsites("", "all", 500, 0)
	byContainer := map[string]string{}
	for _, s := range sites {
		if s.ContainerName != "" {
			byContainer[s.ContainerName] = s.SiteID
		}
	}
	if d.AgentUp {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpDiscovery})
		cancel()
		if err != nil || !resp.OK {
			d.Error = "The provisioning agent did not return a container inventory."
		} else {
			for _, line := range strings.Split(resp.Output, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "network: ") {
					d.Networks = append(d.Networks, strings.TrimPrefix(line, "network: "))
					continue
				}
				f := strings.Fields(line)
				if len(f) < 4 {
					continue
				}
				row := containerRow{Name: f[0], Image: f[1], State: f[2]}
				switch {
				case agentproto.ProtectedContainers[row.Name]:
					row.Class, row.Protected = "protected", true
				case strings.HasSuffix(f[3], "managed=true"):
					row.Class = "managed"
					row.SiteID = byContainer[row.Name]
				default:
					row.Class = "other"
				}
				d.Rows = append(d.Rows, row)
			}
		}
	} else {
		d.Error = "The provisioning agent is not connected, so the live Docker inventory is unavailable."
	}
	a.render(w, "containers.html", pageData{Title: "Docker Containers", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d})
}

// ---------- global logs ----------

type logsData struct {
	Sites    []*store.Website
	Selected *store.Website
	Output   string
	Note     string
}

func (a *App) logsPage(w http.ResponseWriter, r *http.Request) {
	sites, _, _ := a.store.ListWebsites("", "all", 500, 0)
	d := logsData{Sites: sites}
	sel := r.URL.Query().Get("site")
	for _, s := range sites {
		if s.SiteID == sel {
			d.Selected = s
			break
		}
	}
	if d.Selected == nil && len(sites) > 0 {
		d.Selected = sites[0]
	}
	if d.Selected != nil {
		d.Output, d.Note = a.fetchSiteLogs(r.Context(), d.Selected)
	}
	a.render(w, "logs.html", pageData{Title: "Logs", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d})
}

// ---------- migration guide ----------

func (a *App) importPage(w http.ResponseWriter, r *http.Request) {
	sites, _, _ := a.store.ListWebsites("", "all", 500, 0)
	a.render(w, "import.html", pageData{Title: "Migrate a WordPress Website", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: map[string]any{"Sites": sites}})
}

// fetchSiteLogs returns the site's live container logs via the agent,
// falling back to its deployment/job events when the container log is
// unavailable. Shared by the logs page and the live-polling API.
func (a *App) fetchSiteLogs(ctx context.Context, site *store.Website) (string, string) {
	if a.agent != nil && a.agent.Available() {
		if spec, err := a.agentSpec(site, false); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			resp, err := a.agent.Call(cctx, agentproto.Request{Op: agentproto.OpLogs, Site: spec})
			cancel()
			if err == nil && resp.OK && resp.Output != "" {
				return resp.Output, ""
			}
		}
	}
	var sb strings.Builder
	jobs, _ := a.store.ListJobsForSite(site.SiteID)
	for _, j := range jobs {
		events, _ := a.store.ListEvents(j.ID, 100)
		for _, e := range events {
			fmt.Fprintf(&sb, "%s [%s] %s: %s\n", e.CreatedAt, e.Level, j.Kind, e.Message)
		}
	}
	out := sb.String()
	if out == "" {
		out = "No logs or deployment events recorded yet for this website."
	}
	return out, "Live container logs are unavailable (agent offline or container not running) — showing deployment events instead."
}

// apiLogs serves plain-text logs for the logs page's live polling.
func (a *App) apiLogs(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("site")
	sites, _, _ := a.store.ListWebsites("", "all", 500, 0)
	var site *store.Website
	for _, s := range sites {
		if s.SiteID == sel {
			site = s
			break
		}
	}
	if site == nil && len(sites) > 0 {
		site = sites[0]
	}
	if site == nil {
		http.Error(w, "no websites", http.StatusNotFound)
		return
	}
	out, _ := a.fetchSiteLogs(r.Context(), site)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, out)
}
