package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAdminSessionAndAuditFlow(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateAdmin("admin", "a@example.com", "hash", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountAdmins(); n != 1 {
		t.Fatalf("expected 1 admin, got %d", n)
	}
	a, err := s.GetAdminByUsername("admin")
	if err != nil || a.ID != id {
		t.Fatalf("admin lookup failed: %v", err)
	}
	if err := s.CreateSession("hash1", id, "csrf1", "127.0.0.1", "test", futureTime()); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession("hash1")
	if err != nil || sess.AdminID != id || sess.CSRFToken != "csrf1" {
		t.Fatalf("session lookup failed: %+v %v", sess, err)
	}
	if err := s.AddAudit(id, "test.action", "target", "password=hunter2 detail", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.ListAudit(10, 0)
	if len(entries) != 1 || entries[0].Detail == "password=hunter2 detail" {
		t.Fatalf("audit redaction failed: %+v", entries)
	}
}

func TestWebsiteDuplicateDomainRejected(t *testing.T) {
	s := openTest(t)
	w := &Website{SiteID: "wp-aaa111", Domain: "example.com", DBName: "wp_aaa_db", DBUser: "wp_aaa_usr", ContainerName: "own-hpanel-wp-aaa111"}
	if _, err := s.CreateWebsite(w); err != nil {
		t.Fatal(err)
	}
	w2 := &Website{SiteID: "wp-bbb222", Domain: "example.com", DBName: "wp_bbb_db", DBUser: "wp_bbb_usr", ContainerName: "own-hpanel-wp-bbb222"}
	if _, err := s.CreateWebsite(w2); err == nil {
		t.Fatal("expected duplicate domain to be rejected by unique constraint")
	}
	list, total, _ := s.ListWebsites("", "all", 10, 0)
	if total != 1 || len(list) != 1 {
		t.Fatalf("expected 1 site, got total=%d len=%d", total, len(list))
	}
}

func TestJobLifecycleAndCrashRecoveryMarker(t *testing.T) {
	s := openTest(t)
	j := &Job{SiteID: "wp-aaa111", Kind: "create_site", Status: "queued"}
	if err := s.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJob(j.ID, "running", 10, "working", ""); err != nil {
		t.Fatal(err)
	}
	active, _ := s.HasActiveJobForSite("wp-aaa111", "create_site")
	if !active {
		t.Fatal("expected active job to be detected (duplicate prevention)")
	}
	if err := s.UpdateJob(j.ID, "succeeded", 100, "done", ""); err != nil {
		t.Fatal(err)
	}
	active, _ = s.HasActiveJobForSite("wp-aaa111", "create_site")
	if active {
		t.Fatal("succeeded job must not count as active")
	}
	if err := s.AddEvent(j.ID, "info", "step done"); err != nil {
		t.Fatal(err)
	}
	events, _ := s.ListEvents(j.ID, 10)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	s := openTest(t)
	id, _ := s.CreateAdmin("admin", "a@example.com", "hash", "", false)
	if err := s.ReplaceRecoveryCodes(id, []string{"hashA", "hashB"}); err != nil {
		t.Fatal(err)
	}
	ok, _ := s.ConsumeRecoveryCode(id, "hashA")
	if !ok {
		t.Fatal("first use should succeed")
	}
	ok, _ = s.ConsumeRecoveryCode(id, "hashA")
	if ok {
		t.Fatal("second use of same code must fail")
	}
}
