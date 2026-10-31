package dbconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{"DB_CONTAINER", "DB_HOST", "DB_EXTERNAL_PORT", "DB_ADMIN_USER", "DB_ADMIN_PASSWORD", "DB_ADMIN_PASSWORD_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Container != "wordpress-mariadb" || cfg.Host != "db.krwebsolution.com" || cfg.ExternalPort != 3024 || cfg.AdminUser != "WordPress_common_DB" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.InternalHost() != cfg.Container {
		t.Fatal("internal host must be the DB container name")
	}
}

func TestFromEnvPasswordFilePreferred(t *testing.T) {
	f := filepath.Join(t.TempDir(), "adminpw")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_ADMIN_PASSWORD_FILE", f)
	t.Setenv("DB_ADMIN_PASSWORD", "from-env")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminPassword != "from-file" {
		t.Fatalf("password file must win and be trimmed, got %q", cfg.AdminPassword)
	}
}

func TestFromEnvBadPort(t *testing.T) {
	t.Setenv("DB_EXTERNAL_PORT", "notaport")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected invalid port to be rejected")
	}
}

func TestIsPrivilegeError(t *testing.T) {
	yes := []string{
		"mariadb command exited 1: ERROR 1227 (42000): Access denied; you need the SUPER privilege",
		"ERROR 1142 (42000): CREATE command denied to user",
		"ERROR 1045 (28000): Access denied for user",
		"ERROR 1044 (42000): Access denied for user to database",
	}
	for _, s := range yes {
		if !IsPrivilegeError(errors.New(s)) {
			t.Errorf("expected privilege error: %s", s)
		}
	}
	no := []string{
		"connection refused",
		"syntax error near line 1",
	}
	for _, s := range no {
		if IsPrivilegeError(errors.New(s)) {
			t.Errorf("not a privilege error: %s", s)
		}
	}
	if IsPrivilegeError(nil) {
		t.Error("nil must not be a privilege error")
	}
}
