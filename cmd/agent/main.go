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
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// maxExtractBytes caps the total decompressed size of a backup
	// archive during restore (decompression-bomb guard). 15 GiB is
	// far above any single lightweight WordPress site's real size on
	// this host while staying well under the disk's free headroom.
	maxExtractBytes = 15 << 30
	serverIP        = "130.210.42.20"
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
	// Fail closed on a weak token: an empty or short token file
	// (a botched secret mount) must never leave the socket guarded
	// by a guessable bearer token — with an empty stored token, an
	// empty presented token would otherwise compare equal.
	if len(strings.TrimSpace(string(tokenBytes))) < 32 {
		log.Error("agent token file is empty or too short (need at least 32 characters); refusing to start")
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
	// No Read/Write timeouts: legitimate operations run for
	// minutes and stream their result at the end. IdleTimeout
	// still reaps dead connections.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	log.Info("own-hpanel agent listening", "socket", socketPath)
	if err := srv.Serve(ln); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func (a *Agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || len(got) != len(a.token) || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
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
	case agentproto.OpRepairSite:
		resp, err = a.repairSite(ctx, req)
	case agentproto.OpImportSite:
		resp, err = a.importSite(ctx, req)
	}
	if err != nil {
		if resp == nil {
			resp = &agentproto.Response{}
		}
		resp.OK = false
		if resp.Error == "" {
			resp.Error = err.Error()
		}
		if resp.ErrorCategory == "" {
			var oe *opError
			if errors.As(err, &oe) {
				resp.ErrorCategory = oe.category
			}
			var amb *docker.AmbiguousError
			if errors.As(err, &amb) {
				resp.ErrorCategory = agentproto.ErrCatAmbiguous
			}
		}
		a.log.Warn("operation failed", "op", req.Op, "site", req.Site.SiteID, "err", resp.Error)
		writeResp(w, 200, *resp) // operational failures are data, not transport errors
		return
	}
	if resp == nil {
		resp = &agentproto.Response{OK: true}
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
	if s.TablePrefix != "" {
		if err := security.ValidateTablePrefix(s.TablePrefix); err != nil {
			return err
		}
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

// guardExisting refuses to let a create proceed when a container
// already owns the target name but is not THIS site's container:
// unmanaged squatters and other sites' containers alike. (A container
// that IS this site's - matching site-id label - passes; createSite's
// idempotency path reuses it.)
func (a *Agent) guardExisting(ctx context.Context, name, siteID string) error {
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
	if owner := c.Labels["own-hpanel.site-id"]; owner != "" && owner != siteID {
		return fmt.Errorf("production guard: container %q belongs to site %s, not %s; refusing", name, owner, siteID)
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
// mariadbArgv builds the SQL client argv WITHOUT the password: the
// password travels as the MYSQL_PWD environment variable of the exec
// instance (see mariadbExec), never in argv where the host process
// list would expose the all-sites provisioning credential.
func mariadbArgv(adminUser, joinedSQL string) []string {
	return []string{"mariadb", "-u" + adminUser, "-e", joinedSQL}
}

func (a *Agent) mariadbExec(ctx context.Context, sqlStatements []string) (string, error) {
	if a.db.AdminPassword == "" {
		return "", fmt.Errorf("agent has no MariaDB admin credential configured (set DB_ADMIN_PASSWORD_FILE)")
	}
	joined := strings.Join(sqlStatements, " ")
	// ExecDB is the exec-only path into the panel-managed DB container; it
	// cannot start/stop/remove anything.
	out, code, err := a.docker.ExecDBEnv(ctx, a.db.Container, mariadbArgv(a.db.AdminUser, joined), []string{"MYSQL_PWD=" + a.db.AdminPassword})
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

// helperSuffix returns 8 random hex characters for helper
// container names; on a (practically impossible) rand failure it
// falls back to a nanosecond timestamp so the name still differs.
func helperSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
	// The random suffix prevents a same-second name collision with
	// a previous helper whose removal failed; cleanup runs on a
	// FRESH context because the operation context is usually the
	// thing that just expired when cleanup matters most.
	name := fmt.Sprintf("own-hpanel-%s-helper-%d-%s", s.SiteID, time.Now().Unix(), helperSuffix())
	id, err := a.docker.CreateContainer(ctx, name, cfg)
	if err != nil {
		return "", fmt.Errorf("creating helper container: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.docker.RemoveAny(cleanupCtx, id, true)
	}()
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
	if err := validateSite(s, false); err != nil {
		return resp, err
	}
	// The DB password is always required (it is stored encrypted and
	// the panel supplies it). The WP admin password is required for a
	// fresh create, but not in RepairMode: it is never stored, and a
	// repair of an already-installed site never needs it.
	if s.DBPassword == "" {
		return resp, fmt.Errorf("missing required secret material for provisioning (database password)")
	}
	if s.AdminPassword == "" && !req.RepairMode {
		return resp, fmt.Errorf("missing required secret material for provisioning (WordPress admin password)")
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return resp, err
	}
	if err := a.guardExisting(ctx, s.ContainerName, s.SiteID); err != nil {
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
		TablePrefix: s.TablePrefix,
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

	// Idempotency: if this site's container already exists (resolved
	// by ownership labels, not by name - a recreated container keeps
	// its labels but may have a new name/ID), reuse it.
	existing, err := a.docker.FindBySiteID(ctx, s.SiteID)
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
		add(60, "ensuring the WordPress image is available locally (first create pulls it - this can take a few minutes)")
		id, err := a.docker.CreateContainer(ctx, s.ContainerName, cfg)
		if err != nil {
			return resp, fmt.Errorf("creating WordPress container: %w", err)
		}
		containerID = id
		if err := a.docker.Start(ctx, id); err != nil {
			return resp, fmt.Errorf("starting WordPress container: %w", err)
		}
		add(65, "WordPress container created and started")
	}
	resp.ContainerID = containerID

	// Wait for Apache to accept connections. This wait used to be
	// cosmetic — on timeout provisioning proceeded and reported a
	// running container anyway, so a dead container produced a
	// "successful" create whose WP-CLI install ran against files
	// and MariaDB alone. Now the deadline is enforced.
	deadline := time.Now().Add(90 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		info, err := a.docker.Inspect(ctx, containerID)
		if err == nil {
			if st, ok := info["State"].(map[string]any); ok {
				if running, _ := st["Running"].(bool); running {
					if h, ok := st["Health"].(map[string]any); ok {
						if hs, _ := h["Status"].(string); hs == "healthy" || hs == "" {
							ready = true
							break
						}
					} else {
						ready = true
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
	if !ready {
		return resp, fmt.Errorf("WordPress container did not become ready within 90 seconds (it may be crash-looping); check the site logs")
	}
	add(75, "WordPress container is running")

	// WP-CLI install via one-shot helper (idempotent: check core is-installed first).
	env := []string{
		"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName,
		"WORDPRESS_DB_USER=" + effDBUser, "WORDPRESS_DB_PASSWORD=" + effDBPassword,
	}
	_, installedErr := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3",
		[]string{"wp", "core", "is-installed", "--allow-root"}, env, nil, network)
	if installedErr != nil && s.AdminPassword == "" {
		// Repair without an admin password: the container and site are
		// fully provisioned; only the WordPress first-run install is
		// left to the browser setup wizard, which is WordPress's own
		// supported flow. Not a failure.
		resp.Events = append(resp.Events, agentproto.Event{Progress: 90, Message: "WordPress is not installed yet and no admin password was supplied: open the website and complete the WordPress setup wizard in the browser to create the administrator account", Level: "warn"})
	} else if installedErr != nil {
		installArgs := wordpress.InstallArgs("https://"+s.Domain, s.Title, s.AdminUser, s.AdminPassword, s.AdminEmail)
		out, err := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", installArgs, env, nil, network)
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

// opError is an operational failure with a machine-readable category
// (agentproto.ErrCat*) that the panel uses for retry policy, website
// status updates and recovery hints.
type opError struct {
	category string
	msg      string
}

func (e *opError) Error() string { return e.msg }

func catErr(category, format string, args ...any) *opError {
	return &opError{category: category, msg: fmt.Sprintf(format, args...)}
}

// resolveSiteContainer finds the site's WordPress container by its
// ownership labels - never by name. Identity rules:
//   - exactly one container carries the site's managed labels -> it,
//     whatever its current name or ID (recreation-safe);
//   - several -> ambiguous error (operator reconciles; never guess);
//   - none, but a container with the expected NAME exists -> guard
//     error: it is not panel-owned for this site and is never adopted
//     on name similarity alone;
//   - none at all -> not_found error with a recovery hint; the site's
//     database and files are untouched by definition.
func (a *Agent) resolveSiteContainer(ctx context.Context, s agentproto.SiteSpec) (*docker.Container, error) {
	c, err := a.docker.FindBySiteID(ctx, s.SiteID)
	if err != nil {
		return nil, err // includes *docker.AmbiguousError
	}
	if c != nil {
		return c, nil
	}
	if s.ContainerName != "" {
		byName, nerr := a.docker.FindByName(ctx, s.ContainerName)
		if nerr == nil && byName != nil {
			return nil, catErr(agentproto.ErrCatGuard,
				"a container named %q exists but does not carry own-hpanel ownership labels for site %s; refusing to adopt or modify it - reconcile it manually", s.ContainerName, s.SiteID)
		}
	}
	return nil, catErr(agentproto.ErrCatNotFound,
		"container for site %s is missing: no container carries the own-hpanel.site-id=%s ownership label (resolution is by label, not by name). The site's database and files were not touched. Use Repair Website to recreate the container from the existing database and files", s.SiteID, s.SiteID)
}

func (a *Agent) lifecycle(ctx context.Context, req agentproto.Request, action string) (*agentproto.Response, error) {
	s := req.Site
	if err := security.ValidateSiteID(s.SiteID); err != nil {
		return nil, err
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return nil, err
	}
	c, err := a.resolveSiteContainer(ctx, s)
	if err != nil {
		return nil, err
	}
	running := c.State == "running"
	done := ""
	switch action {
	case "start":
		if running {
			done = "container already running; nothing to do"
		} else {
			err = a.docker.Start(ctx, c.ID)
			done = "container started"
		}
	case "stop":
		if !running {
			done = "container already stopped; nothing to do"
		} else {
			err = a.docker.Stop(ctx, c.ID, 15)
			done = "container stopped"
		}
	case "restart":
		if running {
			err = a.docker.Restart(ctx, c.ID, 15)
			done = "container restarted"
		} else {
			// Restart of a stopped container means "bring it back":
			// start it (documented lifecycle semantics). Restart never
			// recreates a missing container - that is Repair's job.
			err = a.docker.Start(ctx, c.ID)
			done = "container was stopped; started it"
		}
	}
	if err != nil {
		return nil, err
	}
	return &agentproto.Response{OK: true, ContainerID: c.ID, Events: []agentproto.Event{{Progress: 100, Message: done + " (" + c.ID[:12] + ")", Level: "info"}}}, nil
}

// repairSite is the explicit, controlled recovery for a site whose
// container is missing: it first verifies that the site's files and
// database still exist (repair never recreates lost data and never
// silently provisions an empty replacement), then re-runs the
// idempotent provisioning path, which reuses the existing database
// and files and only recreates the container.
func (a *Agent) repairSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	if err := validateSite(s, false); err != nil {
		return resp, err
	}
	if s.DBPassword == "" {
		return resp, fmt.Errorf("repair requires the site's database password (the panel supplies it from encrypted storage)")
	}
	if err := a.refuseDBContainer(s.ContainerName); err != nil {
		return resp, err
	}
	// Already healthy? Nothing to repair.
	if c, err := a.resolveSiteContainer(ctx, s); err == nil && c != nil {
		resp.OK = true
		resp.ContainerID = c.ID
		resp.Events = append(resp.Events, agentproto.Event{Progress: 100, Message: "container already exists (" + c.ID[:12] + ", state " + c.State + "); nothing to repair", Level: "info"})
		return resp, nil
	} else if err != nil {
		var oe *opError
		if !errors.As(err, &oe) || oe.category != agentproto.ErrCatNotFound {
			return resp, err // guard/ambiguous/daemon errors are not repairable by recreation
		}
	}
	// Verify the site's files survived.
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	if st, serr := os.Stat(filepath.Join(siteDir, "data")); serr != nil || !st.IsDir() {
		return resp, catErr(agentproto.ErrCatDataMissing,
			"repair aborted: the site's files (%s/data) are missing, so there is nothing safe to recreate the container against. Restore a backup, or delete the site and create it again", siteDir)
	}
	resp.Events = append(resp.Events, agentproto.Event{Progress: 20, Message: "repair: site files verified present", Level: "info"})
	// Verify the site's database survived.
	out, err := a.mariadbExec(ctx, []string{"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='" + s.DBName + "';"})
	if err != nil {
		return resp, fmt.Errorf("repair: checking the site database: %w", err)
	}
	if !strings.Contains(out, s.DBName) {
		return resp, catErr(agentproto.ErrCatDataMissing,
			"repair aborted: database %s no longer exists in the dedicated MariaDB, so the site's content is gone. Restore a backup, or delete the site and create it again - repair will not silently create an empty replacement database", s.DBName)
	}
	resp.Events = append(resp.Events, agentproto.Event{Progress: 40, Message: "repair: database " + s.DBName + " verified present; recreating the container against the existing data", Level: "info"})
	// Recreate through the normal idempotent provisioning path: it
	// reuses the existing database/user and files, skips the WP
	// install when WordPress is already installed, and only creates
	// the missing container.
	createReq := req
	createReq.RepairMode = true
	created, err := a.createSite(ctx, createReq)
	if created != nil {
		resp.Events = append(resp.Events, created.Events...)
		resp.ContainerID = created.ContainerID
		resp.DNSPending = created.DNSPending
		resp.UsedCommonDBUser = created.UsedCommonDBUser
	}
	if err != nil {
		return resp, err
	}
	resp.OK = true
	return resp, nil
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
	// Resolve by ownership labels. A missing container must NOT block
	// deletion (the database and files still need cleanup); guard and
	// ambiguity errors must.
	c, err := a.resolveSiteContainer(ctx, s)
	if err != nil {
		var oe *opError
		if errors.As(err, &oe) && oe.category == agentproto.ErrCatNotFound {
			resp.Events = append(resp.Events, agentproto.Event{Progress: 20, Message: "site container already missing; continuing with database and file cleanup", Level: "warn"})
			c = nil
		} else {
			return resp, err
		}
	}
	if c != nil {
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
	// Remove the per-site network. The shared MariaDB container is
	// attached to every site network, so detach it first; without
	// this step, networks and DB-container attachments accumulate
	// forever across create/delete cycles. Best effort: a network
	// that cannot be removed must not block the deletion itself.
	network := websites.NetworkName(s.SiteID)
	if exists, nerr := a.docker.NetworkExists(ctx, network); nerr == nil && exists {
		_ = a.docker.DisconnectNetwork(ctx, network, a.db.Container)
		if rerr := a.docker.RemoveNetwork(ctx, network); rerr != nil {
			resp.Events = append(resp.Events, agentproto.Event{Progress: 80, Message: "could not remove the site network " + network + " (" + trimOut(rerr.Error()) + "); it may still have endpoints attached", Level: "warn"})
		} else {
			resp.Events = append(resp.Events, agentproto.Event{Progress: 80, Message: "site network removed", Level: "info"})
		}
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
	// container with validated identifiers; the DB password travels via
	// the helper's MYSQL_PWD environment, never interpolated into the
	// shell string (an SQL-literal escaper is not a shell quoter).
	dumpCmd := fmt.Sprintf("mariadb-dump -h %s -u %s --single-transaction --routines %s > /stage/db.sql",
		a.db.InternalHost(), s.DBUser, s.DBName)
	helper := agentproto.SiteSpec{SiteID: s.SiteID}
	_ = helper
	if _, err := a.runOneShot(ctx, s, "mariadb:11.4", []string{"sh", "-c", dumpCmd}, []string{"MYSQL_PWD=" + s.DBPassword},
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
	cleanBackups := filepath.Clean(a.backupsDir)
	if req.BackupPath == "" || (filepath.Clean(req.BackupPath) != cleanBackups && !strings.HasPrefix(filepath.Clean(req.BackupPath), cleanBackups+string(os.PathSeparator))) {
		return resp, fmt.Errorf("backup path must be inside the panel backups directory")
	}
	if _, err := os.Stat(req.BackupPath); err != nil {
		return resp, fmt.Errorf("backup artifact not found")
	}
	// Integrity gate: when the panel supplies the recorded checksum,
	// the artifact must match it BEFORE anything is extracted or
	// imported - a swapped or corrupted artifact is never restored.
	if req.ExpectedSHA256 != "" {
		ok, got, err := backups.Verify(req.BackupPath, req.ExpectedSHA256)
		if err != nil {
			return resp, fmt.Errorf("verifying backup checksum: %w", err)
		}
		if !ok {
			return resp, fmt.Errorf("backup checksum mismatch: recorded %s, artifact hashes to %s - refusing to restore", req.ExpectedSHA256, got)
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 10, Message: "backup checksum verified", Level: "info"})
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
	// Restore files by SWAPPING the data directory, not by
	// overlaying the backup on top: an overlay leaves files that
	// were created after the backup in place, so the "restored"
	// site is not the backup. The container is stopped first
	// (its bind mount follows the old directory's inode), the old
	// tree is kept aside until the whole restore succeeds, and
	// any failure rolls the original tree back untouched.
	filesDir := filepath.Join(stage, "files")
	dataDir := filepath.Join(siteDir, "data")
	asideDir := filepath.Join(siteDir, "data.pre-restore")
	// Recover from a restore that died mid-swap: if the live data
	// dir is missing but the set-aside tree exists, it IS the data.
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		if _, err := os.Stat(asideDir); err == nil {
			_ = os.Rename(asideDir, dataDir)
		}
	}
	haveFiles := false
	if st, err := os.Stat(filesDir); err == nil && st.IsDir() {
		haveFiles = true
	}
	var container *docker.Container
	if c, cerr := a.resolveSiteContainer(ctx, s); cerr == nil {
		container = c
	}
	rollbackFiles := func() {}
	if haveFiles {
		if container != nil {
			_ = a.docker.Stop(ctx, container.ID, 15)
		}
		_ = os.RemoveAll(asideDir)
		movedAside := false
		if _, err := os.Stat(dataDir); err == nil {
			if err := os.Rename(dataDir, asideDir); err != nil {
				if container != nil {
					_ = a.docker.Start(ctx, container.ID)
				}
				return resp, fmt.Errorf("setting the current files aside for restore: %w", err)
			}
			movedAside = true
		}
		rollbackFiles = func() {
			_ = os.RemoveAll(dataDir)
			if movedAside {
				_ = os.Rename(asideDir, dataDir)
			}
			if container != nil {
				_ = a.docker.Start(ctx, container.ID)
			}
		}
		if err := copyTree(filesDir, dataDir); err != nil {
			rollbackFiles()
			return resp, err
		}
		if err := chownTree(dataDir, 33, 33); err != nil {
			resp.Events = append(resp.Events, agentproto.Event{Progress: 45, Message: "could not set www-data ownership on the restored files (" + trimOut(err.Error()) + "); check the File Manager if uploads misbehave", Level: "warn"})
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 50, Message: "files restored", Level: "info"})
	}
	if dbSQL != "" {
		importCmd := fmt.Sprintf("mariadb -h %s -u %s %s < /stage/db.sql",
			a.db.InternalHost(), s.DBUser, s.DBName)
		if _, err := a.runOneShot(ctx, s, "mariadb:11.4", []string{"sh", "-c", importCmd}, []string{"MYSQL_PWD=" + s.DBPassword},
			[]string{stage + ":/stage:ro"}, websites.NetworkName(s.SiteID)); err != nil {
			rollbackFiles()
			return resp, fmt.Errorf("database restore failed: %w", err)
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 90, Message: "database restored", Level: "info"})
	}
	if haveFiles {
		_ = os.RemoveAll(asideDir)
		if container != nil {
			if err := a.docker.Start(ctx, container.ID); err != nil {
				return resp, fmt.Errorf("files and database restored, but restarting the site container failed: %w", err)
			}
		}
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
	var extracted int64 // cumulative decompression-bomb guard (see maxExtractBytes)
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
			extracted += hdr.Size
			if extracted > maxExtractBytes {
				return "", fmt.Errorf("archive extracts to more than %d bytes; refusing (possible decompression bomb)", maxExtractBytes)
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
	if err := validateSiteBasic(s); err != nil {
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
		out, err := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", args, env, nil, websites.NetworkName(s.SiteID))
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
	out, err := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", cmd.Args, env, nil, websites.NetworkName(s.SiteID))
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
	if err := security.ValidateComposePolicy(req.Site.ComposeYAML, siteDir, s.Domain, s.AdminDomain); err != nil {
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
	// Recreate ONLY this site's container from the validated
	// spec. createSite deliberately REUSES an existing container
	// (idempotent retries), so calling it alone would report
	// success while the running container keeps the old labels,
	// env and limits — the previous behaviour. Remove the existing
	// container explicitly first, then let createSite build the
	// replacement from the new spec.
	if existing, rerr := a.resolveSiteContainer(ctx, s); rerr == nil && existing != nil {
		_ = a.docker.Stop(ctx, existing.ID, 15)
		if rerr := a.docker.Remove(ctx, existing.ID, false); rerr != nil {
			return resp, fmt.Errorf("removing the previous container for recreation: %w", rerr)
		}
	}
	if _, err := a.createSite(ctx, agentproto.Request{Op: agentproto.OpCreateSite, Site: s}); err != nil {
		return resp, err
	}
	// A domain change rides along in Arg (the previous domain):
	// routing has moved to the new name, but every URL inside
	// WordPress still points at the old one until it is rewritten.
	// Search-replace is serialization-safe via WP-CLI.
	if req.Arg != "" && req.Arg != s.Domain {
		oldURL, newURL := "https://"+req.Arg, "https://"+s.Domain
		wpEnv := []string{"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName, "WORDPRESS_DB_USER=" + s.DBUser, "WORDPRESS_DB_PASSWORD=" + s.DBPassword}
		network := websites.NetworkName(s.SiteID)
		if _, err := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", wordpress.SearchReplaceArgs(oldURL, newURL), wpEnv, nil, network); err != nil {
			return resp, fmt.Errorf("configuration applied under %s, but rewriting the site's internal URLs from %s failed: %w", s.Domain, req.Arg, err)
		}
		for _, args := range [][]string{
			{"wp", "option", "update", "siteurl", newURL, "--allow-root"},
			{"wp", "option", "update", "home", newURL, "--allow-root"},
			{"wp", "cache", "flush", "--allow-root"},
		} {
			_, _ = a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", args, wpEnv, nil, network)
		}
		resp.Events = append(resp.Events, agentproto.Event{Progress: 90, Message: "site URLs rewritten from " + oldURL + " to " + newURL, Level: "info"})
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
	c, err := a.resolveSiteContainer(ctx, s)
	if err != nil {
		return nil, err
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
	out, err := a.mariadbExec(ctx, []string{q})
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
