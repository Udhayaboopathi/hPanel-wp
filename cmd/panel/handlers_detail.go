package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/configgen"
	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
)

// Approved editable environment keys (schema-restricted editor).
var approvedEnvKeys = []string{"PHP_MEMORY_LIMIT", "UPLOAD_MAX_FILESIZE", "POST_MAX_SIZE", "WORDPRESS_DEBUG", "SITE_TITLE"}

type fileEntry struct {
	Name      string
	Path      string
	Size      int64
	IsDir     bool
	Sensitive bool
	Editable  bool
}

type detailData struct {
	Site        *store.Website
	Tab         string
	SSL         *store.SSLCheck
	Jobs        []*store.Job
	Backups     []*store.Backup
	Configs     []*store.ConfigVersion
	Latest      *store.ConfigVersion
	EnvMap      map[string]string
	Files       []fileEntry
	FilePath    string
	FileContent string
	FileView    string
	Logs        string
	DBSize      int64
	AgentUp     bool
}

func (a *App) siteRoot(site *store.Website) string {
	return filepath.Join(a.cfg.SitesDir, site.SiteID, "data")
}

func (a *App) siteDetail(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "overview"
	}
	d := detailData{Site: site, Tab: tab, AgentUp: a.agentUp(), DBSize: a.store.DBSize(site.SiteID)}
	if chk, err := a.store.LatestSSLCheck(site.Domain); err == nil {
		d.SSL = chk
	}
	d.Jobs, _ = a.store.ListJobsForSite(site.SiteID)
	d.Backups, _ = a.store.ListBackups(site.SiteID, 50)
	d.Configs, _ = a.store.ListConfigVersions(site.ID)
	if latest, err := a.store.LatestConfig(site.ID); err == nil {
		d.Latest = latest
		d.EnvMap = map[string]string{}
		_ = json.Unmarshal([]byte(latest.EnvJSON), &d.EnvMap)
	}
	if tab == "files" {
		rel := r.URL.Query().Get("path")
		root := a.siteRoot(site)
		abs, err := security.SafeJoin(root, rel)
		if err != nil {
			d.FilePath = ""
			d.Logs = "Invalid path: " + err.Error()
		} else {
			d.FilePath = rel
			if st, err := os.Stat(abs); err == nil && st.IsDir() {
				entries, _ := os.ReadDir(abs)
				for _, e := range entries {
					info, _ := e.Info()
					var size int64
					if info != nil {
						size = info.Size()
					}
					childRel := filepath.Join(rel, e.Name())
					d.Files = append(d.Files, fileEntry{
						Name: e.Name(), Path: childRel, Size: size, IsDir: e.IsDir(),
						Sensitive: security.IsSensitiveFile(e.Name()),
						Editable:  !e.IsDir() && security.EditableTextFile(e.Name(), size),
					})
				}
				sort.Slice(d.Files, func(i, j int) bool {
					if d.Files[i].IsDir != d.Files[j].IsDir {
						return d.Files[i].IsDir
					}
					return d.Files[i].Name < d.Files[j].Name
				})
			}
			if view := r.URL.Query().Get("view"); view != "" {
				viewAbs, err := security.SafeJoin(root, view)
				if err == nil && !security.IsSensitiveFile(view) {
					if b, err := os.ReadFile(viewAbs); err == nil && len(b) <= 1<<20 {
						d.FileView = view
						d.FileContent = string(b)
					}
				}
			}
		}
	}
	a.render(w, "detail.html", pageData{Title: site.Domain, Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken, Data: d, Flash: r.URL.Query().Get("flash")})
}

func (a *App) siteLogs(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	// Container logs come from the agent (the panel has no Docker access).
	if a.agent != nil && a.agent.Available() {
		if spec, err := a.agentSpec(site, false); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpLogs, Site: spec})
			cancel()
			if err == nil && resp.OK && resp.Output != "" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				fmt.Fprint(w, resp.Output)
				return
			}
		}
	}
	// Fallback: deployment/job events for this site.
	jobs, _ := a.store.ListJobsForSite(site.SiteID)
	var sb strings.Builder
	for _, j := range jobs {
		events, _ := a.store.ListEvents(j.ID, 100)
		for _, e := range events {
			fmt.Fprintf(&sb, "%s [%s] %s: %s\n", e.CreatedAt, e.Level, j.Kind, e.Message)
		}
	}
	if sb.Len() == 0 {
		sb.WriteString("No deployment events recorded yet for this website.")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, sb.String())
}

// ---------- env editor ----------

func (a *App) siteEnvSave(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	env := map[string]string{}
	for _, k := range approvedEnvKeys {
		v := strings.TrimSpace(r.FormValue("env_" + k))
		if v == "" {
			continue
		}
		switch k {
		case "PHP_MEMORY_LIMIT", "UPLOAD_MAX_FILESIZE", "POST_MAX_SIZE":
			if !strings.HasSuffix(v, "M") && !strings.HasSuffix(v, "G") {
				http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=env&flash="+k+" must end in M or G (e.g. 256M).", http.StatusSeeOther)
				return
			}
		case "WORDPRESS_DEBUG":
			if v != "0" && v != "1" {
				http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=env&flash=WORDPRESS_DEBUG must be 0 or 1.", http.StatusSeeOther)
				return
			}
		}
		env[k] = v
	}
	latest, _ := a.store.LatestConfig(site.ID)
	composeYAML := ""
	if latest != nil {
		composeYAML = latest.ComposeYAML
	}
	envJSON, _ := json.Marshal(env)
	if _, err := a.store.SaveConfigVersion(site.ID, string(envJSON), composeYAML, "environment edit (not yet applied)"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.env_edit", site.SiteID, "keys updated; apply configuration to take effect")
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=env&flash=Environment saved as a new config version. Use Apply Configuration to rebuild the container.", http.StatusSeeOther)
}

// ---------- YAML editor ----------

func diffLines(old, new string) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")
	oldSet := map[string]int{}
	for _, l := range oldLines {
		oldSet[l]++
	}
	newSet := map[string]int{}
	for _, l := range newLines {
		newSet[l]++
	}
	var sb strings.Builder
	for _, l := range newLines {
		if oldSet[l] == 0 {
			sb.WriteString("+ " + l + "\n")
		}
	}
	for _, l := range oldLines {
		if newSet[l] == 0 {
			sb.WriteString("- " + l + "\n")
		}
	}
	if sb.Len() == 0 {
		return "(no changes)"
	}
	return sb.String()
}

func (a *App) siteYAMLSave(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	yamlText := r.FormValue("compose_yaml")
	siteDir := filepath.Join(a.cfg.SitesDir, site.SiteID)
	if err := security.ValidateComposePolicy(yamlText, siteDir); err != nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=yaml&flash=Rejected: "+err.Error(), http.StatusSeeOther)
		return
	}
	latest, _ := a.store.LatestConfig(site.ID)
	envJSON := "{}"
	if latest != nil {
		envJSON = latest.EnvJSON
	}
	diff := diffLines(latestYAML(latest), yamlText)
	if _, err := a.store.SaveConfigVersion(site.ID, envJSON, yamlText, "compose edit (draft)"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.yaml_edit", site.SiteID, "compose saved as new version")
	a.render(w, "yaml_diff.html", pageData{Title: "Configuration diff", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
		Data: map[string]any{"Site": site, "Diff": diff}})
}

func latestYAML(c *store.ConfigVersion) string {
	if c == nil {
		return ""
	}
	return c.ComposeYAML
}

func (a *App) siteYAMLRollback(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	version, _ := strconvAtoi(r.FormValue("version"))
	var target *store.ConfigVersion
	configs, _ := a.store.ListConfigVersions(site.ID)
	for _, c := range configs {
		if c.Version == version {
			target = c
			break
		}
	}
	if target == nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=yaml&flash=Config version not found.", http.StatusSeeOther)
		return
	}
	if _, err := a.store.SaveConfigVersion(site.ID, target.EnvJSON, target.ComposeYAML, fmt.Sprintf("rollback to version %d", version)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "website.yaml_rollback", site.SiteID, fmt.Sprintf("rolled back to version %d", version))
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=yaml&flash=Rolled back. Apply Configuration to rebuild the container.", http.StatusSeeOther)
}

func strconvAtoi(s string) (int, error) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid number")
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

func (a *App) siteApplyConfig(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	latest, err := a.store.LatestConfig(site.ID)
	if err != nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=yaml&flash=No saved configuration to apply.", http.StatusSeeOther)
		return
	}
	siteDir := filepath.Join(a.cfg.SitesDir, site.SiteID)
	if err := security.ValidateComposePolicy(latest.ComposeYAML, siteDir); err != nil {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=yaml&flash=Latest config fails policy validation: "+err.Error(), http.StatusSeeOther)
		return
	}
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Rebuild env text from the approved env schema only.
	env := map[string]string{}
	_ = json.Unmarshal([]byte(latest.EnvJSON), &env)
	profile, _ := configgen.ProfileFor(site.ResourceProfile, site.CPULimit, site.MemLimitMB)
	cs := configgen.SiteSpec{
		SiteID: site.SiteID, Domain: site.Domain, AdminDomain: site.AdminDomain, Title: site.Title,
		DBName: site.DBName, DBUser: site.DBUser, DBPassword: spec.DBPassword, DBHost: "wordpress-mariadb",
		PHPVersion: site.PHPVersion, Profile: profile, SiteDir: site.SiteDir,
		ContainerName: site.ContainerName, Network: websites.NetworkName(site.SiteID),
		PHPMemory: env["PHP_MEMORY_LIMIT"], UploadSize: env["UPLOAD_MAX_FILESIZE"], Debug: env["WORDPRESS_DEBUG"] == "1",
	}
	spec.ComposeYAML = latest.ComposeYAML
	spec.EnvText = configgen.GenerateEnv(cs)
	a.audit(r, "website.apply_config", site.SiteID, "applying latest config version")
	job, err := a.runner.EnqueueSimple(site, "apply_config", agentproto.Request{Op: agentproto.OpApplyConfig, Site: spec})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/jobs/"+job.ID, http.StatusSeeOther)
}

func (a *App) siteDBRotate(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	newPW, err := genDBPassword()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	spec, err := a.agentSpec(site, true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if a.agent == nil || !a.agent.Available() {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=db&flash=Provisioning agent is not available; password not changed.", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := a.agent.Call(ctx, agentproto.Request{Op: agentproto.OpRotateDBPassword, Site: spec, NewDBPassword: newPW})
	if err != nil || !resp.OK {
		msg := "rotation failed"
		if err != nil {
			msg = err.Error()
		} else if resp.Error != "" {
			msg = resp.Error
		}
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=db&flash="+msg, http.StatusSeeOther)
		return
	}
	enc, err := a.box.Encrypt(newPW)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = a.store.UpdateWebsite(site.SiteID, map[string]any{"db_password_enc": enc})
	a.audit(r, "website.db_rotate_password", site.SiteID, "database password rotated")
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=db&flash=Database password rotated in MariaDB. Restart the website to apply the updated credentials.", http.StatusSeeOther)
}

// ---------- file manager ----------

func (a *App) fileDownload(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	rel := r.URL.Query().Get("path")
	if security.IsSensitiveFile(rel) {
		http.Error(w, "sensitive files are not downloadable from the panel", 403)
		return
	}
	abs, err := security.SafeJoin(a.siteRoot(site), rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		http.Error(w, "file not found", 404)
		return
	}
	a.audit(r, "file.download", site.SiteID, "path="+rel)
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(abs))
	http.ServeFile(w, r, abs)
}

func (a *App) fileSave(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	rel := r.FormValue("path")
	if security.IsSensitiveFile(rel) {
		http.Error(w, "sensitive files cannot be edited from the panel", 403)
		return
	}
	abs, err := security.SafeJoin(a.siteRoot(site), rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	content := r.FormValue("content")
	if len(content) > 1<<20 || !security.EditableTextFile(rel, int64(len(content))) {
		http.Error(w, "file is not editable (type or size)", 400)
		return
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(abs); err == nil {
		mode = st.Mode()
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "file.edit", site.SiteID, "path="+rel)
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&path="+filepath.Dir(rel)+"&flash=File saved.", http.StatusSeeOther)
}

func (a *App) fileUpload(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	// Stream the upload (32 MB memory cap for the multipart envelope only).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	dir := r.FormValue("path")
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer file.Close()
	if security.IsSensitiveFile(hdr.Filename) {
		http.Error(w, "refusing to upload a sensitive filename", 403)
		return
	}
	abs, err := security.SafeJoin(a.siteRoot(site), filepath.Join(dir, filepath.Base(hdr.Filename)))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	out, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "file.upload", site.SiteID, "path="+filepath.Join(dir, filepath.Base(hdr.Filename)))
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&path="+dir+"&flash=Upload complete.", http.StatusSeeOther)
}

func (a *App) fileMkdir(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	abs, err := security.SafeJoin(a.siteRoot(site), filepath.Join(r.FormValue("path"), r.FormValue("name")))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "file.mkdir", site.SiteID, "path="+r.FormValue("name"))
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&path="+r.FormValue("path"), http.StatusSeeOther)
}

func (a *App) fileRename(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	root := a.siteRoot(site)
	from, err := security.SafeJoin(root, r.FormValue("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	to, err := security.SafeJoin(root, filepath.Join(filepath.Dir(r.FormValue("path")), r.FormValue("new_name")))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := os.Rename(from, to); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "file.rename", site.SiteID, "from="+r.FormValue("path")+" to="+r.FormValue("new_name"))
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&path="+filepath.Dir(r.FormValue("path")), http.StatusSeeOther)
}

func (a *App) fileDelete(w http.ResponseWriter, r *http.Request) {
	site := a.getSiteOr404(w, r)
	if site == nil {
		return
	}
	_ = r.ParseForm()
	rel := r.FormValue("path")
	if r.FormValue("confirm") != "DELETE" {
		http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&flash=Type DELETE in the confirmation field to delete a file.", http.StatusSeeOther)
		return
	}
	abs, err := security.SafeJoin(a.siteRoot(site), rel)
	if err != nil || abs == a.siteRoot(site) {
		http.Error(w, "invalid path", 400)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.audit(r, "file.delete", site.SiteID, "path="+rel)
	http.Redirect(w, r, "/websites/"+site.SiteID+"?tab=files&path="+filepath.Dir(rel)+"&flash=Deleted.", http.StatusSeeOther)
}

var _ = auth.GenerateSecurePassword
