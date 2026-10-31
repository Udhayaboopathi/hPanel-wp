package main

import (
	"strings"
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/dbconfig"
)

func testAgent() *Agent {
	return &Agent{db: dbconfig.Config{
		Container:    "wordpress-mariadb",
		Host:         "db.krwebsolution.com",
		ExternalPort: 3024,
		AdminUser:    "WordPress_common_DB",
	}}
}

func TestRefuseDBContainer(t *testing.T) {
	a := testAgent()
	// The panel-managed MariaDB container must never be a lifecycle
	// target (stop/restart/remove/recreate) of the AGENT under any
	// operation - its lifecycle belongs to Docker Compose alone.
	if err := a.refuseDBContainer("wordpress-mariadb"); err == nil {
		t.Fatal("agent must refuse lifecycle operations on the DB container")
	}
	// Any other pre-existing production container is refused as a DB
	// target too once it is the configured container name (tested below
	// with a custom name); as a plain lifecycle target here it is simply
	// not the DB container.
	for _, ok := range []string{"own-hpanel-wp-abc123", "own-hpanel", ""} {
		if err := a.refuseDBContainer(ok); err != nil {
			t.Fatalf("container %q must be allowed: %v", ok, err)
		}
	}
	// A customised DB_CONTAINER name is protected too.
	a2 := &Agent{db: dbconfig.Config{Container: "my-own-db", AdminUser: "u"}}
	if err := a2.refuseDBContainer("my-own-db"); err == nil {
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
	argv := mariadbArgv("WordPress_common_DB", "SELECT 1;")
	if len(argv) != 4 || argv[0] != "mariadb" {
		t.Fatalf("unexpected argv shape: %v", argv)
	}
	if argv[1] != "-uWordPress_common_DB" {
		t.Fatalf("must authenticate as the common admin user, got %q", argv[1])
	}
	for _, a := range argv {
		if a == "-uroot" || a == "root" {
			t.Fatal("agent must never connect to the MariaDB container as root")
		}
		if strings.HasPrefix(a, "-p") {
			t.Fatal("the password must travel via MYSQL_PWD env, never in argv")
		}
	}
	if !strings.HasSuffix(argv[3], "SELECT 1;") || argv[2] != "-e" {
		t.Fatalf("SQL must be passed via -e: %v", argv[2:])
	}
}
