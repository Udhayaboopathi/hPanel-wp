package configgen

import (
	"strings"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

func testSpec() SiteSpec {
	p, _ := ProfileFor("lightweight", 0, 0)
	return SiteSpec{
		SiteID: "wp-ab12cd34", Domain: "example.com", AdminDomain: "wpadmin.example.com",
		Title: "Example", DBName: "wp_ab12cd34_db", DBUser: "wp_ab12cd34_usr",
		DBPassword: "supersecretvalue", DBHost: "construction-mariadb",
		PHPVersion: "8.3", Profile: p, SiteDir: "/opt/own-hpanel/websites/wp-ab12cd34",
		ContainerName: "own-hpanel-wp-ab12cd34", Network: "own-hpanel-net-wp-ab12cd34",
	}
}

func TestTraefikLabels(t *testing.T) {
	labels := TraefikLabels(testSpec())
	if labels["traefik.http.routers.own-hpanel-wp-ab12cd34.entrypoints"] != "websecure" {
		t.Error("router must use websecure entrypoint")
	}
	if labels["traefik.http.routers.own-hpanel-wp-ab12cd34.tls.certresolver"] != "letsencrypt" {
		t.Error("router must use letsencrypt resolver")
	}
	if labels["traefik.docker.network"] != "redberyl-internal" {
		t.Error("router must use redberyl-internal network")
	}
	if !strings.Contains(labels["traefik.http.routers.own-hpanel-wp-ab12cd34.rule"], "Host(`example.com`)") {
		t.Error("router rule must match the domain")
	}
	// Admin alias must be redirect-only to the real wp-admin.
	found := false
	for _, v := range labels {
		if strings.Contains(v, "https://example.com/wp-admin/") {
			found = true
		}
		if strings.Contains(v, "supersecretvalue") {
			t.Error("secret leaked into Traefik labels")
		}
	}
	if !found {
		t.Error("admin redirect label missing")
	}
}

func TestGenerateComposePassesPolicyAndLimits(t *testing.T) {
	y, err := GenerateCompose(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := security.ValidateComposePolicy(y, "/opt/own-hpanel/websites/wp-ab12cd34"); err != nil {
		t.Fatalf("generated compose failed policy: %v", err)
	}
	for _, want := range []string{"512m", "0.50", "no-new-privileges:true", "redberyl-internal", "own-hpanel.managed"} {
		if !strings.Contains(y, want) {
			t.Errorf("compose missing %q", want)
		}
	}
	if strings.Contains(y, "ports:") {
		t.Error("compose must not publish ports")
	}
}

func TestProfilesAndResourceValidation(t *testing.T) {
	std, err := ProfileFor("standard", 0, 0)
	if err != nil || std.MemMB != 768 || std.CPUs != 1.0 {
		t.Errorf("standard profile wrong: %+v %v", std, err)
	}
	if _, err := ProfileFor("custom", 0.5, 128); err == nil {
		t.Error("expected too-small custom memory to be rejected")
	}
	if _, err := ProfileFor("huge", 0, 0); err == nil {
		t.Error("expected unknown profile to be rejected")
	}
	if _, err := PHPImage("7.4"); err == nil {
		t.Error("expected unverified PHP version to be rejected")
	}
}

func TestDNSRecords(t *testing.T) {
	recs := DNSRecords("example.com", "130.210.42.20")
	if len(recs) != 2 || recs[0] != [3]string{"A", "@", "130.210.42.20"} || recs[1] != [3]string{"A", "wpadmin", "130.210.42.20"} {
		t.Errorf("unexpected DNS records: %v", recs)
	}
}
