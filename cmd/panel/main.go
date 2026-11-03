// Own hPanel - unprivileged web UI + API.
//
// This process NEVER opens the Docker socket. All privileged work goes to
// the provisioning agent over its Unix socket (see internal/agentclient).
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentclient"
	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/auth"
	"github.com/Udhayaboopathi/hPanel-wp/internal/dbconfig"
	"github.com/Udhayaboopathi/hPanel-wp/internal/provisioning"
	"github.com/Udhayaboopathi/hPanel-wp/internal/secrets"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/web"
)

type Config struct {
	Addr          string
	DataDir       string
	SitesDir      string
	BackupsDir    string
	AgentSocket   string
	AgentToken    string
	ServerIP      string
	PanelDomain   string
	SecureCookies bool
	// SiteDBHost is the hostname written into generated site configs as
	// WORDPRESS_DB_HOST: the dedicated MariaDB container's name (DB_CONTAINER),
	// reachable from WP containers on their per-site network (port 3306).
	SiteDBHost string
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}

type App struct {
	cfg    Config
	store  *store.Store
	box    *secrets.Box
	agent  *agentclient.Client
	runner *provisioning.Runner
	tmpl   *template.Template
	log    *slog.Logger

	mu      sync.Mutex
	pending map[string]pendingLogin // totp/recovery pending state, memory only
	once    map[string]string       // one-time display values (TOTP seed, recovery codes): shown once, never in URLs
}

type pendingLogin struct {
	AdminID   int64
	ExpiresAt time.Time
}

type pageData struct {
	Title    string
	Admin    *store.Admin
	CSRF     string
	Flash    string
	Error    string
	Data     any
	ServerIP string
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	dataDir := envOr("PANEL_DATA_DIR", "./data")
	cfg := Config{
		Addr:          envOr("PANEL_ADDR", ":8080"),
		DataDir:       dataDir,
		SitesDir:      envOr("SITES_DIR", "/opt/own-hpanel/websites"),
		BackupsDir:    envOr("BACKUPS_DIR", "/opt/own-hpanel/backups"),
		AgentSocket:   envOr("AGENT_SOCKET", "/run/own-hpanel/agent.sock"),
		AgentToken:    envOr("AGENT_TOKEN_FILE", "/opt/own-hpanel/secrets/agent.token"),
		ServerIP:      envOr("SERVER_IP", "130.210.42.20"),
		PanelDomain:   os.Getenv("PANEL_DOMAIN"),
		SecureCookies: envBool("PANEL_SECURE_COOKIES", true), // secure by default; explicit opt-out only for local dev
		SiteDBHost:    envOr("DB_CONTAINER", dbconfig.DefaultContainer),
	}
	st, err := openStore(log, dataDir)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	box, err := secrets.LoadOrCreateKey(dataDir)
	if err != nil {
		log.Error("encryption key", "err", err)
		os.Exit(1)
	}
	agent, err := agentclient.New(cfg.AgentSocket, cfg.AgentToken)
	if err != nil {
		// Panel must still run (login, dashboard, jobs history) without the agent.
		log.Warn("agent client not configured; provisioning will report agent-unavailable", "err", err)
		agent = nil
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
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			return t.Local().Format("2006-01-02 15:04")
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		log.Error("parse templates", "err", err)
		os.Exit(1)
	}
	app := &App{cfg: cfg, store: st, box: box, agent: agent, tmpl: tmpl, log: log, pending: map[string]pendingLogin{}, once: map[string]string{}}
	app.runner = provisioning.New(st, agent)

	mux := http.NewServeMux()
	staticSub, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /setup", app.setupForm)
	mux.HandleFunc("POST /setup", app.setupSubmit)
	mux.HandleFunc("GET /login", app.loginForm)
	mux.HandleFunc("POST /login", app.loginSubmit)
	mux.HandleFunc("GET /login/totp", app.totpForm)
	mux.HandleFunc("POST /login/totp", app.totpSubmit)
	mux.HandleFunc("GET /login/recover", app.recoverForm)
	mux.HandleFunc("POST /login/recover", app.recoverSubmit)
	mux.HandleFunc("POST /logout", app.requireAuth(app.logout))
	mux.HandleFunc("POST /logout-all", app.requireAuth(app.logoutAll))
	mux.HandleFunc("GET /{$}", app.requireAuth(app.dashboard))
	// KR Host canonical routes (Stitch UI). The legacy paths below
	// them keep working; both spellings serve the same handlers.
	mux.HandleFunc("GET /dashboard", app.requireAuth(app.dashboard))
	mux.HandleFunc("GET /deployments", app.requireAuth(app.jobsList))
	mux.HandleFunc("GET /deployments/{id}", app.requireAuth(app.jobDetail))
	mux.HandleFunc("POST /deployments/{id}/retry", app.requireAuth(app.jobRetry))
	mux.HandleFunc("POST /deployments/{id}/cancel", app.requireAuth(app.jobCancel))
	mux.HandleFunc("GET /websites/new/details", app.requireAuth(app.createForm))
	mux.HandleFunc("GET /websites/new/admin", app.requireAuth(app.createForm))
	mux.HandleFunc("GET /websites/new/review", app.requireAuth(app.createForm))
	mux.HandleFunc("GET /websites/new/progress", app.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/deployments", http.StatusFound)
	}))
	mux.HandleFunc("GET /server", app.requireAuth(app.monitoringPage))
	mux.HandleFunc("GET /server/containers", app.requireAuth(app.containersPage))
	mux.HandleFunc("GET /logs", app.requireAuth(app.logsPage))
	mux.HandleFunc("GET /audit-logs", app.requireAuth(app.auditList))
	mux.HandleFunc("GET /settings/security", app.requireAuth(app.accountPage))
	mux.HandleFunc("GET /websites/import", app.requireAuth(app.importPage))
	mux.HandleFunc("POST /websites/import", app.requireAuth(app.importSubmit))
	mux.HandleFunc("GET /websites/{site}/wordpress", app.requireAuth(app.siteDetailTab("wp")))
	mux.HandleFunc("GET /websites/{site}/domains", app.requireAuth(app.siteDetailTab("domain")))
	mux.HandleFunc("GET /websites/{site}/performance", app.requireAuth(app.performancePage))
	mux.HandleFunc("GET /websites/{site}/files", app.requireAuth(app.siteDetailTab("files")))
	mux.HandleFunc("GET /websites/{site}/environment", app.requireAuth(app.siteDetailTab("env")))
	mux.HandleFunc("GET /websites/{site}/compose", app.requireAuth(app.siteDetailTab("yaml")))
	mux.HandleFunc("GET /websites/{site}/database", app.requireAuth(app.siteDetailTab("db")))
	mux.HandleFunc("GET /websites/{site}/backups", app.requireAuth(app.siteDetailTab("backups")))
	mux.HandleFunc("GET /websites/{site}/jobs", app.requireAuth(app.siteDetailTab("jobs")))
	mux.HandleFunc("GET /websites/{site}/restore", app.requireAuth(app.restorePage))
	mux.HandleFunc("GET /websites/{site}/settings", app.requireAuth(app.siteSettingsPage))
	mux.HandleFunc("GET /websites", app.requireAuth(app.websitesList))
	mux.HandleFunc("GET /websites/new", app.requireAuth(app.createForm))
	mux.HandleFunc("POST /websites/preview", app.requireAuth(app.createPreview))
	mux.HandleFunc("POST /websites/create", app.requireAuth(app.createSubmit))
	mux.HandleFunc("GET /api/generate-password", app.requireAuth(app.generatePassword))
	mux.HandleFunc("GET /websites/{site}", app.requireAuth(app.siteDetail))
	mux.HandleFunc("POST /websites/{site}/start", app.requireAuth(app.siteAction("start_site", agentproto.OpStartSite)))
	mux.HandleFunc("POST /websites/{site}/stop", app.requireAuth(app.siteAction("stop_site", agentproto.OpStopSite)))
	mux.HandleFunc("POST /websites/{site}/restart", app.requireAuth(app.siteAction("restart_site", agentproto.OpRestartSite)))
	mux.HandleFunc("POST /websites/{site}/delete", app.requireAuth(app.siteDelete))
	mux.HandleFunc("POST /websites/{site}/backup", app.requireAuth(app.siteBackup))
	mux.HandleFunc("POST /websites/{site}/repair", app.requireAuth(app.siteRepair))
	mux.HandleFunc("POST /websites/{site}/backups/{id}/restore", app.requireAuth(app.siteRestore))
	mux.HandleFunc("GET /websites/{site}/backups/{id}/download", app.requireAuth(app.backupDownload))
	mux.HandleFunc("POST /websites/{site}/wp", app.requireAuth(app.siteWP))
	mux.HandleFunc("POST /websites/{site}/reset-admin-password", app.requireAuth(app.siteResetAdminPW))
	mux.HandleFunc("POST /websites/{site}/env", app.requireAuth(app.siteEnvSave))
	mux.HandleFunc("POST /websites/{site}/yaml", app.requireAuth(app.siteYAMLSave))
	mux.HandleFunc("POST /websites/{site}/yaml-rollback", app.requireAuth(app.siteYAMLRollback))
	mux.HandleFunc("POST /websites/{site}/apply-config", app.requireAuth(app.siteApplyConfig))
	mux.HandleFunc("POST /websites/{site}/db-rotate", app.requireAuth(app.siteDBRotate))
	mux.HandleFunc("POST /websites/{site}/check", app.requireAuth(app.siteCheck))
	mux.HandleFunc("POST /websites/{site}/change-domain", app.requireAuth(app.siteChangeDomain))
	mux.HandleFunc("GET /websites/{site}/files/download", app.requireAuth(app.fileDownload))
	mux.HandleFunc("POST /websites/{site}/files/save", app.requireAuth(app.fileSave))
	mux.HandleFunc("POST /websites/{site}/files/upload", app.requireAuth(app.fileUpload))
	mux.HandleFunc("POST /websites/{site}/files/mkdir", app.requireAuth(app.fileMkdir))
	mux.HandleFunc("POST /websites/{site}/files/rename", app.requireAuth(app.fileRename))
	mux.HandleFunc("POST /websites/{site}/files/delete", app.requireAuth(app.fileDelete))
	mux.HandleFunc("GET /websites/{site}/logs/view", app.requireAuth(app.siteLogs))
	mux.HandleFunc("GET /jobs", app.requireAuth(app.jobsList))
	mux.HandleFunc("GET /jobs/{id}", app.requireAuth(app.jobDetail))
	mux.HandleFunc("POST /jobs/{id}/cancel", app.requireAuth(app.jobCancel))
	mux.HandleFunc("POST /jobs/{id}/retry", app.requireAuth(app.jobRetry))
	mux.HandleFunc("GET /backups", app.requireAuth(app.backupsList))
	mux.HandleFunc("GET /monitoring", app.requireAuth(app.monitoringPage))
	mux.HandleFunc("GET /audit", app.requireAuth(app.auditList))
	mux.HandleFunc("GET /account", app.requireAuth(app.accountPage))
	mux.HandleFunc("POST /account/password", app.requireAuth(app.accountPassword))
	mux.HandleFunc("POST /account/totp/setup", app.requireAuth(app.accountTOTPSetup))
	mux.HandleFunc("POST /account/totp/enable", app.requireAuth(app.accountTOTPEnable))
	mux.HandleFunc("POST /account/totp/disable", app.requireAuth(app.accountTOTPDisable))
	mux.HandleFunc("POST /account/recovery/regenerate", app.requireAuth(app.accountRecovery))
	mux.HandleFunc("GET /settings", app.requireAuth(app.settingsPage))
	mux.HandleFunc("POST /settings", app.requireAuth(app.settingsSave))
	mux.HandleFunc("GET /api/metrics", app.requireAuth(app.apiMetrics))
	mux.HandleFunc("GET /api/logs", app.requireAuth(app.apiLogs))
	mux.HandleFunc("GET /api/websites", app.requireAuth(app.apiWebsites))
	mux.HandleFunc("GET /api/jobs/{id}", app.requireAuth(app.apiJob))
	mux.HandleFunc("/", app.notFound)

	// Continuous resource sampler for the live graphs (dashboard,
	// server, per-site performance).
	go app.sampleLoop()

	// Periodic housekeeping: expire sessions, prune samples and old
	// history rows. (DB sizes are refreshed on the website detail
	// page via the agent's OpDBSize.)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			_ = st.DeleteExpiredSessions()
			_ = st.PruneResourceSamples(7)
			_ = st.PruneHistory(90)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           securityHeaders(limitBody(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("own-hpanel panel listening", "addr", cfg.Addr, "agent_available", agent != nil && agent.Available())
	if err := srv.Serve(ln); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// openStore selects the panel metadata store. Default is SQLite in the
// data dir (local development). In production PANEL_STORE_DRIVER=mysql
// stores panel metadata in the dedicated wordpress-mariadb container of
// the hPanel stack, in its own database (PANEL_DB_NAME, default
// hpanel_panel), reached through DB_HOST:DB_EXTERNAL_PORT with the common
// admin account - the same container every WordPress site uses. In the
// compose stack DB_HOST/DB_EXTERNAL_PORT are pinned to the internal
// wordpress-mariadb:3306 endpoint. PANEL_STORE_DSN overrides the built
// DSN.
func openStore(log *slog.Logger, dataDir string) (*store.Store, error) {
	driver := envOr("PANEL_STORE_DRIVER", "sqlite")
	switch driver {
	case "sqlite":
		return store.Open(filepath.Join(dataDir, "panel.db"))
	case "mysql":
		dsn := os.Getenv("PANEL_STORE_DSN")
		if dsn == "" {
			dbCfg, err := dbconfig.FromEnv()
			if err != nil {
				return nil, err
			}
			if dbCfg.AdminPassword == "" {
				return nil, fmt.Errorf("PANEL_STORE_DRIVER=mysql but no DB admin credential is configured (set DB_ADMIN_PASSWORD_FILE or DB_ADMIN_PASSWORD, or supply PANEL_STORE_DSN)")
			}
			port := dbCfg.ExternalPort
			dsn = store.BuildMySQLDSN(dbCfg.Host, port,
				envOr("PANEL_DB_NAME", dbconfig.DefaultPanelDBName),
				dbCfg.AdminUser, dbCfg.AdminPassword)
		}
		st, err := store.OpenMySQL(dsn)
		if err != nil {
			return nil, err
		}
		log.Info("panel store: mysql (dedicated wordpress-mariadb)", "db", envOr("PANEL_DB_NAME", dbconfig.DefaultPanelDBName))
		return st, nil
	default:
		return nil, fmt.Errorf("unknown PANEL_STORE_DRIVER %q (want sqlite or mysql)", driver)
	}
}

// limitBody caps request bodies so a single request cannot fill
// the panel host's disk. ParseMultipartForm spools file parts to
// temp files as it parses, so without this ceiling the panel's
// per-file caps — enforced only when copying OUT of the spool —
// could never honour their guarantee. Multipart uploads (site
// imports, the file manager) get room for the panel's own caps
// (2 GB archive + 1 GB database file) plus form overhead; every
// other write is a small form post.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			limit := int64(2 << 20)
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				limit = (3 << 30) + (256 << 20)
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func humanBytes(v any) string {
	var n uint64
	switch x := v.(type) {
	case uint64:
		n = x
	case int64:
		if x > 0 {
			n = uint64(x)
		}
	case int:
		if x > 0 {
			n = uint64(x)
		}
	case float64:
		if x > 0 {
			n = uint64(x)
		}
	default:
		return fmt.Sprintf("%v", v)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---- rendering & request helpers ----

func (a *App) render(w http.ResponseWriter, name string, d pageData) {
	d.ServerIP = a.cfg.ServerIP
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Rendered pages carry sensitive one-time material (recovery
	// codes, TOTP seeds, generated passwords) and live account
	// state: they must never be stored by a browser or proxy cache.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if err := a.tmpl.ExecuteTemplate(w, name, d); err != nil {
		a.log.Error("render", "template", name, "err", err)
	}
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	a.render(w, "error.html", pageData{Title: "Not found", Error: "The page you requested does not exist."})
}

func clientIP(r *http.Request) string {
	// The panel sits behind Traefik, which APPENDS the real client IP
	// to X-Forwarded-For. The LAST hop is therefore the value our own
	// proxy vouches for; the first hop is whatever the client claimed
	// and is trivially spoofable. (Rate limiting also matches on
	// username, so this is attribution hygiene, not a sole control.)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- auth plumbing ----

type ctxKey int

const ctxAdmin ctxKey = 1

func (a *App) session(r *http.Request) (*store.Session, *store.Admin) {
	c, err := r.Cookie("own_hpanel_session")
	if err != nil || c.Value == "" {
		return nil, nil
	}
	sess, err := a.store.GetSession(auth.HashToken(c.Value))
	if err != nil {
		return nil, nil
	}
	admin, err := a.store.GetAdminByID(sess.AdminID)
	if err != nil {
		return nil, nil
	}
	return sess, admin
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Fail closed: a store error must not be read as "no
		// administrators yet" (which would route an authenticated
		// area into the first-run setup flow).
		if n, err := a.store.CountAdmins(); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		} else if n == 0 {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		sess, admin := a.session(r)
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// CSRF: every authenticated POST must carry the session CSRF token.
		if r.Method == http.MethodPost {
			// For multipart bodies, ParseForm alone leaves r.Form
			// holding only the query string, so a csrf_token field
			// inside an upload form would be invisible to FormValue
			// and every file upload would 403. Parse the multipart
			// body first (file parts beyond the memory cap spill to
			// temp files; the handler re-reads them from
			// r.MultipartForm and cleans them up).
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				_ = r.ParseMultipartForm(32 << 20)
			} else {
				_ = r.ParseForm()
			}
			got := r.FormValue("csrf_token")
			if got == "" {
				got = r.Header.Get("X-CSRF-Token")
			}
			if len(got) != len(sess.CSRFToken) || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
				http.Error(w, "CSRF validation failed", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxAdmin, admin)
		ctx = context.WithValue(ctx, ctxKey(2), sess)
		next(w, r.WithContext(ctx))
	}
}

func adminFrom(r *http.Request) *store.Admin {
	admin, _ := r.Context().Value(ctxAdmin).(*store.Admin)
	return admin
}
func sessionFrom(r *http.Request) *store.Session {
	sess, _ := r.Context().Value(ctxKey(2)).(*store.Session)
	return sess
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request, admin *store.Admin) error {
	plain, hash, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	csrf := randomToken()
	if err := a.store.CreateSession(hash, admin.ID, csrf, clientIP(r), r.UserAgent(), auth.NewSessionExpiry()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: "own_hpanel_session", Value: plain, Path: "/",
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		Expires: auth.NewSessionExpiry(),
	})
	return nil
}

// preCSRF issues/reads the double-submit token that protects the
// UNAUTHENTICATED forms (setup, login, TOTP, recovery): those routes
// cannot use the session CSRF token because no session exists yet.
// A cross-site POST cannot read the cookie, so it cannot forge the
// matching form field. This closes login-CSRF and, more importantly,
// a cross-site submission against the one-shot first-run setup.
func (a *App) preCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("own_hpanel_pre_csrf"); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	tok := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: "own_hpanel_pre_csrf", Value: tok, Path: "/",
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: 3600,
	})
	return tok
}

func (a *App) validPreCSRF(r *http.Request) bool {
	c, err := r.Cookie("own_hpanel_pre_csrf")
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.FormValue("csrf_token"))) == 1
}

func (a *App) setPending(w http.ResponseWriter, adminID int64) {
	tok := randomToken()
	a.mu.Lock()
	a.pending[tok] = pendingLogin{AdminID: adminID, ExpiresAt: time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "own_hpanel_pending", Value: tok, Path: "/",
		HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: 600,
	})
}

func (a *App) getPending(r *http.Request) (int64, bool) {
	c, err := r.Cookie("own_hpanel_pending")
	if err != nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[c.Value]
	if !ok {
		return 0, false
	}
	if time.Now().After(p.ExpiresAt) {
		// Delete expired entries on sight: without this the
		// pending map grows by one entry per password-valid login
		// that never completes (or abandons) the TOTP step.
		delete(a.pending, c.Value)
		return 0, false
	}
	return p.AdminID, true
}

func (a *App) clearPending(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("own_hpanel_pending"); err == nil {
		a.mu.Lock()
		delete(a.pending, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "own_hpanel_pending", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func (a *App) audit(r *http.Request, action, target, detail string) {
	adminID := int64(0)
	if admin := adminFrom(r); admin != nil {
		adminID = admin.ID
	}
	_ = a.store.AddAudit(adminID, action, target, detail, clientIP(r))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var _ = sql.NullInt64{}
