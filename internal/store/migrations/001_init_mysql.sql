-- Own hPanel panel-store schema for MariaDB / MySQL.
-- This is the dialect translation of 001_init.sql (SQLite): identical table
-- and column names, with AUTO_INCREMENT, bounded VARCHARs for indexed /
-- unique columns (utf8mb4 index limits) and DOUBLE instead of REAL.
-- Target server: the dedicated MariaDB 11.4 container of the hPanel stack
-- (wordpress-mariadb, defined in compose.yaml).
-- The migration-lint test in internal/store asserts both files create the
-- same set of tables.
CREATE TABLE IF NOT EXISTS administrators(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  username VARCHAR(191) NOT NULL UNIQUE,
  email VARCHAR(191) NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL,
  totp_enabled INT NOT NULL DEFAULT 0,
  created_at VARCHAR(191) NOT NULL,
  updated_at VARCHAR(191) NOT NULL
);
CREATE TABLE IF NOT EXISTS recovery_codes(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  admin_id BIGINT NOT NULL REFERENCES administrators(id) ON DELETE CASCADE,
  code_hash VARCHAR(191) NOT NULL UNIQUE,
  used_at VARCHAR(191)
);
CREATE TABLE IF NOT EXISTS sessions(
  token_hash VARCHAR(191) PRIMARY KEY,
  admin_id BIGINT NOT NULL REFERENCES administrators(id) ON DELETE CASCADE,
  csrf_token VARCHAR(191) NOT NULL,
  ip VARCHAR(191) NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL,
  expires_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_admin ON sessions(admin_id);
CREATE TABLE IF NOT EXISTS login_attempts(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  username VARCHAR(191) NOT NULL,
  ip VARCHAR(191) NOT NULL,
  success INT NOT NULL DEFAULT 0,
  created_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_attempts ON login_attempts(username, ip, created_at);
CREATE TABLE IF NOT EXISTS websites(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  site_id VARCHAR(191) NOT NULL UNIQUE,
  name VARCHAR(191) NOT NULL DEFAULT '',
  domain VARCHAR(191) NOT NULL UNIQUE,
  admin_domain VARCHAR(191) NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  status VARCHAR(64) NOT NULL DEFAULT 'provisioning',
  php_version VARCHAR(32) NOT NULL DEFAULT '8.3',
  wp_version VARCHAR(64) NOT NULL DEFAULT '',
  resource_profile VARCHAR(64) NOT NULL DEFAULT 'lightweight',
  cpu_limit DOUBLE NOT NULL DEFAULT 0.5,
  mem_limit_mb INT NOT NULL DEFAULT 512,
  container_name VARCHAR(191) NOT NULL DEFAULT '',
  container_id VARCHAR(191) NOT NULL DEFAULT '',
  db_name VARCHAR(191) NOT NULL DEFAULT '',
  db_user VARCHAR(191) NOT NULL DEFAULT '',
  db_password_enc TEXT NOT NULL,
  admin_username VARCHAR(191) NOT NULL DEFAULT '',
  admin_email VARCHAR(191) NOT NULL DEFAULT '',
  site_dir TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL,
  updated_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_websites_status ON websites(status);
CREATE TABLE IF NOT EXISTS website_domains(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT NOT NULL REFERENCES websites(id) ON DELETE CASCADE,
  domain VARCHAR(191) NOT NULL UNIQUE,
  kind VARCHAR(32) NOT NULL DEFAULT 'primary',
  created_at VARCHAR(191) NOT NULL
);
CREATE TABLE IF NOT EXISTS website_configurations(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT NOT NULL REFERENCES websites(id) ON DELETE CASCADE,
  version INT NOT NULL,
  env_json TEXT NOT NULL,
  compose_yaml TEXT NOT NULL,
  note TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL,
  UNIQUE(website_id, version)
);
CREATE TABLE IF NOT EXISTS website_databases(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT NOT NULL UNIQUE REFERENCES websites(id) ON DELETE CASCADE,
  db_name VARCHAR(191) NOT NULL UNIQUE,
  db_user VARCHAR(191) NOT NULL UNIQUE,
  size_bytes BIGINT NOT NULL DEFAULT 0,
  last_checked_at VARCHAR(191)
);
CREATE TABLE IF NOT EXISTS deployments(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT REFERENCES websites(id) ON DELETE SET NULL,
  job_id VARCHAR(191) NOT NULL DEFAULT '',
  action VARCHAR(191) NOT NULL,
  status VARCHAR(64) NOT NULL,
  detail TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL
);
CREATE TABLE IF NOT EXISTS deployment_events(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  job_id VARCHAR(191) NOT NULL,
  level VARCHAR(32) NOT NULL DEFAULT 'info',
  message TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_depev_job ON deployment_events(job_id, id);
CREATE TABLE IF NOT EXISTS jobs(
  id VARCHAR(191) PRIMARY KEY,
  website_id BIGINT REFERENCES websites(id) ON DELETE SET NULL,
  site_id VARCHAR(191) NOT NULL DEFAULT '',
  kind VARCHAR(64) NOT NULL,
  status VARCHAR(64) NOT NULL DEFAULT 'queued',
  progress INT NOT NULL DEFAULT 0,
  message TEXT NOT NULL,
  error TEXT NOT NULL,
  attempts INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  payload_json TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL,
  updated_at VARCHAR(191) NOT NULL,
  started_at VARCHAR(191),
  finished_at VARCHAR(191)
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_site ON jobs(site_id);
CREATE TABLE IF NOT EXISTS backups(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT REFERENCES websites(id) ON DELETE SET NULL,
  site_id VARCHAR(191) NOT NULL DEFAULT '',
  filename VARCHAR(255) NOT NULL,
  path TEXT NOT NULL,
  size_bytes BIGINT NOT NULL DEFAULT 0,
  sha256 VARCHAR(191) NOT NULL DEFAULT '',
  status VARCHAR(64) NOT NULL DEFAULT 'creating',
  storage VARCHAR(64) NOT NULL DEFAULT 'local',
  created_at VARCHAR(191) NOT NULL,
  verified_at VARCHAR(191)
);
CREATE INDEX IF NOT EXISTS idx_backups_site ON backups(site_id, created_at);
CREATE TABLE IF NOT EXISTS ssl_checks(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  website_id BIGINT REFERENCES websites(id) ON DELETE CASCADE,
  domain VARCHAR(191) NOT NULL,
  ok INT NOT NULL DEFAULT 0,
  issuer TEXT NOT NULL,
  expires_at VARCHAR(191),
  days_left INT NOT NULL DEFAULT 0,
  cloudflare INT NOT NULL DEFAULT 0,
  dns_ok INT NOT NULL DEFAULT 0,
  detail TEXT NOT NULL,
  created_at VARCHAR(191) NOT NULL
);
CREATE TABLE IF NOT EXISTS resource_samples(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  scope VARCHAR(64) NOT NULL DEFAULT 'host',
  site_id VARCHAR(191) NOT NULL DEFAULT '',
  cpu_percent DOUBLE NOT NULL DEFAULT 0,
  mem_bytes BIGINT NOT NULL DEFAULT 0,
  created_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples ON resource_samples(scope, site_id, created_at);
CREATE TABLE IF NOT EXISTS audit_logs(
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  admin_id BIGINT NOT NULL DEFAULT 0,
  action VARCHAR(191) NOT NULL,
  target VARCHAR(191) NOT NULL DEFAULT '',
  detail TEXT NOT NULL,
  ip VARCHAR(191) NOT NULL DEFAULT '',
  created_at VARCHAR(191) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit ON audit_logs(created_at);
CREATE TABLE IF NOT EXISTS system_settings(
  `key` VARCHAR(191) PRIMARY KEY,
  value TEXT NOT NULL
);
