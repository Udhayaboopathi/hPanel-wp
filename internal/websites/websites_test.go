package websites

import "testing"

func TestCreateInputValidationAndDefaults(t *testing.T) {
	in := CreateInput{Domain: "Example.COM", AdminUser: "siteadmin", AdminEmail: "a@example.com", AdminPassword: "long-enough-password"}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	if in.Domain != "example.com" {
		t.Errorf("domain not normalized: %q", in.Domain)
	}
	if in.AdminDomain != "wpadmin.example.com" {
		t.Errorf("admin domain default wrong: %q", in.AdminDomain)
	}
	if in.PHPVersion != "8.3" || in.Profile != "lightweight" {
		t.Error("defaults not applied")
	}
	bad := CreateInput{Domain: "not a domain", AdminUser: "ab", AdminEmail: "nope", AdminPassword: "short"}
	if err := bad.Validate(); err == nil {
		t.Error("expected invalid input rejected")
	}
	if DBNameFor("wp-ab12cd34") != "wp_ab12cd34_db" || DBUserFor("wp-ab12cd34") != "wp_ab12cd34_usr" {
		t.Error("db name/user derivation wrong")
	}
}
