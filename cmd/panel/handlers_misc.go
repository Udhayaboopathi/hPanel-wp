package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// ---------- jobs ----------

func (a *App) jobsList(w http.ResponseWriter, r *http.Request) {
	jobs, _ := a.store.ListJobs(100)
	a.render(w, "jobs.html", pageData{Title: "Deployment Jobs", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: jobs, Flash: r.URL.Query().Get("flash")})
}

func (a *App) jobDetail(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.GetJob(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(404)
		a.render(w, "error.html", pageData{Title: "Not found", Admin: adminFrom(r), Error: "Job not found."})
		return
	}
	events, _ := a.store.ListEvents(job.ID, 500)
	a.render(w, "job.html", pageData{Title: "Job " + job.ID, Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data: map[string]any{"Job": job, "Events": events}, Flash: r.URL.Query().Get("flash")})
}

func (a *App) jobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.runner.Cancel(id); err != nil {
		http.Redirect(w, r, "/jobs/"+id+"?flash="+err.Error(), http.StatusSeeOther)
		return
	}
	a.audit(r, "job.cancel", id, "")
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// retryableKindOps maps job kinds whose agent request can be fully
// reconstructed from the current website record to their agent op.
// Retrying those RE-EXECUTES the operation as a fresh job. Kinds that
// carry operator input which is never stored (create_site's admin
// password, wpcli arguments, a new rotation password, restore's
// backup choice) cannot be reconstructed and are rejected with
// precise guidance instead of a "queued" state that never runs.
var retryableKindOps = map[string]string{
	"start_site":   agentproto.OpStartSite,
	"stop_site":    agentproto.OpStopSite,
	"restart_site": agentproto.OpRestartSite,
	"repair_site":  agentproto.OpRepairSite,
	// backup_site is deliberately absent: a backup job must carry a
	// Backup record ID created by the website page's backup action
	// (plus its reconciliation watcher), which a bare re-execution
	// cannot reproduce - an orphaned artifact would result.
}

func (a *App) jobRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := a.store.GetJob(id)
	if err != nil {
		http.Redirect(w, r, "/jobs/"+id+"?flash=Job not found.", http.StatusSeeOther)
		return
	}
	if job.Status != "failed" && job.Status != "interrupted" && job.Status != "cancelled" {
		http.Redirect(w, r, "/jobs/"+id+"?flash=Only failed, interrupted or cancelled jobs can be retried.", http.StatusSeeOther)
		return
	}
	op, ok := retryableKindOps[job.Kind]
	if !ok {
		msg := "This job type cannot be retried from here because it needs input that is never stored (for example the WordPress admin password). Re-run the operation from the website page."
		if job.Kind == "create_site" {
			msg = "A failed creation cannot be retried from here: the WordPress admin password is never stored. Use Repair Website on the website page - it recreates the container against the existing database and files - or delete the site and create it again."
		}
		if job.Kind == "import_site" {
			msg = "A failed import cannot be retried from here: the WordPress admin password is never stored. Re-run the import from the Migrate page (the site record and its database can be deleted first from the website page if you want a clean slate)."
		}
		http.Redirect(w, r, "/jobs/"+id+"?flash="+msg, http.StatusSeeOther)
		return
	}
	site, err := a.store.GetWebsiteBySiteID(job.SiteID)
	if err != nil {
		http.Redirect(w, r, "/jobs/"+id+"?flash=The website for this job no longer exists in the panel.", http.StatusSeeOther)
		return
	}
	spec, err := a.agentSpec(site, job.Kind == "repair_site")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	newJob, err := a.runner.EnqueueSimple(site, job.Kind, agentproto.Request{Op: op, Site: spec})
	if err != nil {
		http.Redirect(w, r, "/jobs/"+id+"?flash=Could not queue the retry: "+err.Error(), http.StatusSeeOther)
		return
	}
	_ = a.store.AddEvent(id, "info", "retried as new job "+newJob.ID)
	a.audit(r, "job.retry", id, "re-executed as new job "+newJob.ID+" ("+job.Kind+")")
	http.Redirect(w, r, "/jobs/"+newJob.ID+"?flash=Retry queued - this is the new job for the same operation.", http.StatusSeeOther)
}

// ---------- backups ----------

func (a *App) backupsList(w http.ResponseWriter, r *http.Request) {
	backups, _ := a.store.ListBackups("", 200)
	a.render(w, "backups.html", pageData{Title: "Backups", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: backups})
}

// ---------- monitoring ----------

func (a *App) monitoringPage(w http.ResponseWriter, r *http.Request) {
	host := monitoring.Sample("/")
	sites, _, _ := a.store.ListWebsites("", "all", 100, 0)
	var statsOutput string
	if a.agent != nil && a.agent.Available() {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpStats})
		cancel()
		if err == nil && resp.OK {
			statsOutput = resp.Output
		}
	}
	samples, _ := a.store.ListResourceSamples("host", "", 120)
	a.render(w, "monitoring.html", pageData{Title: "Resource Monitoring", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data: map[string]any{"Host": host, "Sites": sites, "Discovery": statsOutput, "Alerts": monitoring.Alerts(host), "AgentUp": a.agentUp(),
			"Bars": makeBars(samples, float64(host.MemTotal))}})
}

func (a *App) apiMetrics(w http.ResponseWriter, r *http.Request) {
	host := monitoring.Sample("/")
	counts, total, _ := a.store.CountWebsitesByStatus()
	writeJSON(w, 200, map[string]any{
		"cpu_percent": host.CPUPercent, "mem_total": host.MemTotal, "mem_used": host.MemUsed, "mem_avail": host.MemAvail,
		"disk_total": host.DiskTotal, "disk_free": host.DiskFree, "disk_used": host.DiskUsed,
		"load1": host.Load1, "uptime_sec": host.UptimeSec,
		"websites_total": total, "websites_by_status": counts, "agent_available": a.agentUp(),
	})
}

func (a *App) apiWebsites(w http.ResponseWriter, r *http.Request) {
	sites, total, _ := a.store.ListWebsites(r.URL.Query().Get("q"), r.URL.Query().Get("status"), 100, 0)
	type row struct {
		SiteID string `json:"site_id"`
		Domain string `json:"domain"`
		Status string `json:"status"`
		PHP    string `json:"php_version"`
	}
	out := make([]row, 0, len(sites))
	for _, s := range sites {
		out = append(out, row{s.SiteID, s.Domain, s.Status, s.PHPVersion})
	}
	writeJSON(w, 200, map[string]any{"total": total, "websites": out})
}

func (a *App) apiJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.GetJob(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}
	events, _ := a.store.ListEvents(job.ID, 200)
	writeJSON(w, 200, map[string]any{"job": job, "events": events})
}

// ---------- audit ----------

func (a *App) auditList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	entries, _ := a.store.ListAudit(100, (page-1)*100)
	a.render(w, "audit.html", pageData{Title: "Audit Logs", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data: map[string]any{"Entries": entries, "Page": page, "HasNext": len(entries) == 100}})
}

// ---------- account & security ----------

// setOnce/popOnce carry one-time secrets (a fresh TOTP seed, new
// recovery codes) from a POST to exactly one page render, server-side.
// They must never travel in redirect URLs: URLs land in browser
// history and reverse-proxy access logs, which would leak live
// second-factor material.
func (a *App) setOnce(key, val string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.once == nil {
		a.once = map[string]string{}
	}
	a.once[key] = val
}

func (a *App) popOnce(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.once[key]
	delete(a.once, key)
	return v
}

func (a *App) accountPage(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	id := fmt.Sprint(admin.ID)
	a.render(w, "account.html", pageData{Title: "Account & Security", Admin: admin, CSRF: sessionFrom(r).CSRFToken,
		Flash: r.URL.Query().Get("flash"),
		Data: map[string]any{
			"TOTPSecret": a.popOnce("totp_secret:" + id),
			"TOTPURL":    a.popOnce("totp_url:" + id),
			"Recovery":   splitCSV(a.popOnce("recovery:" + id)),
		}})
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == ',' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(ch)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func (a *App) accountPassword(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	_ = r.ParseForm()
	if !auth.VerifyPassword(admin.PasswordHash, r.FormValue("current_password")) {
		http.Redirect(w, r, "/account?flash=Current password is incorrect.", http.StatusSeeOther)
		return
	}
	if r.FormValue("new_password") != r.FormValue("confirm_password") {
		http.Redirect(w, r, "/account?flash=New passwords do not match.", http.StatusSeeOther)
		return
	}
	hash, err := auth.HashPassword(r.FormValue("new_password"))
	if err != nil {
		http.Redirect(w, r, "/account?flash="+err.Error(), http.StatusSeeOther)
		return
	}
	_ = a.store.UpdateAdminPassword(admin.ID, hash)
	// Changing the password revokes every other session.
	_ = a.store.DeleteAllSessions(admin.ID)
	a.audit(r, "account.password_change", admin.Username, "all sessions revoked")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) accountTOTPSetup(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	secret, url, err := auth.GenerateTOTPSecret(admin.Username)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Stored disabled until the admin confirms with a valid code.
	// The seed is stored ENCRYPTED (the panel DB must not hand out
	// second factors in plaintext) and shown once via server-side
	// one-time storage - never in the redirect URL.
	stored := secret
	if a.box != nil {
		if enc, err := a.box.Encrypt(secret); err == nil {
			stored = enc
		}
	}
	_ = a.store.UpdateAdminTOTP(admin.ID, stored, false)
	a.setOnce(fmt.Sprintf("totp_secret:%d", admin.ID), secret)
	a.setOnce(fmt.Sprintf("totp_url:%d", admin.ID), url)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (a *App) accountTOTPEnable(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	_ = r.ParseForm()
	fresh, err := a.store.GetAdminByID(admin.ID)
	if err != nil || fresh.TOTPSecret == "" {
		http.Redirect(w, r, "/account?flash=Generate a TOTP secret first.", http.StatusSeeOther)
		return
	}
	if !auth.ValidateTOTP(a.totpSecret(fresh), r.FormValue("code")) {
		http.Redirect(w, r, "/account?flash=Invalid TOTP code; two-factor remains disabled.", http.StatusSeeOther)
		return
	}
	_ = a.store.UpdateAdminTOTP(admin.ID, fresh.TOTPSecret, true)
	a.audit(r, "account.totp_enable", admin.Username, "")
	http.Redirect(w, r, "/account?flash=Two-factor authentication enabled.", http.StatusSeeOther)
}

func (a *App) accountTOTPDisable(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	_ = r.ParseForm()
	if !auth.VerifyPassword(admin.PasswordHash, r.FormValue("current_password")) {
		http.Redirect(w, r, "/account?flash=Current password is incorrect.", http.StatusSeeOther)
		return
	}
	_ = a.store.UpdateAdminTOTP(admin.ID, "", false)
	a.audit(r, "account.totp_disable", admin.Username, "")
	http.Redirect(w, r, "/account?flash=Two-factor authentication disabled.", http.StatusSeeOther)
}

func (a *App) accountRecovery(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	plains, hashes, err := auth.GenerateRecoveryCodes(10)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := a.store.ReplaceRecoveryCodes(admin.ID, hashes); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "account.recovery_regenerate", admin.Username, "previous recovery codes invalidated")
	joined := ""
	for i, p := range plains {
		if i > 0 {
			joined += ","
		}
		joined += p
	}
	// One-time server-side display only - plaintext recovery codes in
	// a redirect URL would persist in browser history and proxy logs.
	a.setOnce(fmt.Sprintf("recovery:%d", admin.ID), joined)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// ---------- settings ----------

var editableSettings = []string{"backup_retention", "s3_endpoint", "s3_region", "s3_bucket", "default_profile", "alert_email"}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	vals := map[string]string{}
	for _, k := range editableSettings {
		vals[k] = a.store.GetSetting(k, "")
	}
	vals["backup_retention"] = a.store.GetSetting("backup_retention", "7")
	vals["default_profile"] = a.store.GetSetting("default_profile", "lightweight")
	a.render(w, "settings.html", pageData{Title: "Server Settings", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data:  map[string]any{"Values": vals, "AgentUp": a.agentUp(), "ServerIP": a.cfg.ServerIP, "PanelDomain": a.cfg.PanelDomain},
		Flash: r.URL.Query().Get("flash")})
}

func (a *App) settingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	for _, k := range editableSettings {
		if k == "backup_retention" {
			if n, err := strconv.Atoi(r.FormValue(k)); err != nil || n < 1 || n > 90 {
				http.Redirect(w, r, "/settings?flash=Backup retention must be between 1 and 90.", http.StatusSeeOther)
				return
			}
		}
		_ = a.store.SetSetting(k, r.FormValue(k))
	}
	// S3 credentials are intentionally NOT a settings field: they are read by
	// the agent from environment/secret files, never stored in the panel DB.
	a.audit(r, "settings.update", "", "server settings updated")
	http.Redirect(w, r, "/settings?flash=Settings saved.", http.StatusSeeOther)
}

var _ = store.Now
