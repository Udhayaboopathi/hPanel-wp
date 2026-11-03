// Package docker talks to the Docker Engine API over the Unix socket using
// only the standard library. It is used exclusively by the provisioning
// agent. A production guard refuses mutating operations on containers that
// do not carry the own-hpanel.managed=true label.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const ManagedLabel = "own-hpanel.managed"

type Client struct {
	Socket string
	http   *http.Client
	// httpLong is used for image pulls: a first pull of a WordPress
	// image can legitimately take several minutes, far beyond the
	// 120s ceiling that is right for ordinary API calls. It shares the
	// transport and has no overall timeout - the caller's context is
	// the deadline.
	httpLong *http.Client
}

func New(socket string) *Client {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		Socket:   socket,
		http:     &http.Client{Transport: tr, Timeout: 120 * time.Second},
		httpLong: &http.Client{Transport: tr},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker API %s %s: %w (is the Docker daemon running?)", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = string(data)
		}
		return fmt.Errorf("docker API %s %s: %s (%s)", method, path, resp.Status, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, "GET", "/_ping", nil, nil)
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

func (c *Client) ListContainers(ctx context.Context, all bool, labelFilter string) ([]Container, error) {
	filters := map[string]map[string]bool{}
	if labelFilter != "" {
		filters["label"] = map[string]bool{labelFilter: true}
	}
	fb, _ := json.Marshal(filters)
	path := "/v1.47/containers/json?all=" + boolStr(all) + "&filters=" + urlQueryEscape(string(fb))
	var out []Container
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}

func (c *Client) Inspect(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, "GET", "/v1.47/containers/"+id+"/json", nil, &out)
	return out, err
}

// IsManaged inspects a container and reports the managed label + site id.
func (c *Client) IsManaged(ctx context.Context, id string) (bool, string, error) {
	info, err := c.Inspect(ctx, id)
	if err != nil {
		return false, "", err
	}
	cfg, _ := info["Config"].(map[string]any)
	labels, _ := cfg["Labels"].(map[string]any)
	managed := labels[ManagedLabel] == "true"
	siteID, _ := labels["own-hpanel.site-id"].(string)
	return managed, siteID, nil
}

// Guard refuses mutation of containers not owned by the panel. The
// dedicated MariaDB container (own-hpanel.component=mariadb) IS
// panel-managed, so its compose lifecycle is allowed here - but the agent
// still never lifecycle-manages it directly (see refuseDBContainer in the
// agent); the only agent operations on it are ConnectNetwork (adding it
// to a new per-site network) and ExecDB (exec of the mariadb SQL client
// only, see below). All pre-existing production containers (redberyl-*,
// construction-*, krweb-*) are NOT panel-managed and are refused.
func (c *Client) Guard(ctx context.Context, id string) error {
	managed, _, err := c.IsManaged(ctx, id)
	if err != nil {
		return err
	}
	if !managed {
		return fmt.Errorf("production guard: container %s is not labelled own-hpanel.managed=true; refusing to modify it", id)
	}
	return nil
}

func (c *Client) FindByName(ctx context.Context, name string) (*Container, error) {
	all, err := c.ListContainers(ctx, true, "")
	if err != nil {
		return nil, err
	}
	for i := range all {
		for _, n := range all[i].Names {
			if strings.TrimPrefix(n, "/") == name {
				return &all[i], nil
			}
		}
	}
	return nil, nil
}

// AmbiguousError reports that more than one container carries a
// site's ownership labels - a state an operator must reconcile by
// hand, because acting on a guessed match could hit the wrong site.
type AmbiguousError struct {
	SiteID string
	Count  int
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("ambiguous container identity: %d containers carry own-hpanel.site-id=%s; refusing to guess - reconcile the duplicates manually", e.Count, e.SiteID)
}

// FindBySiteID resolves a site's WordPress container by its ownership
// labels (own-hpanel.managed=true AND own-hpanel.site-id=<id>) - the
// only trustworthy identity. The container NAME is never used for
// resolution: names are conventions, labels are ownership. Ephemeral
// WP-CLI helper containers carry the site-id label too and are
// excluded via their own-hpanel.ephemeral=true label. Returns
// (nil, nil) when no container matches, *AmbiguousError when several
// do, and silently skips label matches that are not panel-managed
// (a foreign container wearing the site-id label without the managed
// label is not adopted).
func (c *Client) FindBySiteID(ctx context.Context, siteID string) (*Container, error) {
	all, err := c.ListContainers(ctx, true, "own-hpanel.site-id="+siteID)
	if err != nil {
		return nil, err
	}
	var match *Container
	count := 0
	for i := range all {
		cand := &all[i]
		if cand.Labels[ManagedLabel] != "true" {
			continue
		}
		if cand.Labels["own-hpanel.ephemeral"] == "true" {
			continue
		}
		count++
		match = cand
	}
	if count > 1 {
		return nil, &AmbiguousError{SiteID: siteID, Count: count}
	}
	return match, nil
}

type Network struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.do(ctx, "GET", "/v1.47/networks", nil, &out)
	return out, err
}

func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	nets, err := c.ListNetworks(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range nets {
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	exists, err := c.NetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	body := map[string]any{"Name": name, "Driver": "bridge", "Labels": labels, "Internal": false}
	return c.do(ctx, "POST", "/v1.47/networks/create", body, nil)
}

func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	err := c.do(ctx, "POST", "/v1.47/networks/"+network+"/connect", map[string]any{"Container": container}, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// DisconnectNetwork detaches a container from a network (used when
// a site is deleted: the shared MariaDB container is attached to
// every site network and must be detached before the network can
// be removed).
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	return c.do(ctx, "POST", "/v1.47/networks/"+network+"/disconnect", map[string]any{"Container": container, "Force": true}, nil)
}

// RemoveNetwork deletes a network by name or ID.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1.47/networks/"+name, nil, nil)
}

type CreateConfig struct {
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd,omitempty"`
	User             string              `json:"User,omitempty"`
	WorkingDir       string              `json:"WorkingDir,omitempty"`
	Env              []string            `json:"Env,omitempty"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck      map[string]any      `json:"Healthcheck,omitempty"`
	HostConfig       map[string]any      `json:"HostConfig,omitempty"`
	NetworkingConfig map[string]any      `json:"NetworkingConfig,omitempty"`
}

func (c *Client) CreateContainer(ctx context.Context, name string, cfg CreateConfig) (string, error) {
	// The daemon does NOT pull on create: a missing image is a hard
	// 404 ("No such image"). Ensure the image first so no call site
	// can forget - the pinned WordPress/WP-CLI images are not pulled
	// by anything else on a fresh host.
	if cfg.Image != "" {
		if err := c.EnsureImage(ctx, cfg.Image); err != nil {
			return "", err
		}
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, "POST", "/v1.47/containers/create?name="+name, cfg, &out)
	return out.ID, err
}

// ImagePresent reports whether an image reference exists locally.
func (c *Client) ImagePresent(ctx context.Context, image string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/v1.47/images/"+image+"/json", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("docker API image inspect %s: %w (is the Docker daemon running?)", image, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("docker API image inspect %s: unexpected status %s", image, resp.Status)
	}
}

// splitImageRef splits "name:tag" (the tag colon is the one after the
// last slash, so registry ports in the name are not mistaken for tags).
func splitImageRef(image string) (name, tag string) {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, ""
}

// PullImage pulls an image from its registry. The response is a stream
// of JSON progress objects that runs until the pull finishes; it is
// drained in full (no overall client timeout) and any error object
// embedded in the stream - the daemon reports many pull failures that
// way, with an HTTP 200 - is surfaced as an error.
func (c *Client) PullImage(ctx context.Context, image string) error {
	name, tag := splitImageRef(image)
	path := "/v1.47/images/create?fromImage=" + urlQueryEscape(name)
	if tag != "" {
		path += "&tag=" + urlQueryEscape(tag)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpLong.Do(req)
	if err != nil {
		return fmt.Errorf("docker API pull %s: %w (is the Docker daemon running?)", image, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = string(data)
		}
		return fmt.Errorf("docker API pull %s: %s (%s)", image, resp.Status, e.Message)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // progress lines we cannot parse are not failures
		}
		if line.Error != "" {
			msg := line.Error
			if line.ErrorDetail != nil && line.ErrorDetail.Message != "" {
				msg = line.ErrorDetail.Message
			}
			return fmt.Errorf("pulling image %s: %s", image, msg)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("pulling image %s: reading pull progress: %w", image, err)
	}
	return nil
}

// EnsureImage pulls the image if it is not already present locally.
func (c *Client) EnsureImage(ctx context.Context, image string) error {
	present, err := c.ImagePresent(ctx, image)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if err := c.PullImage(ctx, image); err != nil {
		return fmt.Errorf("image %s is not present locally and pulling it failed: %w", image, err)
	}
	return nil
}

// Wait polls inspect until the container exits or ctx is done.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	for {
		info, err := c.Inspect(ctx, id)
		if err != nil {
			return -1, err
		}
		state, _ := info["State"].(map[string]any)
		if state == nil {
			return -1, fmt.Errorf("container %s has no state", id)
		}
		if running, _ := state["Running"].(bool); !running {
			code := 0
			if ec, ok := state["ExitCode"].(float64); ok {
				code = int(ec)
			}
			return code, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// RemoveAny removes a container without the managed guard - used ONLY for
// one-shot containers the agent itself created seconds earlier in the same
// operation (WP-CLI / DB client helpers) after verifying the managed label.
func (c *Client) RemoveAny(ctx context.Context, id string, force bool) error {
	return c.do(ctx, "DELETE", "/v1.47/containers/"+id+"?force="+boolStr(force), nil, nil)
}

// StartUnchecked starts a freshly created one-shot container after the
// caller verified its labels via Inspect.
func (c *Client) StartUnchecked(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1.47/containers/"+id+"/start", nil, nil)
}

func (c *Client) Start(ctx context.Context, id string) error {
	if err := c.Guard(ctx, id); err != nil {
		// Creating flow: freshly created containers are inspected again; if
		// inspect fails the start is still refused.
		return err
	}
	return c.do(ctx, "POST", "/v1.47/containers/"+id+"/start", nil, nil)
}

// StartFresh starts a container created in this provisioning run whose
// labels were set at creation time (Guard inside Start covers it).
func (c *Client) Stop(ctx context.Context, id string, timeoutSec int) error {
	if err := c.Guard(ctx, id); err != nil {
		return err
	}
	return c.do(ctx, "POST", "/v1.47/containers/"+id+"/stop?t="+strconv.Itoa(timeoutSec), nil, nil)
}

func (c *Client) Restart(ctx context.Context, id string, timeoutSec int) error {
	if err := c.Guard(ctx, id); err != nil {
		return err
	}
	return c.do(ctx, "POST", "/v1.47/containers/"+id+"/restart?t="+strconv.Itoa(timeoutSec), nil, nil)
}

func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	if err := c.Guard(ctx, id); err != nil {
		return err
	}
	return c.do(ctx, "DELETE", "/v1.47/containers/"+id+"?force="+boolStr(force), nil, nil)
}

type Stats struct {
	CPUPercent float64
	MemUsage   uint64
	MemLimit   uint64
}

func (c *Client) StatsOneShot(ctx context.Context, id string) (*Stats, error) {
	var raw map[string]any
	if err := c.do(ctx, "GET", "/v1.47/containers/"+id+"/stats?stream=false", nil, &raw); err != nil {
		return nil, err
	}
	st := &Stats{}
	if ms, ok := raw["memory_stats"].(map[string]any); ok {
		st.MemUsage = uint64(num(ms["usage"]))
		st.MemLimit = uint64(num(ms["limit"]))
	}
	if cs, ok := raw["cpu_stats"].(map[string]any); ok {
		if pre, ok := raw["precpu_stats"].(map[string]any); ok {
			// Nested maps are asserted with comma-ok throughout: a
			// partial stats object from the daemon must degrade to
			// zero values, never panic the agent.
			cpuUsage, _ := cs["cpu_usage"].(map[string]any)
			preUsage, _ := pre["cpu_usage"].(map[string]any)
			cpuDelta := num(cpuUsage["total_usage"]) - num(preUsage["total_usage"])
			sysDelta := num(cs["system_cpu_usage"]) - num(pre["system_cpu_usage"])
			cpus := num(cs["online_cpus"])
			if cpus == 0 {
				cpus = 1
			}
			if sysDelta > 0 {
				st.CPUPercent = (cpuDelta / sysDelta) * cpus * 100
			}
		}
	}
	return st, nil
}

func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/v1.47/containers/"+id+"/logs?stdout=1&stderr=1&tail="+strconv.Itoa(tail), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("docker logs request failed: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return readMultiplexed(resp.Body)
}

// Exec runs argv inside a managed container and returns combined output.
// Uses the exec API with a raw hijacked stream over the unix socket.
func (c *Client) Exec(ctx context.Context, container string, argv []string) (string, int, error) {
	if err := c.Guard(ctx, container); err != nil {
		return "", -1, err
	}
	return c.execRun(ctx, container, argv)
}

// ExecDB is the narrow exec-only path into the ONE dedicated MariaDB
// container (production name: wordpress-mariadb, defined by the hPanel
// compose stack). That container IS panel-managed - it carries
// own-hpanel.managed=true and own-hpanel.component=mariadb - and it is
// the only container ExecDB will ever exec into. ExecDB therefore:
//
//   - refuses any argv whose program is not the `mariadb` SQL client,
//   - refuses if the named container does not exist,
//   - refuses unless the container is the panel-managed MariaDB
//     (managed label set AND component=mariadb, and no per-site label),
//   - offers NO start/stop/restart/remove path: it is exec and nothing
//     else. (The agent additionally refuses lifecycle operations on the
//     DB container by name - see refuseDBContainer in cmd/agent.)
//
// SQL statements passed here are built only by internal/database from
// validated identifiers; no user input is executed as raw SQL.
func (c *Client) ExecDB(ctx context.Context, container string, argv []string) (string, int, error) {
	if len(argv) == 0 || argv[0] != "mariadb" {
		return "", -1, fmt.Errorf("ExecDB: only the mariadb SQL client may be executed in the DB container")
	}
	found, err := c.FindByName(ctx, container)
	if err != nil {
		return "", -1, err
	}
	if found == nil {
		return "", -1, fmt.Errorf("ExecDB: DB container %q not found", container)
	}
	if found.Labels["own-hpanel.site-id"] != "" {
		return "", -1, fmt.Errorf("ExecDB: %q is a per-site container, not the DB container", container)
	}
	if found.Labels[ManagedLabel] != "true" || found.Labels["own-hpanel.component"] != "mariadb" {
		return "", -1, fmt.Errorf("ExecDB: %q is not the panel-managed MariaDB container (missing own-hpanel.managed/component=mariadb labels); refusing", container)
	}
	return c.execRun(ctx, container, argv)
}

// ExecDBEnv is ExecDB with environment for the exec instance - used
// to pass the MariaDB password as MYSQL_PWD instead of a -p argv
// entry that would be visible in the container's process list.
func (c *Client) ExecDBEnv(ctx context.Context, container string, argv []string, env []string) (string, int, error) {
	if len(argv) == 0 || argv[0] != "mariadb" {
		return "", -1, fmt.Errorf("ExecDB: only the mariadb SQL client may be executed in the DB container")
	}
	found, err := c.FindByName(ctx, container)
	if err != nil {
		return "", -1, err
	}
	if found == nil {
		return "", -1, fmt.Errorf("ExecDB: DB container %q not found", container)
	}
	if found.Labels["own-hpanel.site-id"] != "" {
		return "", -1, fmt.Errorf("ExecDB: %q is a per-site container, not the DB container", container)
	}
	if found.Labels[ManagedLabel] != "true" || found.Labels["own-hpanel.component"] != "mariadb" {
		return "", -1, fmt.Errorf("ExecDB: %q is not the panel-managed MariaDB container (missing own-hpanel.managed/component=mariadb labels); refusing", container)
	}
	return c.execRunEnv(ctx, container, argv, env)
}

func (c *Client) execRun(ctx context.Context, container string, argv []string) (string, int, error) {
	return c.execRunEnv(ctx, container, argv, nil)
}

// execRunEnv is execRun with extra environment for the exec instance.
// Secrets belong here (e.g. MYSQL_PWD) rather than in argv, where any
// user on the host could read them from the process list.
func (c *Client) execRunEnv(ctx context.Context, container string, argv []string, env []string) (string, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv}
	if len(env) > 0 {
		body["Env"] = env
	}
	if err := c.do(ctx, "POST", "/v1.47/containers/"+container+"/exec", body, &created); err != nil {
		return "", -1, err
	}
	// Start exec via raw unix dial to read the multiplexed stream.
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return "", -1, err
	}
	defer conn.Close()
	// A stalled daemon stream must not outlive the caller's
	// context: close the connection when the context ends so the
	// read below unblocks instead of hanging past the deadline.
	stopWatch := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopWatch()
	payload, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	fmt.Fprintf(conn, "POST /v1.47/exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", created.ID, len(payload), payload)
	br := bufio.NewReader(conn)
	// Skip headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", -1, err
		}
		if line == "\r\n" {
			break
		}
	}
	out, err := readMultiplexed(br)
	if err != nil {
		return out, -1, err
	}
	// The exit code starts at -1 and an inspection error is
	// propagated: defaulting to the zero value would report a
	// failed or uninspectable exec as exit code 0 (success).
	insp := struct {
		ExitCode int `json:"ExitCode"`
	}{ExitCode: -1}
	if err := c.do(ctx, "GET", "/v1.47/exec/"+created.ID+"/json", nil, &insp); err != nil {
		return out, -1, fmt.Errorf("inspecting exec result: %w", err)
	}
	return out, insp.ExitCode, nil
}

// maxStreamBytes bounds the total output one logs/exec call can
// accumulate in memory; individual frames were already capped,
// but the stream as a whole was not.
const maxStreamBytes = 32 << 20

func readMultiplexed(r io.Reader) (string, error) {
	var sb strings.Builder
	hdr := make([]byte, 8)
	for {
		if sb.Len() > maxStreamBytes {
			return sb.String(), fmt.Errorf("docker output exceeded the %d MB safety cap", maxStreamBytes>>20)
		}
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Possibly a non-multiplexed (TTY) stream remainder.
				rest, _ := io.ReadAll(io.LimitReader(r, maxStreamBytes))
				sb.Write(rest)
				return sb.String(), nil
			}
			return sb.String(), nil
		}
		size := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if size < 0 || size > 16<<20 {
			// Not a framed stream; treat header bytes as content and drain.
			sb.Write(hdr)
			rest, _ := io.ReadAll(io.LimitReader(r, maxStreamBytes))
			sb.Write(rest)
			return sb.String(), nil
		}
		if _, err := io.CopyN(&sb, r, int64(size)); err != nil {
			return sb.String(), nil
		}
	}
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
