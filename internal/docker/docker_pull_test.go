package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDaemon serves the tiny slice of the Docker API that
// CreateContainer/EnsureImage use, on a real unix socket, so the pull
// behaviour can be tested without a Docker daemon.
type fakeDaemon struct {
	imagePresent bool
	pullStream   string
	pullCalls    int
	createCalls  int
	t            *testing.T
}

func (f *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.47/images/create", func(w http.ResponseWriter, r *http.Request) {
		f.pullCalls++
		if got := r.URL.Query().Get("fromImage"); got != "wordpress" {
			f.t.Errorf("fromImage = %q, want %q", got, "wordpress")
		}
		if got := r.URL.Query().Get("tag"); got != "6.7.2-php8.3-apache" {
			f.t.Errorf("tag = %q, want %q", got, "6.7.2-php8.3-apache")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, f.pullStream)
	})
	mux.HandleFunc("/v1.47/containers/create", func(w http.ResponseWriter, r *http.Request) {
		f.createCalls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Id":"fake-container-id"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1.47/images/") && strings.HasSuffix(r.URL.Path, "/json") {
			if f.imagePresent {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"Id":"sha256:fake"}`)
				return
			}
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"message":"not implemented in fake"}`, http.StatusNotImplemented)
	})
	return mux
}

func newFakeClient(t *testing.T, f *fakeDaemon) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: f.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return New(sock)
}

const wpImage = "wordpress:6.7.2-php8.3-apache"

func TestCreateContainerPullsMissingImage(t *testing.T) {
	f := &fakeDaemon{
		t: t,
		pullStream: "{\"status\":\"Pulling from library/wordpress\"}\n" +
			"{\"status\":\"Digest: sha256:fake\"}\n" +
			"{\"status\":\"Status: Downloaded newer image\"}\n",
	}
	c := newFakeClient(t, f)
	id, err := c.CreateContainer(context.Background(), "own-hpanel-wp-test", CreateConfig{Image: wpImage})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if id != "fake-container-id" {
		t.Errorf("id = %q, want fake-container-id", id)
	}
	if f.pullCalls != 1 {
		t.Errorf("pullCalls = %d, want 1 (image was missing)", f.pullCalls)
	}
	if f.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", f.createCalls)
	}
}

func TestCreateContainerSkipsPullWhenImagePresent(t *testing.T) {
	f := &fakeDaemon{t: t, imagePresent: true}
	c := newFakeClient(t, f)
	if _, err := c.CreateContainer(context.Background(), "own-hpanel-wp-test", CreateConfig{Image: wpImage}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if f.pullCalls != 0 {
		t.Errorf("pullCalls = %d, want 0 (image already present)", f.pullCalls)
	}
	if f.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", f.createCalls)
	}
}

func TestCreateContainerSurfacesPullStreamError(t *testing.T) {
	f := &fakeDaemon{
		t: t,
		pullStream: "{\"status\":\"Pulling from library/wordpress\"}\n" +
			"{\"errorDetail\":{\"message\":\"manifest unknown\"},\"error\":\"manifest unknown\"}\n",
	}
	c := newFakeClient(t, f)
	_, err := c.CreateContainer(context.Background(), "own-hpanel-wp-test", CreateConfig{Image: wpImage})
	if err == nil {
		t.Fatal("CreateContainer succeeded, want the embedded pull error to fail it")
	}
	if !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("error = %q, want it to mention the pull failure", err)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (create must not run after a failed pull)", f.createCalls)
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := map[string][2]string{
		"wordpress:6.7.2-php8.3-apache":     {"wordpress", "6.7.2-php8.3-apache"},
		"wordpress":                         {"wordpress", ""},
		"registry.local:5000/wp:1.0":        {"registry.local:5000/wp", "1.0"},
		"docker.io/library/wordpress:6.7.2": {"docker.io/library/wordpress", "6.7.2"},
		"registry.local:5000/wp":            {"registry.local:5000/wp", ""},
	}
	for in, want := range cases {
		name, tag := splitImageRef(in)
		if name != want[0] || tag != want[1] {
			t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)", in, name, tag, want[0], want[1])
		}
	}
}
