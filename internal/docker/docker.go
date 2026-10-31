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
	return &Client{Socket: socket, http: &http.Client{Transport: tr, Timeout: 120 * time.Second}}
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

// Guard refuses mutation of containers not owned by the panel. The shared
// pre-existing MariaDB container is NOT panel-managed, so all lifecycle
// operations on it are refused here. The only operations ever performed on
// it are: ConnectNetwork (adding it to a new per-site network - it never
// stops/restarts/recreates the container) and ExecDB (exec of the mariadb
// SQL client only, see below).
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
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, "POST", "/v1.47/containers/create?name="+name, cfg, &out)
	return out.ID, err
}

// PullImage pulls an approved image (used for the pinned WordPress/MariaDB images).
func (c *Client) PullImage(ctx context.Context, image string) error {
	return c.do(ctx, "POST", "/v1.47/images/create?fromImage="+urlQueryEscape(image), nil, nil)
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
			cpuDelta := num(cs["cpu_usage"].(map[string]any)["total_usage"]) - num(pre["cpu_usage"].(map[string]any)["total_usage"])
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

// ExecDB is the narrow exec-only exception for the ONE pre-existing shared
// MariaDB container (production name: construction-mariadb). That container
// is deliberately NOT labelled own-hpanel.managed=true - it belongs to the
// existing production stack - so Guard would (correctly) refuse lifecycle
// operations on it. ExecDB therefore:
//
//   - refuses any argv whose program is not the `mariadb` SQL client,
//   - refuses if the named container does not exist,
//   - offers NO start/stop/restart/remove path: it is exec and nothing else.
//
// SQL statements passed here are built only by internal/database from
// validated identifiers; no user input is executed as raw SQL.
func (c *Client) ExecDB(ctx context.Context, container string, argv []string) (string, int, error) {
	if len(argv) == 0 || argv[0] != "mariadb" {
		return "", -1, fmt.Errorf("ExecDB: only the mariadb SQL client may be executed in the shared DB container")
	}
	found, err := c.FindByName(ctx, container)
	if err != nil {
		return "", -1, err
	}
	if found == nil {
		return "", -1, fmt.Errorf("ExecDB: shared DB container %q not found", container)
	}
	if found.Labels["own-hpanel.site-id"] != "" {
		return "", -1, fmt.Errorf("ExecDB: %q is a per-site container, not the shared DB container", container)
	}
	return c.execRun(ctx, container, argv)
}

func (c *Client) execRun(ctx context.Context, container string, argv []string) (string, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv}
	if err := c.do(ctx, "POST", "/v1.47/containers/"+container+"/exec", body, &created); err != nil {
		return "", -1, err
	}
	// Start exec via raw unix dial to read the multiplexed stream.
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return "", -1, err
	}
	defer conn.Close()
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
	var insp struct {
		ExitCode int `json:"ExitCode"`
	}
	_ = c.do(ctx, "GET", "/v1.47/exec/"+created.ID+"/json", nil, &insp)
	return out, insp.ExitCode, nil
}

func readMultiplexed(r io.Reader) (string, error) {
	var sb strings.Builder
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Possibly a non-multiplexed (TTY) stream remainder.
				rest, _ := io.ReadAll(r)
				sb.Write(rest)
				return sb.String(), nil
			}
			return sb.String(), nil
		}
		size := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if size < 0 || size > 16<<20 {
			// Not a framed stream; treat header bytes as content and drain.
			sb.Write(hdr)
			rest, _ := io.ReadAll(r)
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
