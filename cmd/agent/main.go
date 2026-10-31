// Own hPanel provisioning agent.
//
// This is the ONLY component with access to the Docker socket. It listens
// on a Unix domain socket, requires a bearer token from a 0600 file, accepts
// only the allowlisted operations in internal/agentproto, re-validates every
// request, and refuses to mutate any container that is not labelled
// own-hpanel.managed=true (production guard). It never touches the existing
// production containers (redberyl-*, construction-*, krweb-*, runners).
//
// The ONE special case is the stack's own dedicated MariaDB container
// (DB_CONTAINER, default wordpress-mariadb; defined by compose.yaml and
// labelled own-hpanel.managed=true + own-hpanel.component=mariadb): the
// agent may exec the mariadb SQL client in it (docker.ExecDB - the only
// exec target ExecDB accepts) and connect it to new per-site networks.
// The agent must NEVER stop, restart, remove or recreate it directly -
// its lifecycle belongs to Docker Compose alone. That rule is enforced
// by refuseDBContainer and covered by tests. The pre-existing production
// construction-mariadb is not the DB container anymore and is never
// touched at all.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/backups"
	"github.com/Udhayaboopathi/hPanel-wp/internal/configgen"
	"github.com/Udhayaboopathi/hPanel-wp/internal/database"
	"github.com/Udhayaboopathi/hPanel-wp/internal/dbconfig"
	"github.com/Udhayaboopathi/hPanel-wp/internal/docker"
	"github.com/Udhayaboopathi/hPanel-wp/internal/domains"
	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
	"github.com/Udhayaboopathi/hPanel-wp/internal/wordpress"
)

const (
	traefikNetwork = "redberyl-internal"
	serverIP       = "130.210.42.20"
)

type Agent struct {
	docker     *docker.Client
	token      string
	sitesDir   string
	backupsDir string
	// db describes the ONE dedicated MariaDB container of the hPanel stack
	// (wordpress-mariadb). The agent connects to it as the common admin
	// user; the agent itself never lifecycle-manages it (Compose does).
	db  dbconfig.Config
	log *slog.Logger
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	socketPath := envOr("AGENT_SOCKET", "/run/own-hpanel/agent.sock")
	tokenFile := envOr("AGENT_TOKEN_FILE", "/opt/own-hpanel/secrets/agent.token")
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		log.Error("cannot read agent token file", "err", err)
		os.Exit(1)
	}
	// Dedicated-MariaDB configuration. Legacy AGENT_MARIADB_* names are honoured
	// as a fallback so older systemd units keep working during the upgrade.
	if os.Getenv("DB_ADMIN_PASSWORD_FILE") == "" && os.Getenv("AGENT_MARIADB_ROOT_PASSWORD_FILE") != "" {
		_ = os.Setenv("DB_ADMIN_PASSWORD_FILE", os.Getenv("AGENT_MARIADB_ROOT_PASSWORD_FILE"))
	}
	if os.Getenv("DB_ADMIN_PASSWORD") == "" && os.Getenv("AGENT_MARIADB_ROOT_PASSWORD") != "" {
		_ = os.Setenv("DB_ADMIN_PASSWORD", os.Getenv("AGENT_MARIADB_ROOT_PASSWORD"))
	}
	dbCfg, err := dbconfig.FromEnv()
	if err != nil {
		log.Error("database configuration", "err", err)
		os.Exit(1)
	}
	a := &Agent{
		docker:     docker.New(envOr("DOCKER_SOCKET", "/var/run/docker.sock")),
		token:      strings.TrimSpace(string(tokenBytes)),
		sitesDir:   envOr("SITES_DIR", "/opt/own-hpanel/websites"),
		backupsDir: envOr("BACKUPS_DIR", "/opt/own-hpanel/backups"),
		db:         dbCfg,
		log:        log,
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		log.Error("socket dir", "err", err)
		os.Exit(1)
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	// The socket must be connectable by the panel container's user (fixed
	// uid/gid 10001, a different uid than the agent's root). Connect
	// permission is not the auth boundary: every operation requires the
	// bearer token, which stays in 0600/0640 files. So the socket itself
	// is 0666, like the Docker socket pattern used by group proxies.
	_ = os.Chmod(socketPath, 0o666)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /execute", a.auth(a.execute))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("own-hpanel agent listening", "socket", socketPath)
	if err := srv.Serve(ln); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func (a *Agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(a.token) || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			writeResp(w, http.StatusUnauthorized, agentproto.Response{Error: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func writeResp(w http.ResponseWriter, code int, resp agentproto.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *Agent) execute(w http.ResponseWriter, r *http.Request) {
	var req agentproto.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeResp(w, 400, agentproto.Response{Error: "invalid request body"})
		return
	}
	if !agentproto.Allowlist[req.Op] {
		writeResp(w, 400, agentproto.Response{Error: "operation " + req.Op + " is not allowlisted"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 11*time.Minute)
	defer cancel()
	var resp *agentproto.Response
	var err error
	switch req.Op {
	case agentproto.OpPing:
		err = a.docker.Ping(ctx)
		resp = &agentproto.Response{OK: err == nil}
	case agentproto.OpDiscovery:
		resp, err = a.discovery(ctx)
	case agentproto.OpCreateSite:
		resp, err = a.createSite(ctx, req)
	case agentproto.OpDeleteSite:
		resp, err = a.deleteSite(ctx, req)
	case agentproto.OpStartSite:
		resp, err = a.lifecycle(ctx, req, "start")
	case agentproto.OpStopSite:
		resp, err = a.lifecycle(ctx, req, "stop")
	case agentproto.OpRestartSite:
		resp, err = a.lifecycle(ctx, req, "restart")
	case agentproto.OpBackupSite:
		resp, err = a.backupSite(ctx, req)
	case agentproto.OpRestoreSite:
		resp, err = a.restoreSite(ctx, req)
	case agentproto.OpWPCLI:
		resp, err = a.wpcli(ctx, req)
	case agentproto.OpApplyConfig:
		resp, err = a.applyConfig(ctx, req)
	case agentproto.OpRotateDBPassword:
		resp, err = a.rotateDBPassword(ctx, req)
	case agentproto.OpDBSize:
		resp, err = a.dbSize(ctx, req)
	case agentproto.OpStats:
		resp, err = a.stats(ctx)
	case agentproto.OpLogs:
		resp, err = a.siteLogs(ctx, req)
	}
	if err != nil {
		if resp == nil {
			resp = &agentproto.Response{}
		}
		resp.OK = false
		if resp.Error == "" {
			resp.Error = err.Error()
		}
		a.log.Warn("operation failed", "op", req.Op, "site", req.Site.SiteID, "err", resp.Error)
		writeResp(w, 200, *resp) // operational failures are data, not transport errors
		return
	}
	writeResp(w, 200, *resp)
}

// ---- validation & guards ----

func validateSite(s agentproto.SiteSpec, requireSecrets bool) error {
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return err
	}
	if err := security.ValidateDomain(s.Domain); err != nil {
		return err
	}
	if s.AdminDomain != "" {
		if err := security.ValidateDomain(s.AdminDomain); err != nil {
			return err
		}
	}
	if err := security.ValidateDBName(s.DBName); err != nil {
		return err
	}
	if err := security.ValidateDBUser(s.DBUser); err != nil {
		return err
	}
	if requireSecrets && (s.DBPassword == "" || s.AdminPassword == "") {
		return fmt.Errorf("missing required secret material for provisioning")
	}
	if _, err := configgen.PHPImage(s.PHPVersion); err != nil {
		return err
	}
	_, err := configgen.ProfileFor(s.Profile, s.CPULimit, s.MemLimitMB)
	return err
}

func (a *Agent) siteDirFor(s agentproto.SiteSpec) (string, error) {
	// The site dir is derived from the validated site id, never trusted raw.
	dir := filepath.Join(a.sitesDir, s.SiteID)
	if s.SiteDir != "" && s.SiteDir != dir {
		return "", fmt.Errorf("site_dir mismatch: refusing unexpected path")
	}
	return dir, nil
}

func (a *Agent) guardExisting(ctx context.Context, name string) error {
	c, err := a.docker.FindByName(ctx, name)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if c.Labels[docker.ManagedLabel] != "true" {
		return fmt.Errorf("production guard: container %q exists and is not managed by Own hPanel; refusing", name)
	}
	if c.Labels["own-hpanel.site-id"] != "" {
		// Caller checks the site id match where relevant.
	}
	return nil
}

// refuseDBContainer enforces the DB rule of the dedicated-container
// design: the panel-managed MariaDB container (wordpress-mariadb) is a
// lifecycle target of Docker Compose ONLY - the agent never stops,
// restarts, removes or recreates it directly. Any operation that would
// lifecycle-manage a container whose name matches the configured DB
// container is refused outright. Every OTHER container is protected by
// the managed-label guard as usual; the configured DB container is the
// agent's only ExecDB/network-connect target.
func (a *Agent) refuseDBContainer(name string) error {
	if name != "" && name == a.db.Container {
		return fmt.Errorf("production guard: %q is the panel-managed MariaDB container; the agent never stops/restarts/removes/recreates it (its lifecycle belongs to Docker Compose)", name)
	}
	return nil
}

// usesCommonUser reports whether a site's DB user is the common admin
// account (privilege-fallback provisioning). Such a user must never
// be dropped or password-rotated by per-site operations.
func (a *Agent) usesCommonUser(dbUser string) bool {
	return dbUser != "" && dbUser == a.db.AdminUser
}

// mariadbArgv builds the SQL-client argv used inside the dedicated DB
// container. It always authenticates as the configured common admin user -
// never as root.
func mariadbArgv(adminUser, adminPassword, joinedSQL string) []string {
	return []string{"mariadb", "-u" + adminUser, "-p" + adminPassword, "-e", joinedSQL}
}

func (a *Agent) mariadbExec(ctx context.Context, sqlStatements []string) (string, error) {
	if a.db.AdminPassword == "" {
		return "", fmt.Errorf("agent has no MariaDB admin credential configured (set DB_ADMIN_PASSWORD_FILE)")
	}
	joined := strings.Join(sqlStatements, " ")
	// ExecDB is the exec-only path into the panel-managed DB container; it
	// cannot start/stop/remove anything.
	out, code, err := a.docker.ExecDB(ctx, a.db.Container, mariadbArgv(a.db.AdminUser, a.db.AdminPassword, joined))
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("mariadb command exited %d: %s", code, trimOut(out))
	}
	return out, nil
}

func trimOut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// ---- discovery (read-only) ----

func (a *Agent) discovery(ctx context.Context) (*agentproto.Response, error) {
	resp := &agentproto.Response{OK: true}
	containers, err := a.docker.ListContainers(ctx, true, "")
	if err != nil {
		return resp, err
	}
	var lines []string
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		lines = append(lines, fmt.Sprintf("%s %s %s managed=%v", name, c.Image, c.State, c.Labels[docker.ManagedLabel] == "true"))
	}
	nets, err := a.docker.ListNetworks(ctx)
	if err == nil {
		for _, n := range nets {
			lines = append(lines, "network: "+n.Name)
		}
	}
	resp.Output = strings.Join(lines, "\n")
	return resp, nil
}

// ---- one-shot helper containers (WP-CLI / MariaDB client) ----

// runOneShot creates, runs and removes a labelled helper container attached
// to the site network with the site data bind-mounted. Returns combined logs.
func (a *Agent) runOneShot(ctx context.Context, s agentproto.SiteSpec, image string, cmd []string, env []string, extraBinds []string, network string) (string, error) {
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return "", err
	}
	binds := append([]string{siteDir + "/data:/var/www/html"}, extraBinds...)
	labels := map[string]string{docker.ManagedLabel: "true", "own-hpanel.site-id": s.SiteID, "own-hpanel.ephemeral": "true"}
	cfg := docker.CreateConfig{
		Image:  image,
		Cmd:    cmd,
		User:   "0",
		Env:    env,
		Labels: labels,
		HostConfig: map[string]any{
			"Binds": binds, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
		},
		NetworkingConfig: map[string]any{
			"EndpointsConfig": map[string]any{network: map[string]any{}},
		},
	}
	name := fmt.Sprintf("own-hpanel-%s-helper-%d", s.SiteID, time.Now().Unix())
	id, err := a.docker.CreateContainer(ctx, name, cfg)
	if err != nil {
		return "", fmt.Errorf("creating helper container: %w", err)
	}
	defer func() { _ = a.docker.RemoveAny(ctx, id, true) }()
	if err := a.docker.StartUnchecked(ctx, id); err != nil {
		return "", err
	}
	code, err := a.docker.Wait(ctx, id)
	logs, _ := a.docker.Logs(ctx, id, 200)
	if err != nil {
		return logs, err
	}
	if code != 0 {
		return logs, fmt.Errorf("helper container exited %d: %s", code, trimOut(logs))
	}
	return logs, nil
}

func (a *Agent) createSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	add := func(p int, msg string) {
		resp.Events = append(resp.Events, agentproto.Event{Progress: p, Message: msg, Level: "info"})
	}
	if err := validateSite(s, true); err != nil {
		return resp, err
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return resp, err
	}
	if err := a.guardExisting(ctx, s.ContainerName); err != nil {
		return resp, err
	}
	// Router/domain conflict check against existing Traefik-labelled containers.
	all, err := a.docker.ListContainers(ctx, true, "")
	if err != nil {
		return resp, err
	}
	for _, c := range all {
		for _, v := range c.Labels {
			if strings.Contains(v, "Host(`"+s.Domain+"`)") && c.Labels["own-hpanel.site-id"] != s.SiteID {
				return resp, fmt.Errorf("domain %s is already routed by container %s", s.Domain, c.ID[:12])
			}
		}
	}
	add(10, "inputs validated; no domain conflict")
	if err := a.docker.Ping(ctx); err != nil {
		return resp, err
	}
	// Existing Traefik network must be present; agent never creates/modifies Traefik.
	ok, err := a.docker.NetworkExists(ctx, traefikNetwork)
	if err != nil {
		return resp, err
	}
	if !ok {
		return resp, fmt.Errorf("required existing network %q not found; refusing to proceed", traefikNetwork)
	}
	// The MariaDB container is the stack's OWN dedicated container
	// (DB_CONTAINER, default wordpress-mariadb), panel-managed and created
	// by Docker Compose. The agent only verifies it is running, execs the
	// SQL client in it and connects it to per-site networks; it never
	// starts/stops/restarts it directly - if it is down, the operator
	// fixes the stack with `docker compose up -d`.
	maria, err := a.docker.FindByName(ctx, a.db.Container)
	if err != nil {
		return resp, err
	}
	if maria == nil {
		return resp, fmt.Errorf("MariaDB container %q not found; it is created by the hPanel compose stack - deploy/start the stack first (docker compose up -d)", a.db.Container)
	}
	if maria.State != "running" {
		return resp, fmt.Errorf("MariaDB container %q is not running (state=%s); refusing to proceed - start the hPanel stack (docker compose up -d)", a.db.Container, maria.State)
	}
	add(20, "existing Traefik network verified; dedicated MariaDB ("+a.db.Container+") is running")

	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	network := websites.NetworkName(s.SiteID)
	if err := a.docker.CreateNetwork(ctx, network, map[string]string{docker.ManagedLabel: "true", "own-hpanel.site-id": s.SiteID}); err != nil {
		return resp, fmt.Errorf("creating site network: %w", err)
	}
	// Connect the dedicated MariaDB to the new per-site network. Together
	// with ExecDB this is the only permitted agent interaction with that
	// container: the agent never stops, restarts or recreates it.
	if err := a.docker.ConnectNetwork(ctx, network, a.db.Container); err != nil {
		return resp, fmt.Errorf("connecting MariaDB to site network: %w", err)
	}
	add(30, "per-site network created; MariaDB connected to it")

	// Database provisioning with the common admin account: one database +
	// one dedicated least-privilege user per site. If the common account
	// lacks CREATE USER / GRANT privileges, fall back to creating only the
	// database and connecting the site AS the common user - loudly: a
	// warning event is recorded on the job (and an audit entry by the
	// panel), never silently.
	effDBUser, effDBPassword := s.DBUser, s.DBPassword
	sql, err := database.ProvisionSQL(s.DBName, s.DBUser, s.DBPassword)
	if err != nil {
		return resp, err
	}
	if _, err := a.mariadbExec(ctx, sql); err != nil {
		if !dbconfig.IsPrivilegeError(err) {
			return resp, fmt.Errorf("database provisioning: %w", err)
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 40, Level: "warning",
			Message: "common DB user " + a.db.AdminUser + " could not create a dedicated user/grant for this site (privilege error: " + trimOut(err.Error()) + "); falling back to the common DB user for this site - database isolation for this site is reduced until the admin account is granted CREATE USER/GRANT or a dedicated user is created manually"})
		dbOnly, derr := database.CreateDatabaseSQL(s.DBName)
		if derr != nil {
			return resp, derr
		}
		if _, err2 := a.mariadbExec(ctx, []string{dbOnly}); err2 != nil {
			return resp, fmt.Errorf("database provisioning failed and the fallback (create database only) also failed: %w", err2)
		}
		effDBUser, effDBPassword = a.db.AdminUser, a.db.AdminPassword
		resp.UsedCommonDBUser = true
		add(45, "database created; site connects with the common DB user (fallback)")
	} else {
		add(45, "database and least-privilege user created")
	}

	profile, err := configgen.ProfileFor(s.Profile, s.CPULimit, s.MemLimitMB)
	if err != nil {
		return resp, err
	}
	spec := configgen.SiteSpec{
		SiteID: s.SiteID, Domain: s.Domain, AdminDomain: s.AdminDomain, Title: s.Title,
		DBName: s.DBName, DBUser: effDBUser, DBPassword: effDBPassword, DBHost: a.db.InternalHost(),
		AdminUser: s.AdminUser, AdminEmail: s.AdminEmail, PHPVersion: s.PHPVersion,
		Profile: profile, SiteDir: siteDir, ContainerName: s.ContainerName, Network: network,
	}
	composeYAML, err := configgen.GenerateCompose(spec)
	if err != nil {
		return resp, err
	}
	envText := configgen.GenerateEnv(spec)
	if err := os.MkdirAll(filepath.Join(siteDir, "data"), 0o750); err != nil {
		return resp, err
	}
	if err := os.WriteFile(filepath.Join(siteDir, "compose.yaml"), []byte(composeYAML), 0o640); err != nil {
		return resp, err
	}
	if err := os.WriteFile(filepath.Join(siteDir, ".env"), []byte(envText), 0o600); err != nil {
		return resp, err
	}
	add(55, "site storage, .env and compose configuration written")

	// Idempotency: if our own container already exists, reuse it.
	existing, err := a.docker.FindByName(ctx, s.ContainerName)
	if err != nil {
		return resp, err
	}
	var containerID string
	if existing != nil {
		containerID = existing.ID
		if existing.State != "running" {
			if err := a.docker.Start(ctx, containerID); err != nil {
				return resp, err
			}
		}
		add(65, "existing site container reused (idempotent retry)")
	} else {
		img, _ := configgen.PHPImage(s.PHPVersion)
		labels := configgen.TraefikLabels(spec)
		var envList []string
		for k, v := range configgen.EnvMap(spec) {
			envList = append(envList, k+"="+v)
		}
		cfg := docker.CreateConfig{
			Image:        img,
			Env:          envList,
			Labels:       labels,
			ExposedPorts: map[string]struct{}{"80/tcp": {}},
			Healthcheck: map[string]any{
				"Test":     []string{"CMD", "php", "-r", "exit(@fsockopen('127.0.0.1', 80) ? 0 : 1);"},
				"Interval": int64(30 * time.Second), "Timeout": int64(5 * time.Second), "Retries": 3, "StartPeriod": int64(40 * time.Second),
			},
			HostConfig: map[string]any{
				"Binds":             []string{siteDir + "/data:/var/www/html"},
				"CapDrop":           []string{"ALL"},
				"CapAdd":            []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "NET_BIND_SERVICE", "SETGID", "SETUID"},
				"SecurityOpt":       []string{"no-new-privileges:true"},
				"Memory":            int64(profile.MemMB) << 20,
				"MemoryReservation": int64(profile.ReserveMB) << 20,
				"NanoCPUs":          int64(profile.CPUs * 1e9),
				"RestartPolicy":     map[string]any{"Name": "unless-stopped"},
				"LogConfig":         map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}},
			},
			NetworkingConfig: map[string]any{
				"EndpointsConfig": map[string]any{
					network:        map[string]any{},
					traefikNetwork: map[string]any{},
				},
			},
		}
		id, err := a.docker.CreateContainer(ctx, s.ContainerName, cfg)
		if err != nil {
			return resp, fmt.Errorf("creating WordPress container: %w (is image %s pulled?)", err, img)
		}
		containerID = id
		if err := a.docker.Start(ctx, id); err != nil {
			return resp, fmt.Errorf("starting WordPress container: %w", err)
		}
		add(65, "WordPress container created and started")
	}
	resp.ContainerID = containerID

	// Wait briefly for Apache to accept connections (best effort).
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		info, err := a.docker.Inspect(ctx, containerID)
		if err == nil {
			if st, ok := info["State"].(map[string]any); ok {
				if running, _ := st["Running"].(bool); running {
					if h, ok := st["Health"].(map[string]any); ok {
						if hs, _ := h["Status"].(string); hs == "healthy" || hs == "" {
							break
						}
					} else {
						break
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	add(75, "WordPress container is running")

	// WP-CLI install via one-shot helper (idempotent: check core is-installed first).
	env := []string{
		"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName,
		"WORDPRESS_DB_USER=" + effDBUser, "WORDPRESS_DB_PASSWORD=" + effDBPassword,
	}
	_, installedErr := a.runOneShot(ctx, s, "wordpress:cli-2.11.0-php8.3",
		[]string{"wp", "core", "is-installed", "--allow-root"}, env, nil, network)
	if installedErr != nil {
		installArgs := wordpress.InstallArgs("https://"+s.Domain, s.Title, s.AdminUser, s.AdminPassword, s.AdminEmail)
		out, err := a.runOneShot(ctx, s, "wordpress:cli-2.11.0-php8.3", installArgs, env, nil, network)
		if err != nil {
			return resp, fmt.Errorf("WP-CLI install failed: %w", err)
		}
		_ = out
		add(90, "WordPress installed with the supplied administrator account")
	} else {
		add(90, "WordPress already installed (idempotent retry); install skipped")
	}

	// DNS / SSL state: DNS not resolving yet is NOT a provisioning failure.
	dns := domains.Lookup(ctx, s.Domain, serverIP)
	if !dns.Resolved || (!dns.PointsToOrigin && !dns.Cloudflare) {
		resp.DNSPending = true
		add(100, "site provisioned; DNS pending - add the A records shown in the panel")
	} else {
		add(100, "site provisioned and DNS resolves")
	}
	resp.OK = true
	return resp, nil
}

func (a *Agent) lifecycle(ctx context.Context, req agentproto.Request, action string) (*agentproto.Response, error) {
	s := req.Site
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return nil, err
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return nil, err
	}
	c, err := a.docker.FindByName(ctx, s.ContainerName)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("container %q not found", s.ContainerName)
	}
	if c.Labels["own-hpanel.site-id"] != s.SiteID {
		return nil, fmt.Errorf("production guard: container does not belong to site %s", s.SiteID)
	}
	switch action {
	case "start":
		err = a.docker.Start(ctx, c.ID)
	case "stop":
		err = a.docker.Stop(ctx, c.ID, 15)
	case "restart":
		err = a.docker.Restart(ctx, c.ID, 15)
	}
	if err != nil {
		return nil, err
	}
	return &agentproto.Response{OK: true, ContainerID: c.ID, Events: []agentproto.Event{{Progress: 100, Message: "container " + action + " completed", Level: "info"}}}, nil
}

func (a *Agent) deleteSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return resp, err
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return resp, err
	}
	c, err := a.docker.FindByName(ctx, s.ContainerName)
	if err != nil {
		return resp, err
	}
	if c != nil {
		if c.Labels["own-hpanel.site-id"] != s.SiteID {
			return resp, fmt.Errorf("production guard: container does not belong to site %s", s.SiteID)
		}
		_ = a.docker.Stop(ctx, c.ID, 15)
		if err := a.docker.Remove(ctx, c.ID, false); err != nil {
			return resp, err
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 40, Message: "site container removed", Level: "info"})
	}
	// Drop the site's own database (validated identifiers). If the site was
	// provisioned on the common user (privilege fallback), drop ONLY
	// the database - the common user serves every other site and must never
	// be dropped.
	var dropSQL []string
	var derr error
	commonSite := a.usesCommonUser(s.DBUser)
	if commonSite {
		dropSQL, derr = database.DropDatabaseOnlySQL(s.DBName)
	} else {
		dropSQL, derr = database.DropDatabaseSQL(s.DBName, s.DBUser)
	}
	if derr != nil {
		return resp, derr
	}
	if _, err := a.mariadbExec(ctx, dropSQL); err != nil {
		return resp, fmt.Errorf("dropping site database: %w", err)
	}
	if commonSite {
		resp.Events = append(resp.Events, agentproto.Event{Progress: 70, Message: "site database dropped; common DB user preserved", Level: "info"})
	} else {
		resp.Events = append(resp.Events, agentproto.Event{Progress: 70, Message: "site database and user dropped", Level: "info"})
	}
	// Remove only this site's directory (derived from the validated site id).
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	if !strings.HasPrefix(siteDir, a.sitesDir+string(filepath.Separator)) {
		return resp, fmt.Errorf("refusing to delete path outside sites directory")
	}
	if err := os.RemoveAll(siteDir); err != nil {
		return resp, err
	}
	resp.Events = append(resp.Events, agentproto.Event{Progress: 100, Message: "site files removed", Level: "info"})
	resp.OK = true
	return resp, nil
}

func (a *Agent) backupSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	if err := validateSiteBasic(s); err != nil {
		return resp, err
	}
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	stage := filepath.Join(a.backupsDir, "stage-"+s.SiteID+"-"+strconv.FormatInt(time.Now().Unix(), 10))
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return resp, err
	}
	defer os.RemoveAll(stage)
	// Dump via a one-shot MariaDB client container on the site network so no
	// port is ever published. Shell is used only inside this disposable
	// container with validated identifiers and an escaped generated password.
	dumpCmd := fmt.Sprintf("mariadb-dump -h %s -u %s -p'%s' --single-transaction --routines %s > /stage/db.sql",
		a.db.InternalHost(), s.DBUser, database.EscapeLiteral(s.DBPassword), s.DBName)
	helper := agentproto.SiteSpec{SiteID: s.SiteID}
	_ = helper
	if _, err := a.runOneShot(ctx, s, "mariadb:11.4", []string{"sh", "-c", dumpCmd}, nil,
		[]string{stage + ":/stage"}, websites.NetworkName(s.SiteID)); err != nil {
		return resp, fmt.Errorf("database dump failed: %w", err)
	}
	resp.Events = append(resp.Events, agentproto.Event{Progress: 50, Message: "database dump completed", Level: "info"})
	filename := backups.Filename(s.SiteID, time.Now())
	dest := filepath.Join(a.backupsDir, filename)
	sha, size, err := backups.CreateTarGz(filepath.Join(siteDir, "data"), filepath.Join(stage, "db.sql"), dest)
	if err != nil {
		return resp, err
	}
	ok, _, err := backups.Verify(dest, sha)
	if err != nil || !ok {
		_ = os.Remove(dest)
		return resp, fmt.Errorf("backup verification failed; artifact discarded")
	}
	resp.OK = true
	resp.BackupPath = dest
	resp.BackupSHA256 = sha
	resp.BackupSize = size
	resp.Events = append(resp.Events, agentproto.Event{Progress: 100, Message: "backup created and sha256 verified", Level: "info"})
	return resp, nil
}

func validateSiteBasic(s agentproto.SiteSpec) error {
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return err
	}
	if err := security.ValidateDBName(s.DBName); err != nil {
		return err
	}
	return security.ValidateDBUser(s.DBUser)
}

func (a *Agent) restoreSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	if err := validateSiteBasic(s); err != nil {
		return resp, err
	}
	if req.BackupPath == "" || !strings.HasPrefix(req.BackupPath, a.backupsDir) {
		return resp, fmt.Errorf("backup path must be inside the panel backups directory")
	}
	if _, err := os.Stat(req.BackupPath); err != nil {
		return resp, fmt.Errorf("backup artifact not found")
	}
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	stage := filepath.Join(a.backupsDir, "restore-"+s.SiteID+"-"+strconv.FormatInt(time.Now().Unix(), 10))
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return resp, err
	}
	defer os.RemoveAll(stage)
	dbSQL, err := extractAll(req.BackupPath, stage)
	if err != nil {
		return resp, fmt.Errorf("extracting backup: %w", err)
	}
	// Restore files into the site data dir.
	filesDir := filepath.Join(stage, "files")
	if st, err := os.Stat(filesDir); err == nil && st.IsDir() {
		if err := copyTree(filesDir, filepath.Join(siteDir, "data")); err != nil {
			return resp, err
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 50, Message: "files restored", Level: "info"})
	}
	if dbSQL != "" {
		importCmd := fmt.Sprintf("mariadb -h %s -u %s -p'%s' %s < /stage/db.sql",
			a.db.InternalHost(), s.DBUser, database.EscapeLiteral(s.DBPassword), s.DBName)
		if _, err := a.runOneShot(ctx, s, "mariadb:11.4", []string{"sh", "-c", importCmd}, nil,
			[]string{stage + ":/stage:ro"}, websites.NetworkName(s.SiteID)); err != nil {
			return resp, fmt.Errorf("database restore failed: %w", err)
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 90, Message: "database restored", Level: "info"})
	}
	resp.OK = true
	resp.Events = append(resp.Events, agentproto.Event{Progress: 100, Message: "restore completed", Level: "info"})
	return resp, nil
}

// extractAll unpacks an archive into dest, returning the path of db.sql if present.
func extractAll(archivePath, dest string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	cleanDest, _ := filepath.Abs(dest)
	var dbPath string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return dbPath, nil
		}
		if err != nil {
			return "", err
		}
		target := filepath.Join(cleanDest, hdr.Name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe archive entry %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
			if hdr.Name == "db.sql" {
				dbPath = target
			}
		default:
			// Skip links and devices.
		}
	}
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode()&0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func (a *Agent) wpcli(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return nil, err
	}
	// Password reset is handled specially: the new password arrives in the
	// request and is never logged or echoed back.
	if req.Action == "user_reset_password" {
		if req.Arg == "" || req.NewDBPassword == "" {
			return nil, fmt.Errorf("reset-password needs a username and a new password")
		}
		if err := security.ValidateAdminUsername(req.Arg); err != nil {
			return nil, err
		}
		args := wordpress.ResetPasswordArgs(req.Arg, req.NewDBPassword)
		env := []string{"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName, "WORDPRESS_DB_USER=" + s.DBUser, "WORDPRESS_DB_PASSWORD=" + s.DBPassword}
		out, err := a.runOneShot(ctx, s, "wordpress:cli-2.11.0-php8.3", args, env, nil, websites.NetworkName(s.SiteID))
		if err != nil {
			return nil, err
		}
		return &agentproto.Response{OK: true, Output: trimOut(out)}, nil
	}
	cmd, err := wordpress.Build(req.Action, req.Arg)
	if err != nil {
		return nil, err
	}
	env := []string{"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName, "WORDPRESS_DB_USER=" + s.DBUser, "WORDPRESS_DB_PASSWORD=" + s.DBPassword}
	out, err := a.runOneShot(ctx, s, "wordpress:cli-2.11.0-php8.3", cmd.Args, env, nil, websites.NetworkName(s.SiteID))
	if err != nil {
		return nil, err
	}
	return &agentproto.Response{OK: true, Output: out}, nil
}

func (a *Agent) applyConfig(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return resp, err
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return resp, err
	}
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	if req.Site.ComposeYAML == "" {
		return resp, fmt.Errorf("no compose configuration supplied")
	}
	// The agent independently re-validates the policy - panel checks are not trusted.
	if err := security.ValidateComposePolicy(req.Site.ComposeYAML, siteDir); err != nil {
		return resp, fmt.Errorf("compose policy rejected by agent: %w", err)
	}
	if req.Site.EnvText != "" {
		if err := os.WriteFile(filepath.Join(siteDir, ".env"), []byte(req.Site.EnvText), 0o600); err != nil {
			return resp, err
		}
	}
	if err := os.WriteFile(filepath.Join(siteDir, "compose.yaml"), []byte(req.Site.ComposeYAML), 0o640); err != nil {
		return resp, err
	}
	// Recreate ONLY this site's container from the validated spec.
	if _, err := a.createSite(ctx, agentproto.Request{Op: agentproto.OpCreateSite, Site: s}); err != nil {
		// createSite is idempotent and will reuse a running container; a config
		// change that alters the container spec requires explicit recreation:
		c, ferr := a.docker.FindByName(ctx, s.ContainerName)
		if ferr != nil || c == nil {
			return resp, err
		}
		if c.Labels["own-hpanel.site-id"] != s.SiteID {
			return resp, fmt.Errorf("production guard: container does not belong to this site")
		}
		_ = a.docker.Stop(ctx, c.ID, 15)
		if rerr := a.docker.Remove(ctx, c.ID, false); rerr != nil {
			return resp, rerr
		}
		if _, err2 := a.createSite(ctx, agentproto.Request{Op: agentproto.OpCreateSite, Site: s}); err2 != nil {
			return resp, err2
		}
	}
	resp.OK = true
	resp.Events = append(resp.Events, agentproto.Event{Progress: 100, Message: "configuration applied", Level: "info"})
	return resp, nil
}

func (a *Agent) rotateDBPassword(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	if err := validateSiteBasic(s); err != nil {
		return nil, err
	}
	if a.usesCommonUser(s.DBUser) {
		return nil, fmt.Errorf("this site uses the common DB user (%s); per-site password rotation is refused because it would change the password for every site and for the provisioning account itself", s.DBUser)
	}
	if req.NewDBPassword == "" || len(req.NewDBPassword) < 20 {
		return nil, fmt.Errorf("new database password is too short")
	}
	sql, err := database.RotatePasswordSQL(s.DBUser, req.NewDBPassword)
	if err != nil {
		return nil, err
	}
	if _, err := a.mariadbExec(ctx, sql); err != nil {
		return nil, err
	}
	return &agentproto.Response{OK: true, Events: []agentproto.Event{{Progress: 100, Message: "database password rotated in MariaDB; restart the site to apply the new .env", Level: "info"}}}, nil
}

func (a *Agent) stats(ctx context.Context) (*agentproto.Response, error) {
	containers, err := a.docker.ListContainers(ctx, false, docker.ManagedLabel+"=true")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, c := range containers {
		st, err := a.docker.StatsOneShot(ctx, c.ID)
		if err != nil {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		lines = append(lines, fmt.Sprintf("%s site=%s cpu=%.1f%% mem=%d/%d",
			name, c.Labels["own-hpanel.site-id"], st.CPUPercent, st.MemUsage, st.MemLimit))
	}
	return &agentproto.Response{OK: true, Output: strings.Join(lines, "\n")}, nil
}

func (a *Agent) siteLogs(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return nil, err
	}
	c, err := a.docker.FindByName(ctx, s.ContainerName)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Labels["own-hpanel.site-id"] != s.SiteID {
		return nil, fmt.Errorf("production guard: container not found for this site")
	}
	out, err := a.docker.Logs(ctx, c.ID, 300)
	if err != nil {
		return nil, err
	}
	return &agentproto.Response{OK: true, Output: out}, nil
}

func (a *Agent) dbSize(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	q, err := database.SizeQuery(s.DBName)
	if err != nil {
		return nil, err
	}
	out, err := a.mariadbExec(ctx, []string{"SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema='" + s.DBName + "';"})
	_ = q
	if err != nil {
		return nil, err
	}
	// mariadb -e output has a header line; take the last numeric token.
	fields := strings.Fields(out)
	var size int64
	for i := len(fields) - 1; i >= 0; i-- {
		if n, err := strconv.ParseInt(fields[i], 10, 64); err == nil {
			size = n
			break
		}
	}
	return &agentproto.Response{OK: true, DBSizeBytes: size}, nil
}
