package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Udhayaboopathi/hPanel-wp/internal/agentproto"
	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
	"github.com/Udhayaboopathi/hPanel-wp/internal/websites"
	"github.com/Udhayaboopathi/hPanel-wp/internal/wordpress"
	"github.com/Udhayaboopathi/hPanel-wp/internal/wpimport"
)

// importSite implements OpImportSite: the one-upload migration.
// The panel has already staged two artifacts inside the site's own
// directory (<siteDir>/import/): the wp-content ZIP (a WPvivid-style
// backup layout) and the normalised plain-text database dump
// (db.sql). The op then:
//
//  1. provisions the site exactly like a create (container, network,
//     database, least-privilege user, generated config incl. the
//     imported table prefix) - createSite is reused wholesale;
//  2. replaces wp-content with the uploaded tree (source-server
//     scratch directories are skipped by the extractor);
//  3. empties the fresh database and imports the dump through a
//     one-shot MariaDB client container, like restores do;
//  4. rewires WordPress with WP-CLI: domain search-replace when the
//     domain changed, siteurl/home, the operator's admin account,
//     permalink + cache flush.
//
// Every step after provisioning is safe to re-run: a retried job
// re-creates idempotently, re-replaces the files and re-imports.
func (a *Agent) importSite(ctx context.Context, req agentproto.Request) (*agentproto.Response, error) {
	s := req.Site
	resp := &agentproto.Response{}
	add := func(p int, msg string) {
		resp.Events = append(resp.Events, agentproto.Event{Progress: p, Message: msg, Level: "info"})
	}
	warn := func(p int, msg string) {
		resp.Events = append(resp.Events, agentproto.Event{Progress: p, Message: msg, Level: "warning"})
	}
	if err := validateSiteBasic(s); err != nil {
		return resp, err
	}
	if req.ImportZipPath == "" || req.ImportDBPath == "" {
		return resp, fmt.Errorf("import request is missing its staged artifact paths")
	}
	siteDir, err := a.siteDirFor(s)
	if err != nil {
		return resp, err
	}
	importDir := filepath.Join(siteDir, "import")
	for _, p := range []string{req.ImportZipPath, req.ImportDBPath} {
		clean := filepath.Clean(p)
		if clean != importDir && !strings.HasPrefix(clean, importDir+string(filepath.Separator)) {
			return resp, fmt.Errorf("import artifact %q is outside the site's import staging directory", p)
		}
		if _, err := os.Stat(clean); err != nil {
			return resp, fmt.Errorf("import artifact %q is not readable: %w", p, err)
		}
	}

	// 1. Provision (identical to a create, including the throwaway
	// default install whose tables step 3 replaces wholesale).
	createReq := req
	createReq.Op = agentproto.OpCreateSite
	createResp, createErr := a.createSite(ctx, createReq)
	for _, ev := range createResp.Events {
		p := 5 + ev.Progress*45/100
		resp.Events = append(resp.Events, agentproto.Event{Progress: p, Message: ev.Message, Level: ev.Level})
	}
	if createErr != nil {
		return resp, createErr
	}
	if !createResp.OK {
		return resp, fmt.Errorf("provisioning step of the import did not complete")
	}
	resp.ContainerID = createResp.ContainerID
	resp.DNSPending = createResp.DNSPending
	resp.UsedCommonDBUser = createResp.UsedCommonDBUser
	effDBUser, effDBPassword := s.DBUser, s.DBPassword
	if createResp.UsedCommonDBUser {
		effDBUser, effDBPassword = a.db.AdminUser, a.db.AdminPassword
	}

	// 2. Files: extract the wp-content tree and swap it in.
	extractDir := filepath.Join(importDir, "extracted")
	stats, err := wpimport.ExtractContent(req.ImportZipPath, extractDir)
	if err != nil {
		return resp, fmt.Errorf("extracting uploaded files: %w", err)
	}
	add(58, fmt.Sprintf("uploaded files extracted: %d files (%d skipped as source-server scratch)", stats.Files, stats.Skipped))
	wpContent := filepath.Join(siteDir, "data", "wp-content")
	if err := os.MkdirAll(wpContent, 0o755); err != nil {
		return resp, err
	}
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return resp, err
	}
	for _, e := range entries {
		src := filepath.Join(extractDir, e.Name())
		dst := filepath.Join(wpContent, e.Name())
		if e.IsDir() {
			if err := os.RemoveAll(dst); err != nil {
				return resp, err
			}
		} else {
			_ = os.Remove(dst)
		}
		if err := os.Rename(src, dst); err != nil {
			return resp, fmt.Errorf("installing wp-content/%s: %w", e.Name(), err)
		}
	}
	// WordPress runs as www-data (33:33) and must be able to write
	// uploads/cache dirs; freshly extracted files are agent-owned.
	if err := chownTree(wpContent, 33, 33); err != nil {
		warn(68, "could not set www-data ownership on wp-content ("+trimOut(err.Error())+"); check the site's File Manager if uploads misbehave")
	}
	add(70, "wp-content installed (plugins, themes, uploads, mu-plugins)")

	// 3. Database: empty the fresh database, then import the dump.
	if err := security.ValidateDBName(s.DBName); err != nil {
		return resp, err
	}
	drop := []string{
		"SET FOREIGN_KEY_CHECKS=0",
		"SET SESSION group_concat_max_len = 1000000",
		"SET @t := (SELECT GROUP_CONCAT(CONCAT('`', table_name, '`')) FROM information_schema.tables WHERE table_schema = '" + s.DBName + "' AND table_type = 'BASE TABLE')",
		"SET @s := IF(@t IS NULL, 'SELECT 1', CONCAT('DROP TABLE IF EXISTS ', @t))",
		"PREPARE st FROM @s",
		"EXECUTE st",
		"DEALLOCATE PREPARE st",
		"SET FOREIGN_KEY_CHECKS=1",
	}
	if _, err := a.mariadbExec(ctx, drop); err != nil {
		return resp, fmt.Errorf("emptying the fresh database before import: %w", err)
	}
	add(76, "fresh database emptied; importing the uploaded dump")
	stage := filepath.Join(importDir, "stage")
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return resp, err
	}
	if err := copyFile(req.ImportDBPath, filepath.Join(stage, "db.sql")); err != nil {
		return resp, err
	}
	network := websites.NetworkName(s.SiteID)
	importCmd := fmt.Sprintf("mariadb -h %s -u %s %s < /stage/db.sql", a.db.InternalHost(), effDBUser, s.DBName)
	if _, err := a.runOneShot(ctx, s, "mariadb:11.4", []string{"sh", "-c", importCmd},
		[]string{"MYSQL_PWD=" + effDBPassword}, []string{stage + ":/stage:ro"}, network); err != nil {
		return resp, fmt.Errorf("database import failed: %w", err)
	}
	add(88, "database imported")

	// 4. WP-CLI rewiring.
	wpEnv := []string{
		"WORDPRESS_DB_HOST=" + a.db.InternalHost(), "WORDPRESS_DB_NAME=" + s.DBName,
		"WORDPRESS_DB_USER=" + effDBUser, "WORDPRESS_DB_PASSWORD=" + effDBPassword,
	}
	runWP := func(args []string) error {
		_, err := a.runOneShot(ctx, s, "wordpress:cli-2.12.0-php8.3", args, wpEnv, nil, network)
		return err
	}
	newURL := "https://" + s.Domain
	if req.ImportSourceURL != "" && req.ImportSourceURL != newURL {
		if err := runWP(wordpress.SearchReplaceArgs(req.ImportSourceURL, newURL)); err != nil {
			warn(92, "domain search-replace from "+req.ImportSourceURL+" to "+newURL+" failed ("+trimOut(err.Error())+"); fix it from the site's WordPress tab before going live")
		} else {
			add(92, "domain rewritten throughout the imported content: "+req.ImportSourceURL+" -> "+newURL)
		}
	}
	_ = runWP([]string{"wp", "option", "update", "siteurl", newURL, "--allow-root"})
	_ = runWP([]string{"wp", "option", "update", "home", newURL, "--allow-root"})
	if s.AdminUser != "" && s.AdminPassword != "" {
		createArgs := []string{"wp", "user", "create", s.AdminUser, s.AdminEmail,
			"--role=administrator", "--user_pass=" + s.AdminPassword, "--allow-root"}
		if err := runWP(createArgs); err != nil {
			updateArgs := []string{"wp", "user", "update", s.AdminUser,
				"--user_pass=" + s.AdminPassword, "--user_email=" + s.AdminEmail, "--allow-root"}
			roleArgs := []string{"wp", "user", "set-role", s.AdminUser, "administrator", "--allow-root"}
			if err2 := runWP(updateArgs); err2 != nil {
				warn(95, "could not create or update the administrator account ("+trimOut(err2.Error())+"); use the site's Reset admin password action after the import")
			} else {
				_ = runWP(roleArgs)
				add(95, "administrator account updated with the supplied credentials (it already existed in the imported database)")
			}
		} else {
			add(95, "administrator account created with the supplied credentials")
		}
	}
	_ = runWP([]string{"wp", "rewrite", "flush", "--allow-root"})
	_ = runWP([]string{"wp", "cache", "flush", "--allow-root"})
	add(97, "permalinks and caches flushed")

	// 5. Clean the staging directory (uploads can be gigabytes) and
	// report. On failure paths above, staging is deliberately left
	// in place so a retry does not need a re-upload... but the job
	// model re-runs from the panel, which re-stages; a leftover from
	// a FAILED attempt is removed by the next import or by deleting
	// the site.
	_ = os.RemoveAll(importDir)
	resp.OK = true
	add(100, "import completed: the migrated site is deployed")
	return resp, nil
}

// chownTree sets uid/gid on a directory tree (best effort per node:
// the first error is returned so the caller can warn).
func chownTree(root string, uid, gid int) error {
	var firstErr error
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if err := os.Chown(path, uid, gid); err != nil && firstErr == nil {
			firstErr = err
		}
		return nil
	})
	return firstErr
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
