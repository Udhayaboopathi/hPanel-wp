package backups

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxExtractBytes caps the total decompressed size of a backup
// archive during extraction (decompression-bomb guard).
const MaxExtractBytes = 15 << 30

type Metadata struct {
	SiteID    string `json:"site_id"`
	Domain    string `json:"domain"`
	DBName    string `json:"db_name"`
	CreatedAt string `json:"created_at"`
	FilesSize int64  `json:"files_size"`
	HasDBDump bool   `json:"has_db_dump"`
	WPVersion string `json:"wp_version"`
}

func Filename(siteID string, t time.Time) string {
	return fmt.Sprintf("%s-%s.tar.gz", siteID, t.UTC().Format("20060102-150405"))
}

// CreateTarGz streams srcDir into destPath (no full in-memory buffering) and
// returns sha256 + size. An optional dbDump file is added as db.sql.
func CreateTarGz(srcDir, dbDumpPath, destPath string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return "", 0, err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	mw := io.MultiWriter(f, h)
	gz, err := gzip.NewWriterLevel(mw, gzip.BestSpeed)
	if err != nil {
		return "", 0, err
	}
	tw := tar.NewWriter(gz)
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil || rel == "." {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // never follow symlinks out of the site dir
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = "files/" + filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		rc, err := os.Open(path)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(tw, rc)
		return err
	})
	if err == nil && dbDumpPath != "" {
		if st, serr := os.Stat(dbDumpPath); serr == nil {
			hdr, herr := tar.FileInfoHeader(st, "")
			if herr != nil {
				err = herr
			} else {
				hdr.Name = "db.sql"
				if err = tw.WriteHeader(hdr); err == nil {
					var rc *os.File
					rc, err = os.Open(dbDumpPath)
					if err == nil {
						_, err = io.Copy(tw, rc)
						rc.Close()
					}
				}
			}
		}
	}
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// Verify recomputes the sha256 of a backup artifact.
func Verify(path, expected string) (bool, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	return expected == "" || got == expected, got, nil
}

// ExtractTarGz restores a backup into destDir with traversal protection.
func ExtractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	cleanDest, _ := filepath.Abs(destDir)
	var extracted int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "files/")
		if name == "db.sql" || strings.HasPrefix(hdr.Name, "..") {
			if name == "db.sql" {
				continue // DB restore is a separate, explicit step
			}
			return fmt.Errorf("unsafe entry %q in archive", hdr.Name)
		}
		target := filepath.Join(cleanDest, name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			extracted += hdr.Size
			if extracted > MaxExtractBytes {
				return fmt.Errorf("archive extracts to more than %d bytes; refusing (possible decompression bomb)", MaxExtractBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			// Skip links/devices entirely.
		}
	}
}

// Retention: keep the newest `keep` verified backups; never delete the last
// confirmed working backup (status=ready) even if keep==0.
func RetentionCandidates(backups []BackupLike, keep int) []BackupLike {
	var ready []BackupLike
	for _, b := range backups {
		if b.Status == "ready" {
			ready = append(ready, b)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].CreatedAt > ready[j].CreatedAt })
	if keep < 1 {
		keep = 1
	}
	if len(ready) <= keep {
		return nil
	}
	return ready[keep:]
}

type BackupLike struct {
	ID        int64
	Status    string
	CreatedAt string
	Path      string
}
