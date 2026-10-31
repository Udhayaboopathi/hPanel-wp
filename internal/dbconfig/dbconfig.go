// Package dbconfig holds the environment-driven configuration for the ONE
// shared MariaDB server used by every WordPress site and by the panel
// itself. That server is the pre-existing `construction-mariadb` container
// (MariaDB 11.4) on the production host. Own hPanel NEVER creates, stops,
// restarts, removes or recreates it; the stack only connects to it.
//
// The provisioning/admin account is a single common user (default
// WordPress_common_DB). Its password is only ever read from a secret file
// (DB_ADMIN_PASSWORD_FILE, preferred) or an environment variable
// (DB_ADMIN_PASSWORD, development only). It is never logged, never written
// to the repository, and never stored in the panel database.
package dbconfig

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultContainer    = "construction-mariadb"
	DefaultHost         = "db.krwebsolution.com"
	DefaultExternalPort = 3024
	DefaultAdminUser    = "WordPress_common_DB"
	DefaultPanelDBName  = "hpanel_panel"
	// InternalPort is the MariaDB port used inside Docker networks, where
	// containers address the DB container by name.
	InternalPort = 3306
)

type Config struct {
	// Container is the Docker name of the pre-existing shared MariaDB
	// container (DB_CONTAINER). It doubles as the internal hostname WP
	// containers use (WORDPRESS_DB_HOST) once the agent connects the
	// container to each per-site network.
	Container string
	// Host is the external/testing hostname of the DB server (DB_HOST).
	Host string
	// ExternalPort is the host-published port for external/testing access
	// (DB_EXTERNAL_PORT). The panel's own MySQL store connects through
	// Host:ExternalPort.
	ExternalPort int
	// AdminUser is the common provisioning account (DB_ADMIN_USER).
	AdminUser string
	// AdminPassword is the common account's password, loaded from a secret
	// file or env. Never log it.
	AdminPassword string
}

// InternalHost is the hostname WordPress containers use for the database:
// the DB container's name on their per-site Docker network (port 3306).
func (c Config) InternalHost() string { return c.Container }

// FromEnv builds the config from the environment. The password is read from
// DB_ADMIN_PASSWORD_FILE when set (contents are trimmed), otherwise from
// DB_ADMIN_PASSWORD. A missing password is NOT an error here - callers that
// need the credential check AdminPassword themselves so read-only paths
// (discovery, stats) can still run.
func FromEnv() (Config, error) {
	cfg := Config{
		Container:    envOr("DB_CONTAINER", DefaultContainer),
		Host:         envOr("DB_HOST", DefaultHost),
		ExternalPort: DefaultExternalPort,
		AdminUser:    envOr("DB_ADMIN_USER", DefaultAdminUser),
	}
	if v := os.Getenv("DB_EXTERNAL_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return cfg, fmt.Errorf("DB_EXTERNAL_PORT %q is not a valid port", v)
		}
		cfg.ExternalPort = p
	}
	if f := os.Getenv("DB_ADMIN_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return cfg, fmt.Errorf("cannot read DB_ADMIN_PASSWORD_FILE: %w", err)
		}
		cfg.AdminPassword = strings.TrimSpace(string(b))
	} else {
		cfg.AdminPassword = os.Getenv("DB_ADMIN_PASSWORD") // dev only; file preferred
	}
	return cfg, nil
}

// IsPrivilegeError reports whether an error coming back from a MariaDB
// client looks like a privilege/authorization failure (the common admin
// account lacking CREATE USER / GRANT / CREATE DATABASE rights). MariaDB
// codes: 1044/1045 access denied, 1142 command denied, 1227 needs SUPER or
// a specific privilege, 1370 grant denied, 1410 not allowed to create user.
func IsPrivilegeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"access denied", "command denied", "grant denied",
		"error 1044", "error 1045", "error 1142", "error 1227",
		"error 1370", "error 1410", "not allowed to create",
		"(1044", "(1045", "(1142", "(1227",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
