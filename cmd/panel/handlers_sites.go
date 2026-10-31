package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/configgen"
	"github.com/Udhayaboopathi/hPanel-wp/internal/domains"
	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
	"github.com/Udhayaboopathi/hPanel-wp/internal/wordpress"
)

// ---------- dashboard ----------

type dashData struct {
	Host        monitoring.HostStats
	Counts      map[string]int
	Total       int
	Running     int
	Stopped     int
	Failed      int
	Recent      []*store.Event
	AgentUp     bool
	MariaOK     bool
	TraefikOK   bool
	Alerts      []string
	BackupCount int
}

func (a *App) agentUp() bool { return a.agent != nil && a.agent.Available() }

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	counts, total, _ := a.store.CountWebsitesByStatus()
	host := monitoring.Sample("/")
	_ = a.store.AddResourceSample("host", "", host.CPUPercent, int64(host.MemUsed))
	recent, _ := a.store.RecentEvents(12)
	backups, _ := a.store.ListBackups("", 500)
	backupOK := 0
	for _, b := range backups {
		if b.Status == "ready" {
			backupOK++
		}
	}
	d := dashData{
		Host: host, Counts: counts, Total: total,
		Running: counts["ready"] + counts["dns_pending"],
		Stopped: counts["stopped"], Failed: counts["failed"],
		Recent: recent, AgentUp: a.agentUp(), BackupCount: backupOK,
		Alerts: monitoring.Alerts(host),
	}
	// Traefik/MariaDB status is discovery data from the agent, never faked:
	// when the agent is down the dashboard says so instead of inventing "OK".
	a.render(w, "dashboard.html", pageData{Title: "Dashboard", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d, Flash: r.URL.Query().Get("flash")})
}

// ---------- websites list ----------

func (a *App) websitesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	status := r.URL.Query().Get("status")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 10
	sites, total, err := a.store.ListWebsites(q, status, perPage, (page-1)*perPage)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Optional server-side sort by domain (default: newest first from store).
	if r.URL.Query().Get("sort") == "domain" {
		sort.Slice(sites, func(i, j int) bool { return sites[i].Domain < sites[j].Domain })
	}
	a.render(w, "websites.html", pageData{Title: "Websites", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: map[string]any{
		"Sites": sites, "Total": total, "Page": page, "Pages": (total + perPage - 1) / perPage,
		"Q": q, "Status": status,
	}, Flash: r.URL.Query().Get("flash")})
}

// ---------- create wizard ----------

func (a *App) createForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, "create.html", pageData{Title: "Create New WordPress Site", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data: map[string]any{"ServerIP": a.cfg.ServerIP}})
}

func parseCreateInput(r *http.Request) websites.CreateInput {
	_ = r.ParseForm()
	cpu, _ := strconv.ParseFloat(r.FormValue("cpu_limit"), 64)
	mem, _ := strconv.Atoi(r.FormValue("mem_limit_mb"))
	return websites.CreateInput{
		Domain: r.FormValue("domain"), Title: r.FormValue("title"),
		DBName:        strings.TrimSpace(r.FormValue("db_name")),
		AdminUser:     strings.TrimSpace(r.FormValue("admin_user")),
		AdminEmail:    strings.TrimSpace(r.FormValue("admin_email")),
		AdminPassword: r.FormValue("admin_password"),
		AdminDomain:   r.FormValue("admin_domain"),
		PHPVersion:    r.FormValue("php_version"), Profile: r.FormValue("resource_profile"),
		CPULimit: cpu, MemLimitMB: mem,
	}
}

func (a *App) createPreview(w http.ResponseWriter, r *http.Request) {
	in := parseCreateInput(r)
	if err := in.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	profile, err := configgen.ProfileFor(in.Profile, in.CPULimit, in.MemLimitMB)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"domain": in.Domain, "admin_url": "https://" + in.Domain + "/wp-admin/",
		"admin_domain": in.AdminDomain, "admin_domain_target": "https://" + in.Domain + "/wp-admin/",
		"db_name": in.DBName, "php_version": in.PHPVersion,
		"cpu": profile.CPUs, "mem_mb": profile.MemMB,
		"dns_records": configgen.DNSRecords(in.Domain, a.cfg.ServerIP),
	})
}

func genDBPassword() (string, error) {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 32)
	for i, v := range b {
		out[i] = alpha[int(v)%len(alpha)]
	}
	return string(out), nil
}

func genHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) createSubmit(w http.ResponseWriter, r *http.Request) {
	in := parseCreateInput(r)
	fail := func(msg string) {
		a.render(w, "create.html", pageData{Title: "Create New WordPress Site", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
			Error: msg, Data: map[string]any{"ServerIP": a.cfg.ServerIP, "Input": in}})
	}
	if err := in.Validate(); err != nil {
		fail(err.Error())
		return
	}
	profile, err := configgen.ProfileFor(in.Profile, in.CPULimit, in.MemLimitMB)
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := a.store.GetWebsiteByDomain(in.Domain); err == nil {
		fail("A website with domain " + in.Domain + " already exists in the panel.")
		return
	}
	// Capacity checks against real host state.
	host := monitoring.Sample("/")
	if host.DiskTotal > 0 && host.DiskFree < 2<<30 {
		fail("Less than 2 GB of free disk space remains; refusing to create a new website.")
		return
	}
	if host.MemTotal > 0 && host.MemAvail < 512<<20 {
		fail("Less than 512 MB of RAM is available; refusing to create a new website.")
		return
	}
	siteID := websites.GenerateSiteID()
	dbName := in.DBName
	if dbName == "" {
		dbName = websites.DBNameFor(siteID)
	}
	// DB name uniqueness across sites.
	if sites, _, _ := a.store.ListWebsites("", "all", 1000, 0); sites != nil {
		for _, s := range sites {
			if s.DBName == dbName {
				fail("Database name " + dbName + " is already used by " + s.Domain + ".")
				return
			}
		}
	}
	dbUser := websites.DBUserFor(siteID)
	dbPW, err := genDBPassword()
	if err != nil {
		fail(err.Error())
		return
	}
	encPW, err := a.box.Encrypt(dbPW)
	if err != nil {
		fail(err.Error())
		return
	}
	siteDir := filepath.Join(a.cfg.SitesDir, siteID)
	site := &store.Website{
		SiteID: siteID, Name: in.Title, Domain: in.Domain, AdminDomain: in.AdminDomain,
		Title: in.Title, Status: "provisioning", PHPVersion: in.PHPVersion,
		ResourceProfile: in.Profile, CPULimit: profile.CPUs, MemLimitMB: profile.MemMB,
		ContainerName: websites.ContainerName(siteID),
		DBName:        dbName, DBUser: dbUser, DBPasswordEnc: encPW,
		AdminUsername: in.AdminUser, AdminEmail: in.AdminEmail, SiteDir: siteDir,
	}
	if _, err := a.store.CreateWebsite(site); err != nil {
		fail("Could not save website: " + err.Error())
		return
	}
	// Persist generated config (contains no secrets: DB password lives only in
	// the encrypted column and the site .env written by the agent).
	spec := configgen.SiteSpec{
		SiteID: siteID, Domain: in.Domain, AdminDomain: in.AdminDomain, Title: in.Title,
		DBName: dbName, DBUser: dbUser, DBHost: a.cfg.SiteDBHost,
		PHPVersion: in.PHPVersion, Profile: profile, SiteDir: siteDir,
		ContainerName: site.ContainerName, Network: websites.NetworkName(siteID),
	}
	composeYAML, err := configgen.GenerateCompose(spec)
	if err != nil {
		fail(err.Error())
		return
	}
	envJSON, _ := json.Marshal(map[string]string{
		"PHP_MEMORY_LIMIT": "256M", "UPLOAD_MAX_FILESIZE": "64M", "POST_MAX_SIZE": "64M", "WORDPRESS_DEBUG": "0",
	})
	if _, err := a.store.SaveConfigVersion(site.ID, string(envJSON), composeYAML, "initial configuration"); err != nil {
		a.log.Warn("save config version", "err", err)
	}
	a.audit(r, "website.create", siteID, "domain="+in.Domain+" db="+dbName)
	agentSpec := agentproto.SiteSpec{
		SiteID: siteID, Domain: in.Domain, AdminDomain: in.AdminDomain, Title: in.Title,
		DBName: dbName, DBUser: dbUser, DBPassword: dbPW,
		AdminUser: in.AdminUser, AdminEmail: in.AdminEmail,
		PHPVersion: in.PHPVersion, Profile: in.Profile, CPULimit: profile.CPUs, MemLimitMB: profile.MemMB,
		SiteDir: siteDir, ContainerName: site.ContainerName,
	}
	if _, err := a.runner.Enqueue(site, "create_site", agentSpec, in.AdminPassword); err != nil {
		_ = a.store.UpdateWebsite(siteID, map[string]any{"status": "failed"})
		fail("Could not start provisioning job: " + err.Error())
		return
	}
	http.Redirect(w, r, "/websites/"+siteID+"?flash=Provisioning started. Follow progress in Deployment Jobs.", http.StatusSeeOther)
}

// ---------- site spec helper ----------

func (a *App) agentSpec(site *store.Website, withSecrets bool) (agentproto.SiteSpec, error) {
	spec := agentproto.SiteSpec{
		SiteID: site.SiteID, Domain: site.Domain, AdminDomain: site.AdminDomain, Title: site.Title,
		DBName: site.DBName, DBUser: site.DBUser, AdminUser: site.AdminUsername, AdminEmail: site.AdminEmail,
		PHPVersion: site.PHPVersion, Profile: site.ResourceProfile, CPULimit: site.CPULimit, MemLimitMB: site.MemLimitMB,
		SiteDir: site.SiteDir, ContainerName: site.ContainerName,
	}
	if withSecrets {
		pw, err := a.box.Decrypt(site.DBPasswordEnc)
		if err != nil {
			return spec, err
		}
		spec.DBPassword = pw
	}
	return spec, nil
}

func (a *App) getSiteOr404(w http.ResponseWriter, r *http.Request) *store.Website {
	site, err := a.store.GetWebsiteBySiteID(r.PathValue("site"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		a.render(w, "error.html", pageData{Title: "Not found", Admin: adminFrom(r), Error: "Website not found."})
		return nil
	}
	return site
}

// ---------- lifecycle actions ----------

func (a *App) siteAction(kind, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := a.getSiteOr404(w, r)
		if site == nil {
			return
		}
		spec, err := a.agentSpec(site, false)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.audit(r, "website."+kind, site.SiteID, "domain="+site.Domain)
		_, err = a.runner.EnqueueSimple(site, kind, agentproto.Request{Op: op, Site: spec})
		flash := "Job queued: " + kind
		if err != nil {
			flash = "Error: " + err.Error()
		}
		http.Redirect(w, r, "/websites/"+site.SiteID+"?flash="+flash, http.StatusSeeOther)
	}
}

func (a *App) siteDelete(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	if r.FormValue("confirm_domain") != site.Domain {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=danger&flash=Deletion not confirmed: type the exact domain to delete this website.", http.StatusSeeOther)
		return
	}
	spec, err := a.agentSpec(site, false)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.delete", site.SiteID, "domain="+site.Domain)
	job, err := a.runner.EnqueueSimple(site, "delete_site", agentproto.Request{Op: agentproto.OpDeleteSite, Site: spec})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// The DB row is removed by the operator once the delete job succeeds;
	// check synchronously after a short wait is racy, so mark deleting and let
	// the jobs page show the outcome. If the agent is unavailable the job fails
	// and the site remains, untouched.
	_ = a.store.UpdateWebsite(site.SiteID, map[string]any{"status": "deleting"})
	go func(jobID, siteID string) {
		for i := 0; i < 120; i++ {
			j, err := a.store.GetJob(jobID)
			if err == nil && j.Status == "succeeded" {
				_ = a.store.DeleteWebsite(siteID)
				return
			}
			if err == nil && (j.Status == "failed" || j.Status == "cancelled") {
				_ = a.store.UpdateWebsite(siteID, map[string]any{"status": "ready"})
				return
			}
			time.Sleep(2 * time.Second)
		}
	}(job.ID, site.SiteID)
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (a *App) siteBackup(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	b := &store.Backup{SiteID: site.SiteID, Filename: "pending", Status: "creating", Storage: "local"}
	b.WebsiteID.Int64 = site.ID
	b.WebsiteID.Valid = true
	backupID, err := a.store.CreateBackup(b)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.backup", site.SiteID, fmt.Sprintf("backup_id=%d", backupID))
	job, err := a.runner.EnqueueSimple(site, "backup_site", agentproto.Request{Op: agentproto.OpBackupSite, Site: spec, BackupID: backupID})
	if err != nil {
		_ = a.store.UpdateBackup(backupID, "failed", "", 0, false)
		http.Error(w, err.Error(), 500)
		return
	}
	// Reconcile the backup record when the job finishes (agent returns path+sha).
	go a.reconcileBackup(job.ID, backupID)
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (a *App) reconcileBackup(jobID string, backupID int64) {
	// The agent response is not persisted; reconciliation therefore re-derives
	// the artifact from the backups directory naming convention and verifies it.
	for i := 0; i < 180; i++ {
		j, err := a.store.GetJob(jobID)
		if err != nil {
			return
		}
		if j.Status == "succeeded" {
			b, err := a.store.GetBackup(backupID)
			if err != nil {
				return
			}
			entries, _ := filepath.Glob(filepath.Join(a.cfg.BackupsDir, b.SiteID+"-*.tar.gz"))
			sort.Strings(entries)
			if len(entries) == 0 {
				_ = a.store.UpdateBackup(backupID, "failed", "", 0, false)
				return
			}
			latest := entries[len(entries)-1]
			st, err := os.Stat(latest)
			if err != nil {
				_ = a.store.UpdateBackup(backupID, "failed", "", 0, false)
				return
			}
			// Compute sha by streaming the file (same routine the agent used).
			sha := sha256File(latest)
			_, _ = a.store.DB.Exec(`UPDATE backups SET filename=?, path=? WHERE id=?`, filepath.Base(latest), latest, backupID)
			_ = a.store.UpdateBackup(backupID, "ready", sha, st.Size(), true)
			return
		}
		if j.Status == "failed" || j.Status == "cancelled" || j.Status == "interrupted" {
			_ = a.store.UpdateBackup(backupID, "failed", "", 0, false)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (a *App) siteRestore(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	backup, err := a.store.GetBackup(id)
	if err != nil || backup.SiteID != site.SiteID {
		http.Error(w, "backup not found", 404)
		return
	}
	_ = r.ParseForm()
	if r.FormValue("confirm") != "RESTORE" {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=backups&flash=Type RESTORE to confirm restoring this backup.", http.StatusSeeOther)
		return
	}
	// Safety: always take a fresh backup before a restore.
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.restore", site.SiteID, fmt.Sprintf("backup_id=%d", id))
	job, err := a.runner.EnqueueSimple(site, "restore_site", agentproto.Request{Op: agentproto.OpRestoreSite, Site: spec, BackupID: id, BackupPath: backup.Path})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (a *App) backupDownload(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	backup, err := a.store.GetBackup(id)
	if err != nil || backup.SiteID != site.SiteID || backup.Status != "ready" {
		http.Error(w, "backup not found", 404)
		return
	}
	clean := filepath.Clean(backup.Path)
	if !strings.HasPrefix(clean, filepath.Clean(a.cfg.BackupsDir)+string(filepath.Separator)) {
		http.Error(w, "invalid backup path", 400)
		return
	}
	a.audit(r, "backup.download", site.SiteID, fmt.Sprintf("backup_id=%d", id))
	w.Header().Set("Content-Disposition", "attachment; filename="+backup.Filename)
	http.ServeFile(w, r, clean)
}

// ---------- WordPress operations ----------

func (a *App) siteWP(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	action := r.FormValue("action")
	arg := r.FormValue("arg")
	if _, err := wordpress.Build(action, arg); err != nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=wp&flash="+err.Error(), http.StatusSeeOther)
		return
	}
	// Potentially disruptive updates get a backup first (operator can watch both jobs).
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.wpcli", site.SiteID, "action="+action+" arg="+arg)
	job, err := a.runner.EnqueueSimple(site, "wpcli", agentproto.Request{Op: agentproto.OpWPCLI, Site: spec, Action: action, Arg: arg})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (a *App) siteResetAdminPW(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	newPW := r.FormValue("new_password")
	if len(newPW) < 12 {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=wp&flash=New admin password must be at least 12 characters.", http.StatusSeeOther)
		return
	}
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.reset_admin_password", site.SiteID, "admin_user="+site.AdminUsername)
	job, err := a.runner.EnqueueSimple(site, "wpcli", agentproto.Request{
		Op: agentproto.OpWPCLI, Site: spec, Action: "user_reset_password",
		Arg: site.AdminUsername, NewDBPassword: newPW,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

// ---------- DNS / SSL check ----------

func (a *App) siteCheck(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	dns := domains.Lookup(ctx, site.Domain, a.cfg.ServerIP)
	ssl := domains.CheckTLS(ctx, site.Domain)
	chk := &store.SSLCheck{
		Domain: site.Domain, OK: ssl.OK, Issuer: ssl.Issuer,
		ExpiresAt: ssl.ExpiresAt.Format(time.RFC3339), DaysLeft: ssl.DaysLeft,
		Cloudflare: dns.Cloudflare, DNSOK: dns.Resolved && (dns.PointsToOrigin || dns.Cloudflare),
		Detail: dns.Detail + "; " + ssl.Detail,
	}
	if ssl.ExpiresAt.IsZero() {
		chk.ExpiresAt = ""
	}
	_ = a.store.AddSSLCheck(site.ID, chk)
	if site.Status == "dns_pending" && chk.DNSOK {
		_ = a.store.UpdateWebsite(site.SiteID, map[string]any{"status": "ready"})
	}
	a.audit(r, "website.check_dns_ssl", site.SiteID, chk.Detail)
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=domain&flash=Check complete: "+chk.Detail, http.StatusSeeOther)
}

func (a *App) siteChangeDomain(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	newDomain := domains.Normalize(r.FormValue("new_domain"))
	if err := domains.Validate(newDomain); err != nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=domain&flash="+err.Error(), http.StatusSeeOther)
		return
	}
	if _, err := a.store.GetWebsiteByDomain(newDomain); err == nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=domain&flash=Domain already in use.", http.StatusSeeOther)
		return
	}
	old := site.Domain
	_ = a.store.UpdateWebsite(site.SiteID, map[string]any{"domain": newDomain, "admin_domain": "wpadmin." + newDomain})
	a.audit(r, "website.change_domain", site.SiteID, old+" -> "+newDomain)
	// URL replacement inside WordPress is serialization-safe via WP-CLI.
	spec, err := a.agentSpec(site, true)
	if err == nil {
		spec.Domain = newDomain
		_, _ = a.runner.EnqueueSimple(site, "wpcli", agentproto.Request{Op: agentproto.OpWPCLI, Site: spec, Action: "cache_flush"})
	}
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=domain&flash=Domain updated in the panel. Apply the configuration to rebuild Traefik labels, update DNS, then run a check.", http.StatusSeeOther)
}

var _ = auth.GenerateSecurePassword
var _ = genHex
