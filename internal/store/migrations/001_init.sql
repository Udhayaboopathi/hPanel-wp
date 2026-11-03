CREATE TABLE IF NOT EXISTS administrators(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  email TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL DEFAULT '',
  totp_enabled INTEGER NOT NULL DEFAULT 0,
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS recovery_codes(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER NOT NULL REFERENCES administrators(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL UNIQUE,
  used_at TEXT
);
CREATE TABLE IF NOT EXISTS sessions(
  token_hash TEXT PRIMARY KEY,
  admin_id INTEGER NOT NULL REFERENCES administrators(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_admin ON sessions(admin_id);
CREATE TABLE IF NOT EXISTS login_attempts(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL,
  ip TEXT NOT NULL,
  success INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_attempts ON login_attempts(username, ip, created_at);
CREATE TABLE IF NOT EXISTS websites(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL UNIQUE,
  admin_domain TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'provisioning',
  php_version TEXT NOT NULL DEFAULT '8.3',
  wp_version TEXT NOT NULL DEFAULT '',
  resource_profile TEXT NOT NULL DEFAULT 'lightweight',
  cpu_limit REAL NOT NULL DEFAULT 0.5,
  mem_limit_mb INTEGER NOT NULL DEFAULT 512,
  container_name TEXT NOT NULL DEFAULT '',
  container_id TEXT NOT NULL DEFAULT '',
  db_name TEXT NOT NULL DEFAULT '',
  db_user TEXT NOT NULL DEFAULT '',
  db_password_enc TEXT NOT NULL DEFAULT '',
  admin_username TEXT NOT NULL DEFAULT '',
  admin_email TEXT NOT NULL DEFAULT '',
  site_dir TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_websites_status ON websites(status);
CREATE TABLE IF NOT EXISTS website_domains(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER NOT NULL REFERENCES websites(id) ON DELETE CASCADE,
  domain TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL DEFAULT 'primary',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS website_configurations(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER NOT NULL REFERENCES websites(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  env_json TEXT NOT NULL DEFAULT '{}',
  compose_yaml TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(website_id, version)
);
CREATE TABLE IF NOT EXISTS website_databases(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER NOT NULL UNIQUE REFERENCES websites(id) ON DELETE CASCADE,
  db_name TEXT NOT NULL UNIQUE,
  db_user TEXT NOT NULL UNIQUE,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  last_checked_at TEXT
);
CREATE TABLE IF NOT EXISTS deployments(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER REFERENCES websites(id) ON DELETE SET NULL,
  job_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployment_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  level TEXT NOT NULL DEFAULT 'info',
  message TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_depev_job ON deployment_events(job_id, id);
CREATE TABLE IF NOT EXISTS jobs(
  id TEXT PRIMARY KEY,
  website_id INTEGER REFERENCES websites(id) ON DELETE SET NULL,
  site_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  progress INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_site ON jobs(site_id);
CREATE TABLE IF NOT EXISTS backups(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER REFERENCES websites(id) ON DELETE SET NULL,
  site_id TEXT NOT NULL DEFAULT '',
  filename TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'creating',
  storage TEXT NOT NULL DEFAULT 'local',
  created_at TEXT NOT NULL,
  verified_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_backups_site ON backups(site_id, created_at);
CREATE TABLE IF NOT EXISTS ssl_checks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  website_id INTEGER REFERENCES websites(id) ON DELETE CASCADE,
  domain TEXT NOT NULL,
  ok INTEGER NOT NULL DEFAULT 0,
  issuer TEXT NOT NULL DEFAULT '',
  expires_at TEXT,
  days_left INTEGER NOT NULL DEFAULT 0,
  cloudflare INTEGER NOT NULL DEFAULT 0,
  dns_ok INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS resource_samples(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL DEFAULT 'host',
  site_id TEXT NOT NULL DEFAULT '',
  cpu_percent REAL NOT NULL DEFAULT 0,
  mem_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples ON resource_samples(scope, site_id, created_at);
CREATE TABLE IF NOT EXISTS audit_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER NOT NULL DEFAULT 0,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit ON audit_logs(created_at);
CREATE TABLE IF NOT EXISTS system_settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
