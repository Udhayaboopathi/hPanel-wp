// Package agentproto defines the narrow panel<->agent protocol spoken over
// a Unix domain socket. Every operation is allowlisted; the agent
// re-validates every field and refuses anything outside this protocol.
package agentproto

const (
	OpCreateSite       = "CreateSite"
	OpDeleteSite       = "DeleteSite"
	OpStartSite        = "StartSite"
	OpStopSite         = "StopSite"
	OpRestartSite      = "RestartSite"
	OpBackupSite       = "BackupSite"
	OpRestoreSite      = "RestoreSite"
	OpWPCLI            = "WPCLI"
	OpApplyConfig      = "ApplyConfig"
	OpRotateDBPassword = "RotateDBPassword"
	OpDBSize           = "DBSize"
	OpPing             = "Ping"
	OpDiscovery        = "Discovery"
	OpStats            = "Stats"
	OpLogs             = "Logs"
	OpRepairSite       = "RepairSite"
)

var Allowlist = map[string]bool{
	OpCreateSite: true, OpDeleteSite: true, OpStartSite: true, OpStopSite: true,
	OpRestartSite: true, OpBackupSite: true, OpRestoreSite: true, OpWPCLI: true,
	OpApplyConfig: true, OpRotateDBPassword: true, OpDBSize: true, OpPing: true, OpDiscovery: true,
	OpStats: true, OpLogs: true, OpRepairSite: true,
}

type SiteSpec struct {
	SiteID        string  `json:"site_id"`
	Domain        string  `json:"domain"`
	AdminDomain   string  `json:"admin_domain"`
	Title         string  `json:"title"`
	DBName        string  `json:"db_name"`
	DBUser        string  `json:"db_user"`
	DBPassword    string  `json:"db_password,omitempty"`
	AdminUser     string  `json:"admin_user"`
	AdminEmail    string  `json:"admin_email"`
	AdminPassword string  `json:"admin_password,omitempty"`
	PHPVersion    string  `json:"php_version"`
	Profile       string  `json:"profile"`
	CPULimit      float64 `json:"cpu_limit"`
	MemLimitMB    int     `json:"mem_limit_mb"`
	SiteDir       string  `json:"site_dir"`
	ContainerName string  `json:"container_name"`
	ComposeYAML   string  `json:"compose_yaml,omitempty"`
	EnvText       string  `json:"env_text,omitempty"`
}

type Request struct {
	Op    string   `json:"op"`
	Site  SiteSpec `json:"site"`
	JobID string   `json:"job_id"`
	// WPCLI action/arg (allowlisted in internal/wordpress)
	Action string `json:"action,omitempty"`
	Arg    string `json:"arg,omitempty"`
	// Backup/restore
	BackupID      int64  `json:"backup_id,omitempty"`
	BackupPath    string `json:"backup_path,omitempty"`
	NewDBPassword string `json:"new_db_password,omitempty"`
	// ExpectedSHA256 is the checksum the panel recorded for the
	// backup artifact; the agent verifies it before restoring.
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	// RepairMode relaxes CreateSite's secret requirements for the
	// RepairSite operation: the WordPress admin password is not
	// stored anywhere, so a repair cannot be required to present it.
	// When WordPress turns out not to be installed and no admin
	// password was supplied, provisioning leaves the browser setup
	// wizard for the operator instead of failing.
	RepairMode bool `json:"repair_mode,omitempty"`
}

type Event struct {
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Level    string `json:"level"`
}

// Error categories the agent attaches to operational failures so the
// panel can react correctly (retry policy, website status, recovery
// hints) instead of parsing error text.
const (
	ErrCatNotFound    = "not_found"    // the site's container does not exist
	ErrCatAmbiguous   = "ambiguous"    // several containers claim the site's labels
	ErrCatGuard       = "guard"        // a same-named resource exists but is not panel-owned
	ErrCatDataMissing = "data_missing" // the site's database or files are gone
)

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// ErrorCategory is one of the ErrCat* constants ("" = uncategorised).
	ErrorCategory string  `json:"error_category,omitempty"`
	ContainerID   string  `json:"container_id,omitempty"`
	Output        string  `json:"output,omitempty"`
	BackupPath    string  `json:"backup_path,omitempty"`
	BackupSHA256  string  `json:"backup_sha256,omitempty"`
	BackupSize    int64   `json:"backup_size,omitempty"`
	DBSizeBytes   int64   `json:"db_size_bytes,omitempty"`
	Events        []Event `json:"events,omitempty"`
	DNSPending    bool    `json:"dns_pending,omitempty"`
	// UsedCommonDBUser is set when CreateSite had to fall back to the
	// common DB user because the admin account lacks CREATE USER/GRANT
	// privileges. The panel records an audit warning when this is true.
	UsedCommonDBUser bool `json:"used_common_db_user,omitempty"`
}

// ProtectedContainers is the display copy of the deployment guard
// list (.github/workflows/deploy.yml): pre-existing production
// containers on the host that RK Host must never modify. The agent's
// real protection is label-based (it refuses every container without
// the managed label); this set exists so the UI can badge the known
// production workloads as protected/read-only.
var ProtectedContainers = map[string]bool{
	"krweb-ui": true, "redberyl-api": true, "redberyl-ui": true,
	"redberyl-rustfs": true, "redberyl-postgres": true,
	"construction-api": true, "construction-ui": true,
	"construction-mariadb": true, "redberyl-postgres-proxy": true,
	"redberyl-traefik": true,
}
