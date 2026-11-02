package provisioning

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// EnqueueImport must persist a secret-free payload, mark the job
// import_site, and refuse a second active job for the same site.
func TestEnqueueImport(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	site := &store.Website{
		SiteID: "wp-imp00001", Name: "Imported", Domain: "imp.example.com",
		AdminDomain: "wpadmin.imp.example.com", Title: "Imported", Status: "provisioning",
		PHPVersion: "8.3", ResourceProfile: "standard", ContainerName: "own-hpanel-wp-imp00001",
		DBName: "wp_imp00001_db", DBUser: "wp_imp00001_usr", DBPasswordEnc: "enc",
		TablePrefix: "fc7_", AdminUsername: "chef", AdminEmail: "a@b.co", SiteDir: "/tmp/wp-imp00001",
	}
	if _, err := st.CreateWebsite(site); err != nil {
		t.Fatal(err)
	}
	r := New(st, nil)
	spec := agentproto.SiteSpec{
		SiteID: site.SiteID, Domain: site.Domain, DBName: site.DBName, DBUser: site.DBUser,
		DBPassword: "db-secret", AdminUser: "chef", AdminEmail: "a@b.co",
		TablePrefix: "fc7_", SiteDir: site.SiteDir, ContainerName: site.ContainerName,
	}
	meta := ImportMeta{ZipPath: "/tmp/wp-imp00001/import/upload.zip", DBPath: "/tmp/wp-imp00001/import/db.sql", SourceURL: "https://old.example.org"}
	job, err := r.EnqueueImport(site, spec, "admin-secret-password", meta)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "import_site" {
		t.Errorf("kind = %q", job.Kind)
	}
	if strings.Contains(job.PayloadJSON, "db-secret") || strings.Contains(job.PayloadJSON, "admin-secret") {
		t.Error("payload leaked a secret")
	}
	var p importPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &p); err != nil {
		t.Fatal(err)
	}
	if p.Import.ZipPath != meta.ZipPath || p.Spec.TablePrefix != "fc7_" || p.Import.SourceURL != meta.SourceURL {
		t.Errorf("payload mismatch: %+v", p)
	}
	if _, err := r.EnqueueImport(site, spec, "admin-secret-password", meta); err == nil {
		t.Error("a second active import job for the same site must be refused")
	}
}
