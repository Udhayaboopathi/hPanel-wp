package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listFakeDaemon serves GET /containers/json from a fixed container
// set, honouring the label filters the client sends (same semantics
// as the real daemon: a label filter "k=v" keeps only containers
// carrying exactly that label pair).
type listFakeDaemon struct {
	containers []Container
}

func (f *listFakeDaemon) handler() http.Handler {
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
		var out []Container
		for _, c := range f.containers {
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
	return mux
}

func serveListFake(t *testing.T, f *listFakeDaemon) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
	return New(sock)
}

func wpContainer(id, name, siteID, state string) Container {
	return Container{ID: id, Names: []string{"/" + name}, Image: "wordpress:6.7.2-php8.3-apache", State: state,
		Labels: map[string]string{ManagedLabel: "true", "own-hpanel.site-id": siteID, "own-hpanel.resource": "wordpress"}}
}

const idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Scenario A/B/C: resolution is by label - expected name, renamed
// container, and recreated container (new ID) all resolve.
func TestFindBySiteIDResolvesByLabel(t *testing.T) {
	for _, tc := range []struct{ name, cname, id string }{
		{"expected name", "own-hpanel-wp-aaaa", idA},
		{"renamed container", "compose-generated-xyz", idA},
		{"recreated new id", "own-hpanel-wp-aaaa", idB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &listFakeDaemon{containers: []Container{wpContainer(tc.id, tc.cname, "wp-aaaa", "running")}}
			got, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
			if err != nil {
				t.Fatalf("FindBySiteID: %v", err)
			}
			if got == nil || got.ID != tc.id {
				t.Fatalf("got %+v, want container %s", got, tc.id)
			}
		})
	}
}

// A stopped container still resolves (state is the caller's concern).
func TestFindBySiteIDStopped(t *testing.T) {
	f := &listFakeDaemon{containers: []Container{wpContainer(idA, "own-hpanel-wp-aaaa", "wp-aaaa", "exited")}}
	got, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
	if err != nil || got == nil || got.State != "exited" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// Ephemeral WP-CLI helpers carry the site-id label and must never be
// resolved as the site's container.
func TestFindBySiteIDExcludesEphemeralHelpers(t *testing.T) {
	helper := wpContainer(idB, "own-hpanel-wp-aaaa-cli-1", "wp-aaaa", "running")
	helper.Labels["own-hpanel.ephemeral"] = "true"
	f := &listFakeDaemon{containers: []Container{helper}}
	got, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
	if err != nil || got != nil {
		t.Fatalf("got %+v err %v, want nil", got, err)
	}
}

// A foreign container wearing the site-id label WITHOUT the managed
// label is not adopted.
func TestFindBySiteIDRequiresManagedLabel(t *testing.T) {
	foreign := wpContainer(idA, "squatter", "wp-aaaa", "running")
	delete(foreign.Labels, ManagedLabel)
	f := &listFakeDaemon{containers: []Container{foreign}}
	got, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
	if err != nil || got != nil {
		t.Fatalf("got %+v err %v, want nil", got, err)
	}
}

// Scenario: another site's container never matches this site.
func TestFindBySiteIDOtherSite(t *testing.T) {
	f := &listFakeDaemon{containers: []Container{wpContainer(idA, "own-hpanel-wp-bbbb", "wp-bbbb", "running")}}
	got, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
	if err != nil || got != nil {
		t.Fatalf("got %+v err %v, want nil", got, err)
	}
}

// Two real containers claiming one site-id is an operator problem,
// never a guess: AmbiguousError.
func TestFindBySiteIDAmbiguous(t *testing.T) {
	f := &listFakeDaemon{containers: []Container{
		wpContainer(idA, "own-hpanel-wp-aaaa", "wp-aaaa", "running"),
		wpContainer(idB, "own-hpanel-wp-aaaa-copy", "wp-aaaa", "running"),
	}}
	_, err := serveListFake(t, f).FindBySiteID(context.Background(), "wp-aaaa")
	var amb *AmbiguousError
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !asAmbiguous(err, &amb) {
		t.Fatalf("err = %v, want AmbiguousError", err)
	}
}

func asAmbiguous(err error, target **AmbiguousError) bool {
	for err != nil {
		if a, ok := err.(*AmbiguousError); ok {
			*target = a
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
