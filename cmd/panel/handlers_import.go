package main

import (
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/configgen"
	"github.com/Udhayaboopathi/hPanel-wp/internal/monitoring"
	"github.com/Udhayaboopathi/hPanel-wp/internal/provisioning"
	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
	"github.com/Udhayaboopathi/hPanel-wp/internal/wpimport"
)

// One-upload migration limits. Uploads stream to disk (never held
// in memory); the caps exist so one request cannot fill the disk.
const (
	maxImportZipBytes = 2 << 30
	maxImportDBBytes  = 1 << 30
	maxImportSQLBytes = 4 << 30
)

// importSubmit accepts the one-upload migration form: the site's
// wp-content as a ZIP (WPvivid layout included - the database dump
// may ride inside it) plus an optional separate database file. The
// artifacts are staged inside the new site's own directory, the
// dump is normalised to plain SQL and analysed (table prefix +
// source URL), the website record is created exactly like a normal
// create, and an import_site job performs the deployment.
func (a *App) importSubmit(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		sites, _, _ := a.store.ListWebsites("", "all", 500, 0)
		a.render(w, "import.html", pageData{Title: "Migrate a WordPress Website", Admin: adminFrom(r), CSRF: sessionFrom(r).CSRFToken,
			Error: msg, Data: map[string]any{"Sites": sites}})
	}
	// The auth middleware has normally parsed the multipart body
	// already (so its CSRF field check could see the token); a
	// second call is a no-op. File parts beyond the memory cap are
	// in temp files, removed when the request completes.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail("The upload must be sent as a form with files (multipart).")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	siteID := websites.GenerateSiteID()
	siteDir := filepath.Join(a.cfg.SitesDir, siteID)
	importDir := filepath.Join(siteDir, "import")
	if err := os.MkdirAll(importDir, 0o750); err != nil {
		fail("Could not create the staging directory: " + err.Error())
		return
	}
	abort := func(msg string) {
		_ = os.RemoveAll(siteDir)
		fail(msg)
	}
	fields := map[string]string{}
	for _, name := range []string{"domain", "title", "admin_user", "admin_email", "php_version", "resource_profile"} {
		fields[name] = strings.TrimSpace(r.FormValue(name))
	}
	fields["admin_password"] = r.FormValue("admin_password") // never trimmed
	var zipPath, dbRawPath string
	var zipSize, dbSize int64
	zipFile, _, err := r.FormFile("files_zip")
	if err != nil {
		abort("The wp-content ZIP file is required.")
		return
	}
	zipPath = filepath.Join(importDir, "upload.zip")
	zipSize, err = streamToFile(zipFile, zipPath, maxImportZipBytes)
	zipFile.Close()
	if err != nil {
		abort(err.Error())
		return
	}
	if dbFile, _, err := r.FormFile("db_file"); err == nil {
		dbRawPath = filepath.Join(importDir, "db-raw")
		dbSize, err = streamToFile(dbFile, dbRawPath, maxImportDBBytes)
		dbFile.Close()
		if err != nil {
			abort(err.Error())
			return
		}
	}
	in := websites.CreateInput{
		Domain: fields["domain"], Title: fields["title"],
		AdminUser: fields["admin_user"], AdminEmail: fields["admin_email"],
		AdminPassword: fields["admin_password"],
		PHPVersion:    fields["php_version"], Profile: fields["resource_profile"],
	}
	if err := in.Validate(); err != nil {
		abort(err.Error())
		return
	}
	profile, err := configgen.ProfileFor(in.Profile, in.CPULimit, in.MemLimitMB)
	if err != nil {
		abort(err.Error())
		return
	}

	// Analyse the ZIP: locate the wp-content root and (if the dump
	// was not uploaded separately) the embedded database file.
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		abort("The uploaded file is not a readable ZIP archive: " + err.Error())
		return
	}
	names := make([]string, 0, len(zr.File))
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	contentRoot, err := wpimport.FindContentRoot(names)
	if err != nil {
		zr.Close()
		abort("The ZIP does not contain a WordPress wp-content folder: " + err.Error())
		return
	}
	if dbRawPath == "" {
		entry := wpimport.FindDBEntry(names, contentRoot)
		if entry == "" {
			zr.Close()
			abort("No database dump found: the ZIP contains no WPvivid *-db file or .sql(.gz), and no separate database file was uploaded. Export the old database and add it to the form.")
			return
		}
		rc, err := byName[entry].Open()
		if err != nil {
			zr.Close()
			abort("Could not read the database file inside the ZIP: " + err.Error())
			return
		}
		dbRawPath = filepath.Join(importDir, "db-raw")
		n, err := streamToFile(rc, dbRawPath, maxImportDBBytes)
		rc.Close()
		if err != nil {
			zr.Close()
			abort(err.Error())
			return
		}
		dbSize = n
	}
	zr.Close()

	// Normalise the dump to plain SQL the agent can import.
	dbSQLPath := filepath.Join(importDir, "db.sql")
	format, err := normalizeDBDump(dbRawPath, dbSQLPath)
	if err != nil {
		abort("The database file could not be prepared: " + err.Error())
		return
	}
	_ = os.Remove(dbRawPath)

	// Detect the two facts the deployment needs from the dump.
	sqlFile, err := os.Open(dbSQLPath)
	if err != nil {
		abort(err.Error())
		return
	}
	prefix, sourceURL, derr := wpimport.DetectPrefixAndSiteURL(sqlFile)
	sqlFile.Close()
	if derr != nil {
		abort("Reading the database dump failed: " + derr.Error())
		return
	}
	if prefix == "" && sourceURL == "" {
		abort("The dump does not look like a WordPress database (no options table or siteurl found in it). Nothing was created.")
		return
	}
	if prefix == "" {
		prefix = "wp_"
	}
	if err := security.ValidateTablePrefix(prefix); err != nil {
		abort("The database dump uses an unsupported table prefix: " + err.Error())
		return
	}

	// Same collision and capacity discipline as a normal create.
	if _, err := a.store.GetWebsiteByDomain(in.Domain); err == nil {
		abort("A website with domain " + in.Domain + " already exists in the panel.")
		return
	}
	if all, _, _ := a.store.ListWebsites("", "all", 1000, 0); all != nil {
		for _, s := range all {
			if s.Domain == in.AdminDomain || s.AdminDomain == in.AdminDomain {
				abort("The admin domain " + in.AdminDomain + " is already used by " + s.Domain + ". Choose a different domain.")
				return
			}
			if s.AdminDomain == in.Domain {
				abort("Domain " + in.Domain + " is already used as the admin alias of " + s.Domain + ".")
				return
			}
		}
	}
	host := monitoring.Sample("/")
	needFree := uint64(zipSize + dbSize + (2 << 30))
	if host.DiskTotal > 0 && host.DiskFree < needFree {
		abort("Not enough free disk space for this import (the upload plus working room exceeds what is free).")
		return
	}
	if host.MemTotal > 0 && host.MemAvail < 512<<20 {
		abort("Less than 512 MB of RAM is available; refusing to start an import.")
		return
	}

	dbName := websites.DBNameFor(siteID)
	if sites, _, _ := a.store.ListWebsites("", "all", 1000, 0); sites != nil {
		for _, s := range sites {
			if s.DBName == dbName {
				abort("Database name collision; please retry the import.")
				return
			}
		}
	}
	dbUser := websites.DBUserFor(siteID)
	dbPW, err := genDBPassword()
	if err != nil {
		abort(err.Error())
		return
	}
	encPW, err := a.box.Encrypt(dbPW)
	if err != nil {
		abort(err.Error())
		return
	}
	site := &store.Website{
		SiteID: siteID, Name: in.Title, Domain: in.Domain, AdminDomain: in.AdminDomain,
		Title: in.Title, Status: "provisioning", PHPVersion: in.PHPVersion,
		ResourceProfile: in.Profile, CPULimit: profile.CPUs, MemLimitMB: profile.MemMB,
		ContainerName: websites.ContainerName(siteID),
		DBName:        dbName, DBUser: dbUser, DBPasswordEnc: encPW, TablePrefix: prefix,
		AdminUsername: in.AdminUser, AdminEmail: in.AdminEmail, SiteDir: siteDir,
	}
	if _, err := a.store.CreateWebsite(site); err != nil {
		abort("Could not save website: " + err.Error())
		return
	}
	// Past this point the site record exists: failures keep the
	// record (status failed) exactly like a failed create, and the
	// staged artifacts stay for diagnosis.
	spec := configgen.SiteSpec{
		SiteID: siteID, Domain: in.Domain, AdminDomain: in.AdminDomain, Title: in.Title,
		DBName: dbName, DBUser: dbUser, DBHost: a.cfg.SiteDBHost,
		PHPVersion: in.PHPVersion, Profile: profile, SiteDir: siteDir,
		ContainerName: site.ContainerName, Network: websites.NetworkName(siteID),
		TablePrefix: prefix,
	}
	composeYAML, err := configgen.GenerateCompose(spec)
	if err != nil {
		_ = a.store.UpdateWebsite(siteID, map[string]any{"status": "failed"})
		fail("Could not generate the site configuration: " + err.Error())
		return
	}
	envJSON, _ := json.Marshal(map[string]string{
		"PHP_MEMORY_LIMIT": "256M", "UPLOAD_MAX_FILESIZE": "64M", "POST_MAX_SIZE": "64M", "WORDPRESS_DEBUG": "0",
	})
	if _, err := a.store.SaveConfigVersion(site.ID, string(envJSON), composeYAML, "initial configuration (import)"); err != nil {
		a.log.Warn("save config version (import)", "err", err)
	}
	a.audit(r, "website.import", siteID, "domain="+in.Domain+" prefix="+prefix+" source="+sourceURL+" db_format="+format)
	agentSpec := agentproto.SiteSpec{
		SiteID: siteID, Domain: in.Domain, AdminDomain: in.AdminDomain, Title: in.Title,
		DBName: dbName, DBUser: dbUser, DBPassword: dbPW,
		AdminUser: in.AdminUser, AdminEmail: in.AdminEmail,
		PHPVersion: in.PHPVersion, Profile: in.Profile, CPULimit: profile.CPUs, MemLimitMB: profile.MemMB,
		SiteDir: siteDir, ContainerName: site.ContainerName, TablePrefix: prefix,
	}
	meta := provisioning.ImportMeta{ZipPath: zipPath, DBPath: dbSQLPath, SourceURL: sourceURL}
	job, err := a.runner.EnqueueImport(site, agentSpec, in.AdminPassword, meta)
	if err != nil {
		_ = a.store.UpdateWebsite(siteID, map[string]any{"status": "failed"})
		fail("Could not start the import job: " + err.Error())
		return
	}
	_ = a.store.AddEvent(job.ID, "info", fmt.Sprintf("backup analysed: database format %s, table prefix %q, source URL %s", format, prefix, orNone(sourceURL)))
	http.Redirect(w, r, "/websites/"+siteID+"?flash=Import started. Follow progress in Deployment Jobs.", http.StatusSeeOther)
}

func orNone(s string) string {
	if s == "" {
		return "(not detected)"
	}
	return s
}

// streamToFile copies src to dst enforcing a hard byte cap.
func streamToFile(src io.Reader, dst string, limit int64) (int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(src, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("saving an uploaded file failed: %w", err)
	}
	if n > limit {
		_ = os.Remove(dst)
		return 0, fmt.Errorf("an uploaded file exceeds the %d MB limit", limit>>20)
	}
	return n, nil
}

// normalizeDBDump converts an uploaded database file (plain SQL,
// gzipped SQL - the WPvivid *-db form - or a ZIP wrapping one of
// those) into a plain SQL file at dst. It returns a short label of
// the detected input format.
func normalizeDBDump(src, dst string) (string, error) {
	head := make([]byte, 4)
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	if _, err := io.ReadFull(f, head); err != nil && err != io.ErrUnexpectedEOF {
		f.Close()
		return "", err
	}
	f.Close()
	switch {
	case wpimport.IsGzipHeader(head):
		gz, err := openGzip(src)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		if _, err := streamToFile(gz, dst, maxImportSQLBytes); err != nil {
			return "", err
		}
		return "gzipped SQL (WPvivid-style)", nil
	case head[0] == 'P' && head[1] == 'K':
		zr, err := zip.OpenReader(src)
		if err != nil {
			return "", fmt.Errorf("the database ZIP could not be opened: %w", err)
		}
		defer zr.Close()
		for _, zf := range zr.File {
			if !wpimport.IsDBBackupName(zf.Name) || zf.FileInfo().IsDir() {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			if strings.HasSuffix(strings.ToLower(zf.Name), ".gz") {
				gz, err := gzip.NewReader(rc)
				if err != nil {
					rc.Close()
					return "", err
				}
				_, err = streamToFile(gz, dst, maxImportSQLBytes)
				rc.Close()
				if err != nil {
					return "", err
				}
				return "ZIP containing gzipped SQL", nil
			}
			_, err = streamToFile(rc, dst, maxImportSQLBytes)
			rc.Close()
			if err != nil {
				return "", err
			}
			return "ZIP containing SQL", nil
		}
		return "", fmt.Errorf("the ZIP contains no .sql or .sql.gz database file")
	default:
		if _, err := copyFileTo(src, dst, maxImportSQLBytes); err != nil {
			return "", err
		}
		return "plain SQL", nil
	}
}

func openGzip(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("the gzipped database file is corrupt: %w", err)
	}
	return &gzipFile{f: f, r: gz}, nil
}

type gzipFile struct {
	f *os.File
	r *gzip.Reader
}

func (g *gzipFile) Read(p []byte) (int, error) { return g.r.Read(p) }
func (g *gzipFile) Close() error {
	_ = g.r.Close()
	return g.f.Close()
}

func copyFileTo(src, dst string, limit int64) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return streamToFile(f, dst, limit)
}
