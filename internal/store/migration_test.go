package store

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

var createTableRe = regexp.MustCompile("(?i)CREATE TABLE IF NOT EXISTS `?([a-z_]+)`?")

func tableSet(sqlText string) map[string]bool {
	set := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(sqlText, -1) {
		set[m[1]] = true
	}
	return set
}

// The SQLite and MySQL migrations must create exactly the same tables -
// the store code is shared and assumes one schema.
func TestMigrationsCreateSameTables(t *testing.T) {
	lite := tableSet(migration001)
	my := tableSet(migration001MySQL)
	if len(lite) == 0 || len(my) == 0 {
		t.Fatalf("migration parse failed: sqlite=%d mysql=%d tables", len(lite), len(my))
	}
	var onlyLite, onlyMy []string
	for name := range lite {
		if !my[name] {
			onlyLite = append(onlyLite, name)
		}
	}
	for name := range my {
		if !lite[name] {
			onlyMy = append(onlyMy, name)
		}
	}
	sort.Strings(onlyLite)
	sort.Strings(onlyMy)
	if len(onlyLite) > 0 || len(onlyMy) > 0 {
		t.Fatalf("schema drift: only in sqlite: %v; only in mysql: %v", onlyLite, onlyMy)
	}
	for _, want := range []string{"administrators", "sessions", "websites", "jobs", "backups", "audit_logs", "system_settings"} {
		if !lite[want] {
			t.Fatalf("expected core table %q in migrations", want)
		}
	}
}

// Column names for shared tables must also match, because every query in
// store.go runs unchanged against both drivers.
func TestMigrationColumnsMatchForCoreTables(t *testing.T) {
	for _, table := range []string{"websites", "jobs", "administrators", "system_settings"} {
		liteCols := columnsOf(t, migration001, table)
		myCols := columnsOf(t, migration001MySQL, table)
		if strings.Join(liteCols, ",") != strings.Join(myCols, ",") {
			t.Fatalf("table %s columns differ:\n sqlite: %v\n mysql:  %v", table, liteCols, myCols)
		}
	}
}

func columnsOf(t *testing.T, sqlText, table string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS ` + "`?" + table + "`?" + `\((.*?)\n\);`)
	m := re.FindStringSubmatch(sqlText)
	if m == nil {
		t.Fatalf("table %s not found", table)
	}
	var cols []string
	for _, ln := range strings.Split(m[1], "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		first := strings.Fields(strings.TrimSuffix(ln, ","))[0]
		first = strings.Trim(first, "`")
		upper := strings.ToUpper(first)
		if upper == "UNIQUE" || upper == "PRIMARY" || upper == "REFERENCES" || upper == "CONSTRAINT" {
			continue
		}
		cols = append(cols, first)
	}
	return cols
}

func TestSplitStatements(t *testing.T) {
	stmts := SplitStatements(migration001MySQL)
	tables := 0
	for _, s := range stmts {
		if strings.Contains(s, ";") {
			t.Fatalf("statement still contains a semicolon (split failed): %.60s", s)
		}
		if strings.HasPrefix(strings.ToUpper(s), "CREATE TABLE") {
			tables++
		}
	}
	if tables != len(tableSet(migration001MySQL)) {
		t.Fatalf("expected %d CREATE TABLE statements, got %d", len(tableSet(migration001MySQL)), tables)
	}
	if len(stmts) < tables {
		t.Fatalf("expected at least the table statements, got %d", len(stmts))
	}
}

func TestBuildMySQLDSN(t *testing.T) {
	dsn := BuildMySQLDSN("db.example.test", 3024, "hpanel_panel", "common_user", "pw")
	for _, want := range []string{"common_user:pw@tcp(db.example.test:3024)/hpanel_panel", "charset=utf8mb4"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN %q missing %q", dsn, want)
		}
	}
}

func TestOpenDriverRejectsUnknown(t *testing.T) {
	if _, err := OpenDriver("postgres", "x"); err == nil {
		t.Fatal("unknown driver must be rejected")
	}
}

func TestSQLiteDriverReported(t *testing.T) {
	s := openTest(t)
	if s.Driver() != "sqlite" {
		t.Fatalf("expected sqlite driver, got %q", s.Driver())
	}
	// Settings roundtrip exercises the upsert path on the sqlite dialect.
	if err := s.SetSetting("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k1", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetSetting("k1", ""); got != "v2" {
		t.Fatalf("upsert failed, got %q", got)
	}
}
