package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/dbconfig"
	"github.com/Udhayaboopathi/hPanel-wp/internal/docker"
)

// recordingDaemon is a fake Docker daemon on a unix socket serving
// the list/inspect/action endpoints the agent's lifecycle uses, and
// recording every mutating call so tests can prove the RIGHT
// container (and only it) was acted on.
type recordingDaemon struct {
	containers []docker.Container
	mu         sync.Mutex
	calls      []string // "restart:<id>", "start:<id>", "stop:<id>"
}

func (d *recordingDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.47/containers/json", func(w http.ResponseWriter, r *http.Request) {
		var want [][2]string
		if raw := r.URL.Query().Get("filters"); raw != "" {
			var filters map[string]map[string]bool
			if err := json.Unmarshal([]byte(raw), &filters); err == nil {
				for kv := range filters["label"] {
					if k, v, ok := strings.Cut(kv, "="); ok {
						want = append(want, [2]string{k, v})
					}
				}
			}
		}
		var out []docker.Container
		for _, c := range d.containers {
			ok := true
			for _, kv := range want {
				if c.Labels[kv[0]] != kv[1] {
					ok = false
				}
			}
			if ok {
				out = append(out, c)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/v1.47/containers/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1.47/containers/")
		id, action, _ := strings.Cut(rest, "/")
		var found *docker.Container
		for i := range d.containers {
			if d.containers[i].ID == id {
				found = &d.containers[i]
			}
		}
		if found == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such container"})
			return
		}
		if r.Method == http.MethodGet && action == "json" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     found.ID,
				"Config": map[string]any{"Labels": found.Labels},
				"State":  map[string]any{"Running": found.State == "running"},
			})
			return
		}
		if r.Method == http.MethodPost && (action == "start" || action == "stop" || action == "restart") {
			d.mu.Lock()
			d.calls = append(d.calls, action+":"+id)
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func (d *recordingDaemon) called() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.calls...)
}

const (
	lcIDA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lcIDB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func lcContainer(id, name, siteID, state string) docker.Container {
	return docker.Container{ID: id, Names: []string{"/" + name}, Image: "wordpress:6.7.2-php8.3-apache", State: state,
		Labels: map[string]string{docker.ManagedLabel: "true", "own-hpanel.site-id": siteID, "own-hpanel.resource": "wordpress"}}
}

func lcAgent(t *testing.T, d *recordingDaemon) *Agent {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
	return &Agent{
		docker:   docker.New(sock),
		db:       dbconfig.Config{Container: "wordpress-mariadb"},
		sitesDir: t.TempDir(),
	}
}

func lcSpec() agentproto.SiteSpec {
	return agentproto.SiteSpec{
		SiteID: "wp-1c46776b", ContainerName: "own-hpanel-wp-1c46776b",
		Domain: "sudoinnovation.tech", PHPVersion: "8.3", Profile: "lightweight",
		DBName: "wp_1c46776b_db", DBUser: "wp_1c46776b_usr",
	}
}

func lcRestart(t *testing.T, a *Agent) (*agentproto.Response, error) {
	t.Helper()
	return a.lifecycle(context.Background(), agentproto.Request{Op: agentproto.OpRestartSite, Site: lcSpec()}, "restart")
}

// Scenario A: container exists with the expected name -> restarted.
func TestLifecycleRestartExpectedName(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-1c46776b", "running")}}
	resp, err := lcRestart(t, lcAgent(t, d))
	if err != nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if resp.ContainerID != lcIDA {
		t.Fatalf("ContainerID = %q, want %q", resp.ContainerID, lcIDA)
	}
	if got := d.called(); len(got) != 1 || got[0] != "restart:"+lcIDA {
		t.Fatalf("calls = %v", got)
	}
}

// Scenario B/G: the container was recreated under a DIFFERENT name
// (e.g. by Compose) and the panel's stored name/ID are stale - the
// labels still resolve it, and the fresh ID is reported back so the
// panel can reconcile its record.
func TestLifecycleRestartRenamedContainerStaleMetadata(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{lcContainer(lcIDB, "some-compose-name-1", "wp-1c46776b", "running")}}
	resp, err := lcRestart(t, lcAgent(t, d))
	if err != nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if resp.ContainerID != lcIDB {
		t.Fatalf("ContainerID = %q, want the recreated container %q", resp.ContainerID, lcIDB)
	}
	if got := d.called(); len(got) != 1 || got[0] != "restart:"+lcIDB {
		t.Fatalf("calls = %v", got)
	}
}

// Scenario D: restart of a STOPPED container starts it (documented
// lifecycle semantics) and never calls restart on it.
func TestLifecycleRestartStoppedStarts(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-1c46776b", "exited")}}
	resp, err := lcRestart(t, lcAgent(t, d))
	if err != nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := d.called(); len(got) != 1 || got[0] != "start:"+lcIDA {
		t.Fatalf("calls = %v, want exactly one start", got)
	}
}

// Start of a running container and stop of a stopped one are no-op
// successes (Docker would answer 304; the panel must not surface
// that as a failure).
func TestLifecycleIdempotentNoOps(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-1c46776b", "running")}}
	a := lcAgent(t, d)
	resp, err := a.lifecycle(context.Background(), agentproto.Request{Site: lcSpec()}, "start")
	if err != nil || !resp.OK || len(d.called()) != 0 {
		t.Fatalf("start-on-running: resp=%+v err=%v calls=%v", resp, err, d.called())
	}
	d.containers[0].State = "exited"
	resp, err = a.lifecycle(context.Background(), agentproto.Request{Site: lcSpec()}, "stop")
	if err != nil || !resp.OK || len(d.called()) != 0 {
		t.Fatalf("stop-on-stopped: resp=%+v err=%v calls=%v", resp, err, d.called())
	}
}

// Scenario E: the container is GENUINELY missing (the production
// case: creation failed before the container ever existed). The
// error is categorised not_found, mentions the label-based
// resolution and the Repair path, and nothing is acted on.
func TestLifecycleMissingContainerCategorised(t *testing.T) {
	d := &recordingDaemon{}
	_, err := lcRestart(t, lcAgent(t, d))
	var oe *opError
	if !errors.As(err, &oe) || oe.category != agentproto.ErrCatNotFound {
		t.Fatalf("err = %v, want opError category not_found", err)
	}
	if !strings.Contains(err.Error(), "Repair Website") {
		t.Fatalf("error lacks the recovery hint: %v", err)
	}
	if len(d.called()) != 0 {
		t.Fatalf("no container action may happen, got %v", d.called())
	}
}

// Scenario F: a container with the EXPECTED NAME exists but belongs
// to ANOTHER site (or is foreign): guard error, never restarted.
func TestLifecycleNameSquatterGuarded(t *testing.T) {
	foreign := lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-othersite", "running")
	d := &recordingDaemon{containers: []docker.Container{foreign}}
	_, err := lcRestart(t, lcAgent(t, d))
	var oe *opError
	if !errors.As(err, &oe) || oe.category != agentproto.ErrCatGuard {
		t.Fatalf("err = %v, want opError category guard", err)
	}
	if len(d.called()) != 0 {
		t.Fatalf("the foreign container must not be touched, got %v", d.called())
	}
}

// Scenario F2: the site's own container plus ANOTHER site's container
// co-exist; only the site's own is restarted.
func TestLifecycleActsOnlyOnOwnContainer(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{
		lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-1c46776b", "running"),
		lcContainer(lcIDB, "own-hpanel-wp-other", "wp-other", "running"),
	}}
	resp, err := lcRestart(t, lcAgent(t, d))
	if err != nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := d.called(); len(got) != 1 || got[0] != "restart:"+lcIDA {
		t.Fatalf("calls = %v, want only own container restarted", got)
	}
}

// Scenario H: Docker daemon unavailable -> a transport error, NOT a
// "not found" categorisation (the panel retries those, bounded).
func TestLifecycleDaemonDownIsNotNotFound(t *testing.T) {
	a := &Agent{
		docker:   docker.New(filepath.Join(t.TempDir(), "no-such.sock")),
		db:       dbconfig.Config{Container: "wordpress-mariadb"},
		sitesDir: t.TempDir(),
	}
	_, err := lcRestart(t, a)
	if err == nil {
		t.Fatal("expected an error when the daemon is down")
	}
	var oe *opError
	if errors.As(err, &oe) {
		t.Fatalf("daemon failure must not be categorised as an opError, got %v (%s)", err, oe.category)
	}
}

// Repair refuses to invent data: when the container is missing AND
// the site files are gone, it aborts with data_missing before any
// Docker or DB mutation.
func TestRepairAbortsWhenFilesMissing(t *testing.T) {
	d := &recordingDaemon{}
	a := lcAgent(t, d) // sitesDir is an empty temp dir: no site files
	spec := lcSpec()
	spec.DBPassword = "supplied-by-panel"
	_, err := a.repairSite(context.Background(), agentproto.Request{Op: agentproto.OpRepairSite, Site: spec})
	var oe *opError
	if !errors.As(err, &oe) || oe.category != agentproto.ErrCatDataMissing {
		t.Fatalf("err = %v, want opError category data_missing", err)
	}
	if len(d.called()) != 0 {
		t.Fatalf("no container action may happen, got %v", d.called())
	}
}

// Repair on a healthy site is a no-op success.
func TestRepairNoOpWhenContainerExists(t *testing.T) {
	d := &recordingDaemon{containers: []docker.Container{lcContainer(lcIDA, "own-hpanel-wp-1c46776b", "wp-1c46776b", "running")}}
	a := lcAgent(t, d)
	spec := lcSpec()
	spec.DBPassword = "supplied-by-panel"
	resp, err := a.repairSite(context.Background(), agentproto.Request{Op: agentproto.OpRepairSite, Site: spec})
	if err != nil || !resp.OK || resp.ContainerID != lcIDA {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(d.called()) != 0 {
		t.Fatalf("healthy repair must not act on the container, got %v", d.called())
	}
}
