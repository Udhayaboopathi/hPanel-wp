package main

import (
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	var zipSize int64
	zipPath = filepath.Join(importDir, "upload.zip")
	fileHeaders := r.MultipartForm.File["files_zip"]
	switch {
	case len(fileHeaders) == 0:
		abort("The wp-content ZIP file is required.")
		return
	case len(fileHeaders) == 1:
		zipFile, err := fileHeaders[0].Open()
		if err != nil {
			abort(err.Error())
			return
		}
		zipSize, err = streamToFile(zipFile, zipPath, maxImportZipBytes)
		zipFile.Close()
		if err != nil {
			abort(err.Error())
			return
		}
	default:
		// WPvivid multi-part backup set: classify every file,
		// merge the content parts into one wp-content ZIP and
		// keep the database part aside for the normal DB flow.
		var parts []wpimport.MergePart
		var contentTotal int64
		kindsSeen := map[string]bool{}
		for i, fh := range fileHeaders {
			kind := wpimport.PartKind(fh.Filename)
			if kind == "" {
				abort(fmt.Sprintf("The file %q is not a WPvivid backup part (expected names ending in -plugins.zip, -themes.zip, -uploads.zip, -mu-plugins.zip, -others.zip, or the -db database file). Select the complete backup set, or upload one combined wp-content ZIP instead.", fh.Filename))
				return
			}
			src, err := fh.Open()
			if err != nil {
				abort(err.Error())
				return
			}
			staged := filepath.Join(importDir, fmt.Sprintf("part-%d", i))
			// Each part streams against the REMAINING budget, not
			// the full cap: the database part has its own 1 GB
			// budget, and the content parts share the 2 GB budget,
			// so the staged total can never overshoot the limits
			// before the cumulative check gets a chance to run.
			limit := maxImportZipBytes - contentTotal
			if kind == "db" {
				limit = maxImportDBBytes
			}
			if limit <= 0 {
				src.Close()
				abort("The backup parts together exceed the 2 GB limit.")
				return
			}
			n, err := streamToFile(src, staged, limit)
			src.Close()
			if err != nil {
				abort(err.Error())
				return
			}
			if kind != "db" {
				contentTotal += n
			}
			if kind == "db" {
				if dbRawPath != "" {
					abort("Two database files were included in the backup set; keep only one.")
					return
				}
				dbRawPath = staged
				continue
			}
			kindsSeen[kind] = true
			parts = append(parts, wpimport.MergePart{Kind: kind, Path: staged})
		}
		if !kindsSeen["plugins"] || !kindsSeen["themes"] {
			abort("The backup set is incomplete: both the -plugins.zip and -themes.zip parts are required. Select every file of the WPvivid backup.")
			return
		}
		if _, err := wpimport.MergeWPvividParts(zipPath, parts); err != nil {
			abort("The backup parts could not be combined: " + err.Error())
			return
		}
		st, err := os.Stat(zipPath)
		if err != nil {
			abort(err.Error())
			return
		}
		zipSize = st.Size()
		for _, p := range parts {
			_ = os.Remove(p.Path)
		}
	}
	dbOverridden := false
	if dbFile, _, err := r.FormFile("db_file"); err == nil {
		// An explicitly uploaded database file wins over the
		// backup set's own -db part — silently ignoring it (the old
		// behaviour when a set was present) deployed a database the
		// operator did not intend.
		if dbRawPath != "" {
			_ = os.Remove(dbRawPath)
			dbOverridden = true
		}
		dbRawPath = filepath.Join(importDir, "db-raw")
		_, err = streamToFile(dbFile, dbRawPath, maxImportDBBytes)
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
	// Migration semantics: an empty domain means "keep the site's
	// original domain" (read from the backup's database below) and
	// three empty administrator fields mean "keep the backup's
	// original admin account" (its users come across in the dump,
	// password hashes included). Validate everything that does not
	// depend on the dump now; the domain is resolved after the
	// dump is analysed.
	keepDomain := in.Domain == ""
	keepAdmin := in.AdminUser == "" && in.AdminEmail == "" && in.AdminPassword == ""
	if !keepAdmin && (in.AdminUser == "" || in.AdminEmail == "" || in.AdminPassword == "") {
		abort("Fill in all three WordPress administrator fields, or leave all three empty to keep the backup's original admin account.")
		return
	}
	in.Domain = security.NormalizeDomain(in.Domain)
	if !keepDomain {
		if err := security.ValidateDomain(in.Domain); err != nil {
			abort(err.Error())
			return
		}
	}
	if strings.ContainsAny(in.Title, "\r\n") {
		abort("site title must not contain line breaks")
		return
	}
	if !keepAdmin {
		if err := security.ValidateAdminUsername(in.AdminUser); err != nil {
			abort("wordpress admin username: " + err.Error())
			return
		}
		if err := security.ValidateEmail(in.AdminEmail); err != nil {
			abort(err.Error())
			return
		}
		if len(in.AdminPassword) < 12 {
			abort("wordpress admin password must be at least 12 characters")
			return
		}
	}
	if in.PHPVersion == "" {
		in.PHPVersion = "8.3"
	}
	if in.Profile == "" {
		in.Profile = "lightweight"
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
		_, err = streamToFile(rc, dbRawPath, maxImportDBBytes)
		rc.Close()
		if err != nil {
			zr.Close()
			abort(err.Error())
			return
		}
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

	// Resolve the domain now that the dump has been read: an empty
	// form field keeps the site's original domain, so the operator
	// only has to point that domain's DNS at this server.
	if keepDomain {
		host := sourceHost(sourceURL)
		if host == "" {
			abort("The domain field was left empty and the backup's database does not show the site's original address (no siteurl found). Re-submit with the domain filled in.")
			return
		}
		in.Domain = security.NormalizeDomain(host)
		if err := security.ValidateDomain(in.Domain); err != nil {
			abort("The backup's original domain " + host + " cannot be used: " + err.Error())
			return
		}
	}
	if in.AdminDomain == "" {
		in.AdminDomain = "wpadmin." + in.Domain
	}
	in.AdminDomain = security.NormalizeDomain(in.AdminDomain)
	if err := security.ValidateDomain(in.AdminDomain); err != nil {
		abort("admin domain: " + err.Error())
		return
	}
	if in.Title == "" {
		in.Title = in.Domain
	}

	// Same collision and capacity discipline as a normal create.
	// Indexed lookups in both directions (a new primary domain may
	// not equal another site's admin alias, and vice versa) — the
	// old first-1000-rows scans silently stopped checking at scale.
	if _, err := a.store.FindWebsiteByDomainOrAdmin(in.Domain); err == nil {
		abort("Domain " + in.Domain + " is already used by another website in the panel (as its domain or its admin alias).")
		return
	}
	if _, err := a.store.FindWebsiteByDomainOrAdmin(in.AdminDomain); err == nil {
		abort("The admin domain " + in.AdminDomain + " is already used by another website in the panel. Choose a different domain.")
		return
	}
	// Capacity check against the WEBSITES volume (not "/"), with
	// an honest budget: staged uploads plus working headroom
	// (10% of the payload, at least 256 MB) for the running
	// stack. Unknown capacity fails closed — importing blind is
	// how a full disk takes every site down.
	host := monitoring.Sample(a.cfg.SitesDir)
	var sqlSize int64
	if st, err := os.Stat(dbSQLPath); err == nil {
		sqlSize = st.Size()
	}
	headroom := uint64(zipSize+sqlSize) / 10
	if headroom < 256<<20 {
		headroom = 256 << 20
	}
	needFree := uint64(zipSize+sqlSize) + headroom
	if host.DiskTotal == 0 {
		abort("Could not determine the free disk space on the websites volume; refusing to start the import blind.")
		return
	}
	if host.DiskFree < needFree {
		abort("Not enough free disk space for this import (the upload plus working room exceeds what is free).")
		return
	}
	if host.MemTotal > 0 && host.MemAvail < 512<<20 {
		abort("Less than 512 MB of RAM is available; refusing to start an import.")
		return
	}

	dbName := websites.DBNameFor(siteID)
	if _, err := a.store.FindWebsiteByDBName(dbName); err == nil {
		abort("Database name collision; please retry the import.")
		return
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
	if dbOverridden {
		_ = a.store.AddEvent(job.ID, "info", "the separately uploaded database file was used instead of the backup set's own database part")
	}
	http.Redirect(w, r, "/websites/"+siteID+"?flash=Import started. Follow progress in Deployment Jobs.", http.StatusSeeOther)
}

// sourceHost extracts the host from a detected source site URL
// ("https://www.example.com/blog" -> "www.example.com"). It
// tolerates scheme-less values and ports.
func sourceHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Hostname()
	}
	s = strings.TrimPrefix(s, "//")
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return s
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
