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
)

var Allowlist = map[string]bool{
	OpCreateSite: true, OpDeleteSite: true, OpStartSite: true, OpStopSite: true,
	OpRestartSite: true, OpBackupSite: true, OpRestoreSite: true, OpWPCLI: true,
	OpApplyConfig: true, OpRotateDBPassword: true, OpDBSize: true, OpPing: true, OpDiscovery: true,
	OpStats: true, OpLogs: true,
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
}

type Event struct {
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Level    string `json:"level"`
}

type Response struct {
	OK           bool    `json:"ok"`
	Error        string  `json:"error,omitempty"`
	ContainerID  string  `json:"container_id,omitempty"`
	Output       string  `json:"output,omitempty"`
	BackupPath   string  `json:"backup_path,omitempty"`
	BackupSHA256 string  `json:"backup_sha256,omitempty"`
	BackupSize   int64   `json:"backup_size,omitempty"`
	DBSizeBytes  int64   `json:"db_size_bytes,omitempty"`
	Events       []Event `json:"events,omitempty"`
	DNSPending   bool    `json:"dns_pending,omitempty"`
}
