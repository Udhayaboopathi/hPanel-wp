//go:build integration

package docker

import (
	"context"
	"testing"
	"time"
)

// Integration tests require a real Docker daemon. They are excluded from the
// default `go test ./...` run and were NOT executed in the build VM (no
// Docker available there). Run with: go test -tags integration ./internal/docker/
func TestDockerPingAndListManaged(t *testing.T) {
	c := New("")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("docker ping: %v", err)
	}
	list, err := c.ListContainers(ctx, true, ManagedLabel+"=true")
	if err != nil {
		t.Fatalf("list managed: %v", err)
	}
	t.Logf("managed containers: %d", len(list))
}

func TestProductionGuardRejectsUnmanaged(t *testing.T) {
	c := New("")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	all, err := c.ListContainers(ctx, true, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, cont := range all {
		if cont.Labels[ManagedLabel] != "true" {
			if err := c.Guard(ctx, cont.ID); err == nil {
				t.Fatalf("guard must refuse unmanaged container %s", cont.ID)
			}
			return
		}
	}
	t.Skip("no unmanaged container present to test the guard against")
}
