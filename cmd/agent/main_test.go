package main

import (
	"strings"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/dbconfig"
)

func testAgent() *Agent {
	return &Agent{db: dbconfig.Config{
		Container:    "construction-mariadb",
		Host:         "db.krwebsolution.com",
		ExternalPort: 3024,
		AdminUser:    "WordPress_common_DB",
	}}
}

func TestRefuseDBContainer(t *testing.T) {
	a := testAgent()
	// The shared pre-existing MariaDB container must never be a lifecycle
	// target (stop/restart/remove/recreate) under any operation.
	if err := a.refuseDBContainer("construction-mariadb"); err == nil {
		t.Fatal("agent must refuse lifecycle operations on the shared DB container")
	}
	for _, ok := range []string{"own-hpanel-wp-abc123", "own-hpanel", ""} {
		if err := a.refuseDBContainer(ok); err != nil {
			t.Fatalf("container %q must be allowed: %v", ok, err)
		}
	}
	// A customised DB_CONTAINER name is protected too.
	a2 := &Agent{db: dbconfig.Config{Container: "my-shared-db", AdminUser: "u"}}
	if err := a2.refuseDBContainer("my-shared-db"); err == nil {
		t.Fatal("custom DB container name must also be refused")
	}
}

func TestUsesCommonUser(t *testing.T) {
	a := testAgent()
	if !a.usesCommonUser("WordPress_common_DB") {
		t.Fatal("the configured admin user is the common user")
	}
	if a.usesCommonUser("wp_abc_usr") {
		t.Fatal("a dedicated per-site user is not the common user")
	}
	if a.usesCommonUser("") {
		t.Fatal("empty user is not the common user")
	}
}

func TestMariaDBArgvUsesAdminUserNotRoot(t *testing.T) {
	argv := mariadbArgv("WordPress_common_DB", "pw-value", "SELECT 1;")
	if len(argv) != 5 || argv[0] != "mariadb" {
		t.Fatalf("unexpected argv shape: %v", argv[:1])
	}
	if argv[1] != "-uWordPress_common_DB" {
		t.Fatalf("must authenticate as the common admin user, got %q", argv[1])
	}
	for _, a := range argv {
		if a == "-uroot" || a == "root" {
			t.Fatal("agent must never connect to the shared MariaDB as root")
		}
	}
	if !strings.HasSuffix(argv[4], "SELECT 1;") || argv[3] != "-e" {
		t.Fatalf("SQL must be passed via -e: %v", argv[3:])
	}
}
