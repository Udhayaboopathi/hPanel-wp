package store

import "testing"

// Regression tests for the 2026-11-03 full audit fixes.

// ClaimJob is the compare-and-swap that starts a queued job: it
// must succeed exactly once, and never for a job that was
// cancelled before it started (the cancel-at-start race).
func TestClaimJobCAS(t *testing.T) {
	s := openTest(t)
	j := &Job{SiteID: "wp-claim01", Kind: "create_site", Status: "queued"}
	if err := s.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ClaimJob(j.ID)
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true, nil", ok, err)
	}
	ok, err = s.ClaimJob(j.ID)
	if err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false, nil", ok, err)
	}

	j2 := &Job{SiteID: "wp-claim01", Kind: "create_site", Status: "queued"}
	if err := s.CreateJob(j2); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJob(j2.ID, "cancelled", 0, "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}
	ok, err = s.ClaimJob(j2.ID)
	if err != nil || ok {
		t.Fatalf("claim after cancel = %v, %v; want false, nil", ok, err)
	}
}

// A TOTP step may be consumed exactly once: replaying the same
// valid code inside its validity window must fail.
func TestConsumeTOTPStepReplay(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateAdmin("totpadmin", "t@example.com", "hash", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.ConsumeTOTPStep(id, 1000)
	if err != nil || !ok {
		t.Fatalf("first consume = %v, %v; want true, nil", ok, err)
	}
	ok, err = s.ConsumeTOTPStep(id, 1000)
	if err != nil || ok {
		t.Fatalf("replay consume = %v, %v; want false, nil", ok, err)
	}
	ok, err = s.ConsumeTOTPStep(id, 999)
	if err != nil || ok {
		t.Fatalf("older step consume = %v, %v; want false, nil", ok, err)
	}
	ok, err = s.ConsumeTOTPStep(id, 1001)
	if err != nil || !ok {
		t.Fatalf("next step consume = %v, %v; want true, nil", ok, err)
	}
}

// Deleting a website must free its domain for reuse: on MariaDB
// the migration's foreign keys are not enforced the way SQLite
// enforces them, so DeleteWebsite removes the registry rows
// explicitly (a leftover website_domains row burned the domain
// forever).
func TestDeleteWebsiteFreesDomain(t *testing.T) {
	s := openTest(t)
	w := &Website{SiteID: "wp-del0001", Domain: "reuse.example.com", AdminDomain: "wpadmin.reuse.example.com", DBName: "wp_del_db", DBUser: "wp_del_usr", ContainerName: "own-hpanel-wp-del0001"}
	if _, err := s.CreateWebsite(w); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebsite("wp-del0001"); err != nil {
		t.Fatal(err)
	}
	if found, err := s.FindWebsiteByDomainOrAdmin("reuse.example.com"); err == nil {
		t.Fatalf("domain still resolves to %+v after delete", found)
	}
	w2 := &Website{SiteID: "wp-del0002", Domain: "reuse.example.com", AdminDomain: "wpadmin.reuse.example.com", DBName: "wp_del2_db", DBUser: "wp_del2_usr", ContainerName: "own-hpanel-wp-del0002"}
	if _, err := s.CreateWebsite(w2); err != nil {
		t.Fatalf("reusing a deleted site's domain must succeed: %v", err)
	}
}

// CreateWebsite is transactional: when the admin domain collides
// with an existing site, no partial website (or registry rows)
// may remain.
func TestCreateWebsiteRollsBackOnAdminDomainConflict(t *testing.T) {
	s := openTest(t)
	a := &Website{SiteID: "wp-tx00001", Domain: "first.example.com", AdminDomain: "wpadmin.first.example.com", DBName: "wp_tx1_db", DBUser: "wp_tx1_usr", ContainerName: "own-hpanel-wp-tx00001"}
	if _, err := s.CreateWebsite(a); err != nil {
		t.Fatal(err)
	}
	b := &Website{SiteID: "wp-tx00002", Domain: "second.example.com", AdminDomain: "first.example.com", DBName: "wp_tx2_db", DBUser: "wp_tx2_usr", ContainerName: "own-hpanel-wp-tx00002"}
	if _, err := s.CreateWebsite(b); err == nil {
		t.Fatal("expected admin-domain collision to be rejected")
	}
	if _, total, _ := s.ListWebsites("", "all", 10, 0); total != 1 {
		t.Fatalf("expected exactly 1 website after rollback, got %d", total)
	}
	if found, err := s.FindWebsiteByDomainOrAdmin("second.example.com"); err == nil {
		t.Fatalf("rolled-back site's domain must not resolve, got %+v", found)
	}
}
