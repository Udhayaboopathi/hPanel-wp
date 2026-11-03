package store

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

//go:embed migrations/001_init.sql
var migration001 string

//go:embed migrations/001_init_mysql.sql
var migration001MySQL string

func Now() string { return time.Now().UTC().Format(time.RFC3339) }

func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

type Store struct {
	DB     *sql.DB
	driver string // "sqlite" or "mysql"
}

// Driver returns the backend in use ("sqlite" dev default, "mysql" in
// production where the panel metadata lives in the dedicated MariaDB
// container of the hPanel stack).
func (s *Store) Driver() string { return s.driver }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(migration001); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return finishOpen(db, "sqlite")
}

// OpenMySQL opens the panel store on a MariaDB/MySQL server (in production:
// the dedicated wordpress-mariadb container of the hPanel stack, reached
// internally by container name on port 3306). The database itself must
// already exist - the MariaDB entrypoint creates it on first init
// (MARIADB_DATABASE), and the deploy workflow additionally ensures it with
// CREATE DATABASE IF NOT EXISTS via root exec.
func OpenMySQL(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	for _, stmt := range SplitStatements(migration001MySQL) {
		if _, err := db.Exec(stmt); err != nil {
			if isDuplicateObjectErr(err) {
				continue // table/index already exists from a previous boot
			}
			return nil, fmt.Errorf("migrations (mysql): %w", err)
		}
	}
	return finishOpen(db, "mysql")
}

// ensureTablePrefixColumn applies migration 002 (websites.table_prefix,
// the WordPress table prefix of imported sites) when the column is
// not there yet. It probes first instead of relying on duplicate-
// column error dialects, so it is idempotent on SQLite and MySQL.
func ensureTablePrefixColumn(db *sql.DB, driver string) error {
	var n int
	var err error
	if driver == "mysql" {
		err = db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='websites' AND column_name='table_prefix'`).Scan(&n)
	} else {
		err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name='table_prefix'`).Scan(&n)
	}
	if err != nil {
		return fmt.Errorf("checking table_prefix column: %w", err)
	}
	if n > 0 {
		return nil
	}
	ddl := `ALTER TABLE websites ADD COLUMN table_prefix TEXT NOT NULL DEFAULT 'wp_'`
	if driver == "mysql" {
		ddl = "ALTER TABLE websites ADD COLUMN table_prefix VARCHAR(32) NOT NULL DEFAULT 'wp_'"
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migration 002 (table_prefix): %w", err)
	}
	return nil
}

// ensureTOTPColumn applies migration 003 (administrators.
// totp_last_step, the replay-protection counter for TOTP codes)
// with the same probe-first idempotency as migration 002.
func ensureTOTPColumn(db *sql.DB, driver string) error {
	var n int
	var err error
	if driver == "mysql" {
		err = db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='administrators' AND column_name='totp_last_step'`).Scan(&n)
	} else {
		err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('administrators') WHERE name='totp_last_step'`).Scan(&n)
	}
	if err != nil {
		return fmt.Errorf("checking totp_last_step column: %w", err)
	}
	if n > 0 {
		return nil
	}
	ddl := `ALTER TABLE administrators ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0`
	if driver == "mysql" {
		ddl = "ALTER TABLE administrators ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0"
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migration 003 (totp_last_step): %w", err)
	}
	return nil
}

func finishOpen(db *sql.DB, driver string) (*Store, error) {
	if err := ensureTablePrefixColumn(db, driver); err != nil {
		return nil, err
	}
	if err := ensureTOTPColumn(db, driver); err != nil {
		return nil, err
	}
	s := &Store{DB: db, driver: driver}
	// Crash recovery. Execution is driven by in-process goroutines
	// (and in-memory retry timers), so after a restart NOTHING will
	// ever run jobs left in 'queued' or 'running': both are honestly
	// 'interrupted' - the operator can re-run them from the job page.
	if _, err := db.Exec(`UPDATE jobs SET status='interrupted', error=COALESCE(NULLIF(error,''),'panel restarted during job'), updated_at=? WHERE status IN ('running','queued')`, Now()); err != nil {
		return nil, err
	}
	// Websites left in transient states by the dead process would
	// otherwise sit in 'provisioning'/'deleting' forever: no job is
	// active for them anymore. 'failed' is the honest state - the
	// site's data is untouched and Repair/delete can proceed.
	if _, err := db.Exec(`UPDATE websites SET status='failed' WHERE status IN ('provisioning','deleting')`); err != nil {
		return nil, err
	}
	// Backup records whose reconciler goroutine died with the old
	// process would sit in 'creating' forever.
	if _, err := db.Exec(`UPDATE backups SET status='failed' WHERE status='creating'`); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenDriver selects the store backend: "sqlite" with a file path, or
// "mysql" with a full DSN (see BuildMySQLDSN).
func OpenDriver(driver, target string) (*Store, error) {
	switch driver {
	case "", "sqlite":
		return Open(target)
	case "mysql":
		return OpenMySQL(target)
	default:
		return nil, fmt.Errorf("unknown PANEL_STORE_DRIVER %q (want sqlite or mysql)", driver)
	}
}

// BuildMySQLDSN builds a go-sql-driver/mysql DSN from parts. If the
// password contains characters that are ambiguous in a DSN (notably '@',
// '/' or '?'), supply a complete PANEL_STORE_DSN instead.
func BuildMySQLDSN(host string, port int, dbName, user, password string) string {
	// Deliberately NO multiStatements: migrations are executed
	// statement-by-statement via SplitStatements, so the runtime
	// connection never needs stacked queries — and without the
	// flag, any future injection anywhere in the panel cannot
	// escalate into a stacked-query injection.
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=false&timeout=10s&readTimeout=30s&writeTimeout=30s",
		user, password, host, port, dbName)
}

// SplitStatements splits a migration file into individual statements. The
// migration files contain no semicolons inside string literals, so a plain
// split is safe here (and lint-tested).
func SplitStatements(sqlText string) []string {
	var out []string
	for _, part := range strings.Split(sqlText, ";") {
		// Strip leading comment lines so they don't become empty statements.
		var lines []string
		for _, ln := range strings.Split(part, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "--") {
				continue
			}
			lines = append(lines, ln)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func isDuplicateObjectErr(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1050, // table already exists
			1060, // duplicate column
			1061: // duplicate key name (index)
			return true
		}
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "Duplicate key name")
}

func (s *Store) Close() error { return s.DB.Close() }

// ---- Administrators ----

type Admin struct {
	ID           int64
	Username     string
	Email        string
	PasswordHash string
	TOTPSecret   string
	TOTPEnabled  bool
	CreatedAt    string
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM administrators`).Scan(&n)
	return n, err
}

func (s *Store) CreateAdmin(username, email, hash, totpSecret string, totpEnabled bool) (int64, error) {
	now := Now()
	res, err := s.DB.Exec(`INSERT INTO administrators(username,email,password_hash,totp_secret,totp_enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		username, email, hash, totpSecret, boolInt(totpEnabled), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanAdmin(row interface{ Scan(...any) error }) (*Admin, error) {
	var a Admin
	var en int
	if err := row.Scan(&a.ID, &a.Username, &a.Email, &a.PasswordHash, &a.TOTPSecret, &en, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.TOTPEnabled = en == 1
	return &a, nil
}

const adminCols = `id,username,email,password_hash,totp_secret,totp_enabled,created_at`

func (s *Store) GetAdminByUsername(u string) (*Admin, error) {
	return scanAdmin(s.DB.QueryRow(`SELECT `+adminCols+` FROM administrators WHERE username=?`, u))
}

// GetAdminByLogin resolves a login identifier that may be the
// account's username OR its email address. Operators naturally
// type the email they registered with; accepting only the exact
// username locked out a valid admin with a misleading
// "invalid credentials" error (production report, 2026-11-02).
func (s *Store) GetAdminByLogin(login string) (*Admin, error) {
	// An exact username match wins; email is only a fallback. A
	// single OR query could return the WRONG account when one
	// admin's username equals another admin's email, and the
	// password would then be checked against that other account.
	if a, err := s.GetAdminByUsername(login); err == nil {
		return a, nil
	}
	return scanAdmin(s.DB.QueryRow(`SELECT `+adminCols+` FROM administrators WHERE email=?`, login))
}

// ConsumeTOTPStep atomically records a TOTP time-step as used for
// the admin and reports whether it was fresh. TOTP codes stay
// valid for their whole window (~90 s with skew); without this,
// one observed code can be replayed in another session until it
// expires. The conditional UPDATE makes check-and-consume a
// single atomic step on both store backends.
func (s *Store) ConsumeTOTPStep(adminID, step int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE administrators SET totp_last_step=? WHERE id=? AND totp_last_step<?`, step, adminID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func (s *Store) GetAdminByID(id int64) (*Admin, error) {
	return scanAdmin(s.DB.QueryRow(`SELECT `+adminCols+` FROM administrators WHERE id=?`, id))
}
func (s *Store) UpdateAdminPassword(id int64, hash string) error {
	_, err := s.DB.Exec(`UPDATE administrators SET password_hash=?, updated_at=? WHERE id=?`, hash, Now(), id)
	return err
}
func (s *Store) UpdateAdminTOTP(id int64, secret string, enabled bool) error {
	_, err := s.DB.Exec(`UPDATE administrators SET totp_secret=?, totp_enabled=?, updated_at=? WHERE id=?`, secret, boolInt(enabled), Now(), id)
	return err
}

func (s *Store) ReplaceRecoveryCodes(adminID int64, hashes []string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE admin_id=?`, adminID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes(admin_id,code_hash) VALUES(?,?)`, adminID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ConsumeRecoveryCode(adminID int64, hash string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE recovery_codes SET used_at=? WHERE admin_id=? AND code_hash=? AND used_at IS NULL`, Now(), adminID, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---- Sessions ----

type Session struct {
	TokenHash string
	AdminID   int64
	CSRFToken string
	IP        string
	ExpiresAt string
}

func (s *Store) CreateSession(tokenHash string, adminID int64, csrf, ip, ua string, expires time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions(token_hash,admin_id,csrf_token,ip,user_agent,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		tokenHash, adminID, csrf, ip, ua, Now(), expires.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetSession(tokenHash string) (*Session, error) {
	var sess Session
	err := s.DB.QueryRow(`SELECT token_hash,admin_id,csrf_token,ip,expires_at FROM sessions WHERE token_hash=? AND expires_at>?`, tokenHash, Now()).
		Scan(&sess.TokenHash, &sess.AdminID, &sess.CSRFToken, &sess.IP, &sess.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}
func (s *Store) DeleteAllSessions(adminID int64) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE admin_id=?`, adminID)
	return err
}
func (s *Store) DeleteExpiredSessions() error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at<=?`, Now())
	return err
}

// ---- Login attempts / rate limiting ----

func (s *Store) RecordLoginAttempt(username, ip string, success bool) error {
	_, err := s.DB.Exec(`INSERT INTO login_attempts(username,ip,success,created_at) VALUES(?,?,?,?)`, username, ip, boolInt(success), Now())
	return err
}

func (s *Store) RecentFailedLogins(username, ip string, window time.Duration) (int, error) {
	since := time.Now().UTC().Add(-window).Format(time.RFC3339)
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE success=0 AND created_at>? AND (username=? OR ip=?)`, since, username, ip).Scan(&n)
	return n, err
}

// ---- Websites ----

type Website struct {
	ID              int64
	SiteID          string
	Name            string
	Domain          string
	AdminDomain     string
	Title           string
	Status          string
	PHPVersion      string
	WPVersion       string
	ResourceProfile string
	CPULimit        float64
	MemLimitMB      int
	ContainerName   string
	ContainerID     string
	DBName          string
	DBUser          string
	DBPasswordEnc   string
	TablePrefix     string
	AdminUsername   string
	AdminEmail      string
	SiteDir         string
	CreatedAt       string
	UpdatedAt       string
}

const siteCols = `id,site_id,name,domain,admin_domain,title,status,php_version,wp_version,resource_profile,cpu_limit,mem_limit_mb,container_name,container_id,db_name,db_user,db_password_enc,table_prefix,admin_username,admin_email,site_dir,created_at,updated_at`

func scanSite(row interface{ Scan(...any) error }) (*Website, error) {
	var w Website
	err := row.Scan(&w.ID, &w.SiteID, &w.Name, &w.Domain, &w.AdminDomain, &w.Title, &w.Status, &w.PHPVersion, &w.WPVersion, &w.ResourceProfile, &w.CPULimit, &w.MemLimitMB, &w.ContainerName, &w.ContainerID, &w.DBName, &w.DBUser, &w.DBPasswordEnc, &w.TablePrefix, &w.AdminUsername, &w.AdminEmail, &w.SiteDir, &w.CreatedAt, &w.UpdatedAt)
	return &w, err
}

func (s *Store) CreateWebsite(w *Website) (int64, error) {
	now := Now()
	w.CreatedAt, w.UpdatedAt = now, now
	if w.TablePrefix == "" {
		w.TablePrefix = "wp_"
	}
	// All four inserts are ONE transaction: previously a failure
	// after the websites insert left a live row whose domain was
	// burned by the UNIQUE constraint while its domain/database
	// records were missing.
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO websites(site_id,name,domain,admin_domain,title,status,php_version,wp_version,resource_profile,cpu_limit,mem_limit_mb,container_name,container_id,db_name,db_user,db_password_enc,table_prefix,admin_username,admin_email,site_dir,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.SiteID, w.Name, w.Domain, w.AdminDomain, w.Title, w.Status, w.PHPVersion, w.WPVersion, w.ResourceProfile, w.CPULimit, w.MemLimitMB, w.ContainerName, w.ContainerID, w.DBName, w.DBUser, w.DBPasswordEnc, w.TablePrefix, w.AdminUsername, w.AdminEmail, w.SiteDir, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	w.ID = id
	// The domain rows are part of the website's identity: their
	// insert errors are NOT ignored (a swallowed collision here once
	// let two sites publish Traefik routers for the same host - the
	// panel validates collisions before this point, and any remaining
	// failure must surface instead of corrupting state quietly).
	if _, err := tx.Exec(`INSERT INTO website_domains(website_id,domain,kind,created_at) VALUES(?,?, 'primary', ?)`, id, w.Domain, now); err != nil {
		return 0, fmt.Errorf("recording primary domain: %w", err)
	}
	if w.AdminDomain != "" {
		if _, err := tx.Exec(`INSERT INTO website_domains(website_id,domain,kind,created_at) VALUES(?,?, 'admin', ?)`, id, w.AdminDomain, now); err != nil {
			return 0, fmt.Errorf("recording admin domain: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO website_databases(website_id,db_name,db_user) VALUES(?,?,?)`, id, w.DBName, w.DBUser); err != nil {
		return 0, fmt.Errorf("recording website database: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) GetWebsiteBySiteID(siteID string) (*Website, error) {
	return scanSite(s.DB.QueryRow(`SELECT `+siteCols+` FROM websites WHERE site_id=?`, siteID))
}
func (s *Store) GetWebsiteByDomain(d string) (*Website, error) {
	return scanSite(s.DB.QueryRow(`SELECT `+siteCols+` FROM websites WHERE domain=?`, d))
}

// ListWebsitesOrdered is ListWebsites with the sort applied in
// SQL, BEFORE pagination. (Sorting in the handler after LIMIT —
// the old approach — ordered only the current page, so page 2 of
// a domain sort was not actually sorted.) orderBy is whitelisted,
// never interpolated from request input.
func (s *Store) ListWebsitesOrdered(search, status string, limit, offset int, orderBy string) ([]*Website, int, error) {
	return s.listWebsites(search, status, limit, offset, orderBy)
}

func (s *Store) ListWebsites(search, status string, limit, offset int) ([]*Website, int, error) {
	return s.listWebsites(search, status, limit, offset, "")
}

func (s *Store) listWebsites(search, status string, limit, offset int, orderBy string) ([]*Website, int, error) {
	q := `SELECT ` + siteCols + ` FROM websites WHERE 1=1`
	cq := `SELECT COUNT(*) FROM websites WHERE 1=1`
	var args []any
	if search != "" {
		// Escape LIKE wildcards in the operator's text: a bare %
		// or _ must match literally, not act as a pattern.
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
		q += ` AND (domain LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR site_id LIKE ? ESCAPE '\')`
		cq += ` AND (domain LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR site_id LIKE ? ESCAPE '\')`
		like := "%" + esc + "%"
		args = append(args, like, like, like)
	}
	if status != "" && status != "all" {
		q += ` AND status=?`
		cq += ` AND status=?`
		args = append(args, status)
	}
	var total int
	if err := s.DB.QueryRow(cq, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if orderBy == "domain" {
		q += ` ORDER BY domain ASC, id DESC LIMIT ? OFFSET ?`
	} else {
		q += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	}
	rows, err := s.DB.Query(q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Website
	for rows.Next() {
		w, err := scanSite(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, w)
	}
	return out, total, rows.Err()
}

func (s *Store) CountWebsitesByStatus() (map[string]int, int, error) {
	rows, err := s.DB.Query(`SELECT status, COUNT(*) FROM websites GROUP BY status`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	m := map[string]int{}
	total := 0
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, 0, err
		}
		m[st] = n
		total += n
	}
	return m, total, rows.Err()
}

func (s *Store) UpdateWebsite(siteID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	var sets []string
	var args []any
	for k, v := range fields {
		switch k {
		case "status", "wp_version", "container_id", "container_name", "title", "domain", "admin_domain", "php_version", "resource_profile", "cpu_limit", "mem_limit_mb", "db_password_enc", "db_user", "db_name", "table_prefix":
			sets = append(sets, k+"=?")
			args = append(args, v)
		default:
			return fmt.Errorf("field %q is not updatable", k)
		}
	}
	sets = append(sets, "updated_at=?")
	args = append(args, Now(), siteID)
	_, err := s.DB.Exec(`UPDATE websites SET `+strings.Join(sets, ",")+` WHERE site_id=?`, args...)
	return err
}

func (s *Store) DeleteWebsite(siteID string) error {
	w, err := s.GetWebsiteBySiteID(siteID)
	if err != nil {
		return err
	}
	// Child rows are deleted EXPLICITLY, in one transaction. The
	// schema declares ON DELETE CASCADE, but on MariaDB/MySQL the
	// foreign keys were written as column-level REFERENCES, which
	// those servers parse and IGNORE — relying on the cascade left
	// orphaned rows behind, including the website_domains row whose
	// UNIQUE constraint then burned the deleted site's domain
	// forever. History tables (jobs, backups, deployments) keep
	// their rows with website_id nulled, mirroring ON DELETE SET
	// NULL, so the audit trail survives the site.
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM website_domains WHERE website_id=?`,
		`DELETE FROM website_configurations WHERE website_id=?`,
		`DELETE FROM website_databases WHERE website_id=?`,
		`DELETE FROM ssl_checks WHERE website_id=?`,
		`UPDATE deployments SET website_id=NULL WHERE website_id=?`,
		`UPDATE jobs SET website_id=NULL WHERE website_id=?`,
		`UPDATE backups SET website_id=NULL WHERE website_id=?`,
	} {
		if _, err := tx.Exec(stmt, w.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM websites WHERE id=?`, w.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangeWebsiteDomain moves a site to a new primary domain,
// keeping the website_domains registry in step with the websites
// row in one transaction. (Change-domain previously updated only
// the websites row: the registry kept the OLD domain reserved and
// never recorded the new one.)
func (s *Store) ChangeWebsiteDomain(siteID, newDomain, newAdminDomain string) error {
	w, err := s.GetWebsiteBySiteID(siteID)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE websites SET domain=?, admin_domain=?, updated_at=? WHERE id=?`, newDomain, newAdminDomain, Now(), w.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM website_domains WHERE website_id=?`, w.ID); err != nil {
		return err
	}
	now := Now()
	if _, err := tx.Exec(`INSERT INTO website_domains(website_id,domain,kind,created_at) VALUES(?,?, 'primary', ?)`, w.ID, newDomain, now); err != nil {
		return fmt.Errorf("recording primary domain: %w", err)
	}
	if newAdminDomain != "" {
		if _, err := tx.Exec(`INSERT INTO website_domains(website_id,domain,kind,created_at) VALUES(?,?, 'admin', ?)`, w.ID, newAdminDomain, now); err != nil {
			return fmt.Errorf("recording admin domain: %w", err)
		}
	}
	return tx.Commit()
}

// FindWebsiteByDomainOrAdmin returns the site that uses the given
// name as its primary OR admin domain, if any. Collision checks
// must consider both directions: a new primary domain may not
// equal another site's admin alias, and vice versa.
func (s *Store) FindWebsiteByDomainOrAdmin(d string) (*Website, error) {
	return scanSite(s.DB.QueryRow(`SELECT `+siteCols+` FROM websites WHERE domain=? OR admin_domain=?`, d, d))
}

// FindWebsiteByDBName returns the site owning a database name, if
// any (an indexed lookup, replacing first-N-rows scans).
func (s *Store) FindWebsiteByDBName(dbName string) (*Website, error) {
	return scanSite(s.DB.QueryRow(`SELECT `+siteCols+` FROM websites WHERE db_name=?`, dbName))
}

func (s *Store) UpdateDBSize(siteID string, size int64) error {
	_, err := s.DB.Exec(`UPDATE website_databases SET size_bytes=?, last_checked_at=? WHERE website_id=(SELECT id FROM websites WHERE site_id=?)`, size, Now(), siteID)
	return err
}
func (s *Store) DBSize(siteID string) int64 {
	var n int64
	_ = s.DB.QueryRow(`SELECT size_bytes FROM website_databases WHERE website_id=(SELECT id FROM websites WHERE site_id=?)`, siteID).Scan(&n)
	return n
}

// ---- Config versions ----

func (s *Store) SaveConfigVersion(websiteID int64, envJSON, composeYAML, note string) (int, error) {
	// MAX(version)+1 and the insert are one transaction: outside
	// one, two concurrent saves compute the same next version and
	// one dies on the UNIQUE(website_id, version) constraint.
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM website_configurations WHERE website_id=?`, websiteID).Scan(&v); err != nil {
		return 0, err
	}
	v++
	if _, err := tx.Exec(`INSERT INTO website_configurations(website_id,version,env_json,compose_yaml,note,created_at) VALUES(?,?,?,?,?,?)`, websiteID, v, envJSON, composeYAML, note, Now()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return v, nil
}

type ConfigVersion struct {
	ID          int64
	Version     int
	EnvJSON     string
	ComposeYAML string
	Note        string
	CreatedAt   string
}

func (s *Store) ListConfigVersions(websiteID int64) ([]*ConfigVersion, error) {
	rows, err := s.DB.Query(`SELECT id,version,env_json,compose_yaml,note,created_at FROM website_configurations WHERE website_id=? ORDER BY version DESC`, websiteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ConfigVersion
	for rows.Next() {
		var c ConfigVersion
		if err := rows.Scan(&c.ID, &c.Version, &c.EnvJSON, &c.ComposeYAML, &c.Note, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *Store) LatestConfig(websiteID int64) (*ConfigVersion, error) {
	var c ConfigVersion
	err := s.DB.QueryRow(`SELECT id,version,env_json,compose_yaml,note,created_at FROM website_configurations WHERE website_id=? ORDER BY version DESC LIMIT 1`, websiteID).
		Scan(&c.ID, &c.Version, &c.EnvJSON, &c.ComposeYAML, &c.Note, &c.CreatedAt)
	return &c, err
}

// ---- Jobs ----

type Job struct {
	ID          string
	WebsiteID   sql.NullInt64
	SiteID      string
	Kind        string
	Status      string
	Progress    int
	Message     string
	Error       string
	Attempts    int
	MaxAttempts int
	PayloadJSON string
	CreatedAt   string
	UpdatedAt   string
}

func (s *Store) CreateJob(j *Job) error {
	if j.ID == "" {
		j.ID = NewID("job_")
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	j.CreatedAt, j.UpdatedAt = Now(), Now()
	if j.Status == "" {
		j.Status = "queued"
	}
	_, err := s.DB.Exec(`INSERT INTO jobs(id,website_id,site_id,kind,status,progress,message,error,attempts,max_attempts,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.WebsiteID, j.SiteID, j.Kind, j.Status, j.Progress, j.Message, j.Error, j.Attempts, j.MaxAttempts, j.PayloadJSON, j.CreatedAt, j.UpdatedAt)
	return err
}

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.WebsiteID, &j.SiteID, &j.Kind, &j.Status, &j.Progress, &j.Message, &j.Error, &j.Attempts, &j.MaxAttempts, &j.PayloadJSON, &j.CreatedAt, &j.UpdatedAt)
	return &j, err
}

const jobCols = `id,website_id,site_id,kind,status,progress,message,error,attempts,max_attempts,payload_json,created_at,updated_at`

func (s *Store) GetJob(id string) (*Job, error) {
	return scanJob(s.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) ListJobs(limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.Query(`SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ListJobsForSite(siteID string) ([]*Job, error) {
	rows, err := s.DB.Query(`SELECT `+jobCols+` FROM jobs WHERE site_id=? ORDER BY created_at DESC LIMIT 50`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) UpdateJob(id, status string, progress int, message, errText string) error {
	now := Now()
	_, err := s.DB.Exec(`UPDATE jobs SET status=?, progress=?, message=?, error=?, updated_at=?,
		started_at=CASE WHEN ?='running' AND started_at IS NULL THEN ? ELSE started_at END,
		finished_at=CASE WHEN ? IN ('succeeded','failed','cancelled') THEN ? ELSE finished_at END
		WHERE id=?`, status, progress, message, errText, now, status, now, status, now, id)
	return err
}

// ClaimJob atomically moves a job from 'queued' to 'running' and
// reports whether the caller won the claim. This is the
// compare-and-swap that makes operator cancel race-safe: a cancel
// that lands before the claim flips the row to 'cancelled', the
// claim then matches nothing, and the executor never starts —
// previously the executor's unconditional start-write overwrote
// the cancellation and the operation ran anyway.
func (s *Store) ClaimJob(id string) (bool, error) {
	now := Now()
	res, err := s.DB.Exec(`UPDATE jobs SET status='running', progress=5, message='provisioning started', updated_at=?, started_at=COALESCE(started_at, ?) WHERE id=? AND status='queued'`, now, now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) BumpJobAttempts(id string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET attempts=attempts+1, updated_at=? WHERE id=?`, Now(), id)
	return err
}

// HasActiveJobForSite prevents duplicate concurrent creation for one site.
// PruneHistory bounds the growth of append-only history tables
// (SSL checks, login attempts, job events, finished jobs) that
// otherwise grow forever on the production database.
func (s *Store) PruneHistory(days int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	for _, q := range []string{
		`DELETE FROM ssl_checks WHERE created_at < ?`,
		`DELETE FROM login_attempts WHERE created_at < ?`,
		`DELETE FROM deployment_events WHERE created_at < ?`,
		`DELETE FROM jobs WHERE status IN ('succeeded','failed','cancelled','interrupted') AND created_at < ?`,
	} {
		if _, err := s.DB.Exec(q, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// HasActiveJobForSite reports whether a job is active for the site.
// An empty kind matches ANY kind: lifecycle operations for one site
// must never run concurrently (a restart racing a delete against the
// same container is how resources get corrupted), so the runner
// enforces one active job per site across kinds.
func (s *Store) HasActiveJobForSite(siteID, kind string) (bool, error) {
	var n int
	var err error
	if kind == "" {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site_id=? AND status IN ('queued','running')`, siteID).Scan(&n)
	} else {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site_id=? AND kind=? AND status IN ('queued','running')`, siteID, kind).Scan(&n)
	}
	return n > 0, err
}

func (s *Store) AddEvent(jobID, level, message string) error {
	message = Redact(message) // job events must never carry secrets either
	_, err := s.DB.Exec(`INSERT INTO deployment_events(job_id,level,message,created_at) VALUES(?,?,?,?)`, jobID, level, message, Now())
	if err != nil {
		return err
	}
	_, _ = s.DB.Exec(`INSERT INTO deployments(website_id,job_id,action,status,detail,created_at)
		SELECT website_id, ?, kind, status, ?, ? FROM jobs WHERE id=?`, jobID, message, Now(), jobID)
	return nil
}

type Event struct {
	ID        int64
	JobID     string
	Level     string
	Message   string
	CreatedAt string
}

func (s *Store) ListEvents(jobID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.DB.Query(`SELECT id,job_id,level,message,created_at FROM deployment_events WHERE job_id=? ORDER BY id ASC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.JobID, &e.Level, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Store) RecentEvents(limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := s.DB.Query(`SELECT id,job_id,level,message,created_at FROM deployment_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.JobID, &e.Level, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---- Backups ----

type Backup struct {
	ID         int64
	WebsiteID  sql.NullInt64
	SiteID     string
	Filename   string
	Path       string
	SizeBytes  int64
	SHA256     string
	Status     string
	Storage    string
	CreatedAt  string
	VerifiedAt sql.NullString
}

func (s *Store) CreateBackup(b *Backup) (int64, error) {
	b.CreatedAt = Now()
	if b.Status == "" {
		b.Status = "creating"
	}
	res, err := s.DB.Exec(`INSERT INTO backups(website_id,site_id,filename,path,size_bytes,sha256,status,storage,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		b.WebsiteID, b.SiteID, b.Filename, b.Path, b.SizeBytes, b.SHA256, b.Status, b.Storage, b.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CompleteBackup records the AUTHORITATIVE artifact facts the agent
// returned for a finished backup job (exact path, checksum, size) -
// the panel must never have to re-derive them by guessing from a
// directory listing.
func (s *Store) CompleteBackup(id int64, filename, path, sha string, size int64) error {
	_, err := s.DB.Exec(`UPDATE backups SET filename=?, path=?, status='ready', sha256=?, size_bytes=?, verified_at=? WHERE id=?`,
		filename, path, sha, size, Now(), id)
	return err
}

func (s *Store) UpdateBackup(id int64, status, sha string, size int64, verified bool) error {
	if verified {
		_, err := s.DB.Exec(`UPDATE backups SET status=?, sha256=?, size_bytes=?, verified_at=? WHERE id=?`, status, sha, size, Now(), id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE backups SET status=?, sha256=?, size_bytes=? WHERE id=?`, status, sha, size, id)
	return err
}

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	err := row.Scan(&b.ID, &b.WebsiteID, &b.SiteID, &b.Filename, &b.Path, &b.SizeBytes, &b.SHA256, &b.Status, &b.Storage, &b.CreatedAt, &b.VerifiedAt)
	return &b, err
}

const backupCols = `id,website_id,site_id,filename,path,size_bytes,sha256,status,storage,created_at,verified_at`

func (s *Store) GetBackup(id int64) (*Backup, error) {
	return scanBackup(s.DB.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id=?`, id))
}

func (s *Store) ListBackups(siteID string, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if siteID == "" {
		rows, err = s.DB.Query(`SELECT `+backupCols+` FROM backups ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.DB.Query(`SELECT `+backupCols+` FROM backups WHERE site_id=? ORDER BY id DESC LIMIT ?`, siteID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBackupRecord(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM backups WHERE id=?`, id)
	return err
}

// ---- SSL checks ----

type SSLCheck struct {
	ID         int64
	Domain     string
	OK         bool
	Issuer     string
	ExpiresAt  string
	DaysLeft   int
	Cloudflare bool
	DNSOK      bool
	Detail     string
	CreatedAt  string
}

func (s *Store) AddSSLCheck(websiteID int64, c *SSLCheck) error {
	c.CreatedAt = Now()
	_, err := s.DB.Exec(`INSERT INTO ssl_checks(website_id,domain,ok,issuer,expires_at,days_left,cloudflare,dns_ok,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		websiteID, c.Domain, boolInt(c.OK), c.Issuer, c.ExpiresAt, c.DaysLeft, boolInt(c.Cloudflare), boolInt(c.DNSOK), c.Detail, c.CreatedAt)
	return err
}

func (s *Store) LatestSSLCheck(domain string) (*SSLCheck, error) {
	var c SSLCheck
	var ok, cf, dns int
	var exp sql.NullString
	err := s.DB.QueryRow(`SELECT id,domain,ok,issuer,expires_at,days_left,cloudflare,dns_ok,detail,created_at FROM ssl_checks WHERE domain=? ORDER BY id DESC LIMIT 1`, domain).
		Scan(&c.ID, &c.Domain, &ok, &c.Issuer, &exp, &c.DaysLeft, &cf, &dns, &c.Detail, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	c.OK, c.Cloudflare, c.DNSOK = ok == 1, cf == 1, dns == 1
	if exp.Valid {
		c.ExpiresAt = exp.String
	}
	return &c, nil
}

// ---- Audit & settings ----

func (s *Store) AddAudit(adminID int64, action, target, detail, ip string) error {
	// Redact obvious secret-looking detail fragments.
	detail = Redact(detail)
	_, err := s.DB.Exec(`INSERT INTO audit_logs(admin_id,action,target,detail,ip,created_at) VALUES(?,?,?,?,?,?)`, adminID, action, target, detail, ip, Now())
	return err
}

// secretPattern matches key=value and JSON "key": "value" forms for
// common secret-bearing keys, case-insensitively, so Redact can scrub
// EVERY occurrence (the previous implementation handled only the
// first occurrence of four exact markers).
// The value alternative tries a fully quoted string FIRST: the
// bare-token class stops at the first space, which used to leak
// the remainder of a quoted secret ("password: \"abc def\"" left
// ` def"` behind in stored audit/event rows).
var secretPattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[-_]?key|db_password)(\s*["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s"',;]+)`)

func Redact(s string) string {
	return secretPattern.ReplaceAllString(s, "${1}${2}***")
}

type AuditEntry struct {
	ID        int64
	AdminID   int64
	Action    string
	Target    string
	Detail    string
	IP        string
	CreatedAt string
}

func (s *Store) ListAudit(limit, offset int) ([]*AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.Query(`SELECT id,admin_id,action,target,detail,ip,created_at FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.AdminID, &a.Action, &a.Target, &a.Detail, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) SetSetting(k, v string) error {
	// `key` is backtick-quoted: it is a (non-reserved) keyword in MySQL and
	// SQLite accepts backtick quoting too.
	if s.driver == "mysql" {
		_, err := s.DB.Exec("INSERT INTO system_settings(`key`,value) VALUES(?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)", k, v)
		return err
	}
	_, err := s.DB.Exec("INSERT INTO system_settings(`key`,value) VALUES(?,?) ON CONFLICT(`key`) DO UPDATE SET value=excluded.value", k, v)
	return err
}
func (s *Store) GetSetting(k, def string) string {
	var v string
	if err := s.DB.QueryRow("SELECT value FROM system_settings WHERE `key`=?", k).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) AddResourceSample(scope, siteID string, cpu float64, mem int64) error {
	_, err := s.DB.Exec(`INSERT INTO resource_samples(scope,site_id,cpu_percent,mem_bytes,created_at) VALUES(?,?,?,?,?)`, scope, siteID, cpu, mem, Now())
	return err
	// Retention is enforced by a periodic prune in the panel (bounded storage).
}

func (s *Store) PruneResourceSamples(keepDays int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format(time.RFC3339)
	_, err := s.DB.Exec(`DELETE FROM resource_samples WHERE created_at<?`, cutoff)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- resource samples (read side; KR Host performance/server pages) ----------

// ResourceSample is one recorded host or per-site resource sample.
type ResourceSample struct {
	Scope      string
	SiteID     string
	CPUPercent float64
	MemBytes   int64
	CreatedAt  string
}

// ListResourceSamples returns the newest samples for a scope (and site,
// when siteID is non-empty), oldest first, ready for charting.
func (s *Store) ListResourceSamples(scope, siteID string, limit int) ([]ResourceSample, error) {
	if limit <= 0 || limit > 500 {
		limit = 120
	}
	rows, err := s.DB.Query(`SELECT scope,site_id,cpu_percent,mem_bytes,created_at FROM resource_samples
		WHERE scope=? AND site_id=? ORDER BY created_at DESC LIMIT ?`, scope, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResourceSample
	for rows.Next() {
		var r ResourceSample
		if err := rows.Scan(&r.Scope, &r.SiteID, &r.CPUPercent, &r.MemBytes, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
