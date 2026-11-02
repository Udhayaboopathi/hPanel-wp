// Package wpimport implements the analysis and extraction side of
// the one-upload WordPress migration feature: recognising a
// wp-content tree inside an uploaded ZIP (including WPvivid backup
// layouts), finding the embedded database dump, and reading the two
// facts a migration cannot proceed without - the source table
// prefix and the source site URL - out of a SQL dump.
package wpimport

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SkipTopLevel lists wp-content subdirectories that belong to the
// SOURCE server's tooling and must never be carried into a KR Host
// site: LiteSpeed's cache tree (the panel runs Apache), WPvivid's
// staging/upload scratch dirs, and WordPress's own upgrade scratch.
var SkipTopLevel = map[string]bool{
	"litespeed":           true,
	"wpvivid_staging":     true,
	"wpvivid_uploads":     true,
	"upgrade-temp-backup": true,
	"__MACOSX":            true,
}

// IsDBBackupName reports whether a file name looks like a database
// dump the importer can consume: a WPvivid dump (name ends in
// "-db", the plugin's convention, with or without a compression
// extension) or a plain/gzipped SQL export.
func IsDBBackupName(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if strings.HasSuffix(base, "-db") || strings.HasSuffix(base, "-db.gz") || strings.HasSuffix(base, "-db.zip") {
		return true
	}
	return strings.HasSuffix(base, ".sql") || strings.HasSuffix(base, ".sql.gz") || strings.HasSuffix(base, ".sql.gzip")
}

// IsGzipHeader reports the gzip magic bytes.
func IsGzipHeader(head []byte) bool {
	return len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b
}

// PartKind classifies one file of a WPvivid multi-part backup set
// by its name: WPvivid delivers a site as one archive per wp-content
// piece (…-plugins.zip, …-themes.zip, …-uploads.zip,
// …-mu-plugins.zip, …-others.zip) plus the database (…-db[.gz]).
// Returns "" for a name that is not a WPvivid part.
func PartKind(name string) string {
	base := strings.ToLower(filepath.Base(name))
	if IsDBBackupName(base) {
		return "db"
	}
	// mu-plugins must be checked before plugins: its suffix
	// contains the plugins suffix.
	for _, kind := range []string{"mu-plugins", "plugins", "themes", "uploads", "others"} {
		if strings.HasSuffix(base, "-"+kind+".zip") {
			return kind
		}
	}
	return ""
}

// MergePart is one classified WPvivid part archive on disk.
type MergePart struct {
	Kind string // plugins | themes | uploads | mu-plugins | others
	Path string
}

// MergeWPvividParts combines WPvivid part archives into a single
// ZIP whose entries form one wp-content tree (the shape
// FindContentRoot/ExtractContent consume). Part archives may store
// their folder at the top ("plugins/akismet/…") or bare
// ("akismet/…"); entries are re-rooted so both forms land under
// "<kind>/…". "others" entries are wp-content root items and keep
// their names. Entries are stored uncompressed: the parts are
// mostly media, and the merged file is a transport container that
// is extracted once and deleted.
func MergeWPvividParts(dstPath string, parts []MergePart) (int, error) {
	out, err := os.Create(dstPath)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(out)
	written := 0
	for _, part := range parts {
		zr, err := zip.OpenReader(part.Path)
		if err != nil {
			zw.Close()
			out.Close()
			return 0, fmt.Errorf("part %s is not a readable ZIP: %w", filepath.Base(part.Path), err)
		}
		for _, f := range zr.File {
			name := strings.TrimPrefix(f.Name, "./")
			if name == "" || strings.HasSuffix(name, "/") {
				continue
			}
			rel := name
			switch part.Kind {
			case "plugins", "themes", "uploads", "mu-plugins":
				if name != part.Kind && !strings.HasPrefix(name, part.Kind+"/") {
					rel = part.Kind + "/" + name
				}
			}
			hdr := &zip.FileHeader{Name: rel, Method: zip.Store}
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				zr.Close()
				zw.Close()
				out.Close()
				return 0, err
			}
			rc, err := f.Open()
			if err != nil {
				zr.Close()
				zw.Close()
				out.Close()
				return 0, err
			}
			_, err = io.Copy(w, rc)
			rc.Close()
			if err != nil {
				zr.Close()
				zw.Close()
				out.Close()
				return 0, err
			}
			written++
		}
		zr.Close()
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return written, nil
}

// FindContentRoot returns the ZIP-entry prefix ("" or "a/b/") of the
// wp-content directory inside an archive: the shallowest directory
// that contains both plugins/ and themes/. Uploads in the wild nest
// the folder (Drive downloads, "backup/backup/wp-content", or the
// wp-content dir zipped itself), so the root is discovered from the
// entry names, never assumed.
func FindContentRoot(names []string) (string, error) {
	type found struct{ plugins, themes bool }
	candidates := map[string]*found{}
	mark := func(prefix, top string) {
		f := candidates[prefix]
		if f == nil {
			f = &found{}
			candidates[prefix] = f
		}
		switch top {
		case "plugins":
			f.plugins = true
		case "themes":
			f.themes = true
		}
	}
	for _, n := range names {
		n = strings.TrimPrefix(n, "./")
		parts := strings.Split(n, "/")
		// An entry at <prefix>/plugins/... or <prefix>/themes/...
		// marks <prefix> as a candidate content root.
		for i := 0; i < len(parts); i++ {
			if parts[i] == "plugins" || parts[i] == "themes" {
				mark(strings.Join(parts[:i], "/"), parts[i])
			}
		}
	}
	best := ""
	bestLen := -1
	for prefix, f := range candidates {
		if f.plugins && f.themes {
			p := prefix
			if p != "" {
				p += "/"
			}
			if bestLen == -1 || len(p) < bestLen {
				best, bestLen = p, len(p)
			}
		}
	}
	if bestLen == -1 {
		return "", fmt.Errorf("no wp-content directory found in the archive (looked for a folder containing plugins/ and themes/)")
	}
	return best, nil
}

// FindDBEntry returns the ZIP entry that holds the database dump:
// the shallowest IsDBBackupName entry, preferring one inside the
// wp-content root (where WPvivid leaves it). "" when absent.
func FindDBEntry(names []string, contentRoot string) string {
	best := ""
	for _, n := range names {
		clean := strings.TrimPrefix(n, "./")
		if !IsDBBackupName(clean) {
			continue
		}
		if strings.HasSuffix(clean, "/") {
			continue
		}
		if best == "" {
			best = clean
			continue
		}
		// Prefer entries inside the content root, then shallower ones.
		inNew := strings.HasPrefix(clean, contentRoot)
		inBest := strings.HasPrefix(best, contentRoot)
		if inNew != inBest {
			if inNew {
				best = clean
			}
			continue
		}
		if len(strings.Split(clean, "/")) < len(strings.Split(best, "/")) {
			best = clean
		}
	}
	return best
}

var (
	prefixRe  = regexp.MustCompile("(?:CREATE TABLE|INSERT INTO) `(\\w{1,32})options`")
	siteURLRe = regexp.MustCompile(`'siteurl'\s*,\s*'(https?://[^']+)'`)
)

// DetectPrefixAndSiteURL streams a plain-text SQL dump and extracts
// the table prefix (from the options table's DDL/DML) and the
// siteurl option value. Either may be "" when absent. The scan is
// chunked with overlap because dump INSERT lines can be megabytes
// long; reading is capped so a hostile file cannot stream forever.
func DetectPrefixAndSiteURL(r io.Reader) (prefix, siteURL string, err error) {
	const chunk = 1 << 20
	const overlap = 4096
	const maxScan = 512 << 20
	buf := make([]byte, chunk+overlap)
	var carry []byte
	var total int64
	br := bufio.NewReaderSize(r, chunk)
	for total < maxScan {
		n, rerr := br.Read(buf[len(carry):])
		if n > 0 {
			total += int64(n)
			window := append(append([]byte{}, carry...), buf[len(carry):len(carry)+n]...)
			if prefix == "" {
				if m := prefixRe.FindSubmatch(window); m != nil {
					prefix = string(m[1])
				}
			}
			if siteURL == "" {
				if m := siteURLRe.FindSubmatch(window); m != nil {
					siteURL = string(m[1])
				}
			}
			if prefix != "" && siteURL != "" {
				return prefix, siteURL, nil
			}
			if len(window) > overlap {
				carry = append(carry[:0], window[len(window)-overlap:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return prefix, siteURL, rerr
		}
	}
	return prefix, siteURL, nil
}

// ExtractStats summarises an extraction.
type ExtractStats struct {
	Files   int
	Bytes   int64
	Skipped int
}

// MaxExtractedBytes mirrors the agent's backup-extraction guard.
const MaxExtractedBytes = 15 << 30

// ExtractContent unpacks the wp-content tree of the ZIP at zipPath
// into destDir (which becomes a mirror of wp-content): the content
// root is located with FindContentRoot, source-server scratch
// directories (SkipTopLevel), any database dump file and any stray
// wp-config.php are skipped, links are never followed, and every
// entry is confined to destDir.
func ExtractContent(zipPath, destDir string) (*ExtractStats, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	names := make([]string, 0, len(zr.File))
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	root, err := FindContentRoot(names)
	if err != nil {
		return nil, err
	}
	dbEntry := FindDBEntry(names, root)
	cleanDest, _ := filepath.Abs(destDir)
	if err := os.MkdirAll(cleanDest, 0o750); err != nil {
		return nil, err
	}
	stats := &ExtractStats{}
	for _, name := range names {
		if !strings.HasPrefix(name, root) {
			continue
		}
		rel := strings.TrimPrefix(name, root)
		if rel == "" {
			continue
		}
		top := strings.SplitN(rel, "/", 2)[0]
		if SkipTopLevel[top] || name == dbEntry || top == "wp-config.php" {
			stats.Skipped++
			continue
		}
		f := byName[name]
		if f.Mode()&os.ModeSymlink != 0 {
			stats.Skipped++
			continue
		}
		target := filepath.Join(cleanDest, filepath.FromSlash(rel))
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe archive entry %q", name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		stats.Bytes += int64(f.UncompressedSize64)
		if stats.Bytes > MaxExtractedBytes {
			return nil, fmt.Errorf("archive extracts to more than %d bytes; refusing (possible decompression bomb)", int64(MaxExtractedBytes))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		stats.Files++
	}
	return stats, nil
}
