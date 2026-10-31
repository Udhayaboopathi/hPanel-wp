package security

import "testing"

func TestValidateDomain(t *testing.T) {
	valid := []string{"example.com", "sub.example.co.uk", "my-site.org", "EXAMPLE.com", "https://example.com/path"}
	for _, d := range valid {
		if err := ValidateDomain(d); err != nil {
			t.Errorf("expected %q valid: %v", d, err)
		}
	}
	invalid := []string{"", "localhost", "exa mple.com", "-bad.com", "bad-.com", "a..com", "1.2.3.4", "example.c", ".example.com", "exam_ple.com", "example.com.."}
	for _, d := range invalid {
		if err := ValidateDomain(d); err == nil {
			t.Errorf("expected %q invalid", d)
		}
	}
}

func TestValidateDBName(t *testing.T) {
	for _, n := range []string{"wp_ab12_db", "site1_db", "abc"} {
		if err := ValidateDBName(n); err != nil {
			t.Errorf("expected %q valid: %v", n, err)
		}
	}
	for _, n := range []string{"", "ab", "1abc_db", "Abc_db", "a-b", "a;b", "a`b", "mysql", "wp db"} {
		if err := ValidateDBName(n); err == nil {
			t.Errorf("expected %q invalid", n)
		}
	}
}

func TestSafeJoinTraversal(t *testing.T) {
	root := "/srv/sites/wp-1/data"
	if _, err := SafeJoin(root, "../secret"); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := SafeJoin(root, "../../etc/passwd"); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := SafeJoin(root, "/etc/passwd"); err == nil {
		t.Error("expected absolute path to be rejected")
	}
	got, err := SafeJoin(root, "wp-content/uploads/a.txt")
	if err != nil || got != "/srv/sites/wp-1/data/wp-content/uploads/a.txt" {
		t.Errorf("unexpected join result %q err %v", got, err)
	}
}

func TestValidateSQLIdentifierInjection(t *testing.T) {
	for _, n := range []string{"db; DROP DATABASE x", "db`--", "db' OR '1'='1", "db name"} {
		if err := ValidateSQLIdentifier(n); err == nil {
			t.Errorf("expected %q rejected", n)
		}
	}
}
