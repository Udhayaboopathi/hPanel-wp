package security

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var (
	domainLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	dbNameRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)
	dbUserRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	usernameRe    = regexp.MustCompile(`^[A-Za-z0-9_.\-]{3,60}$`)
	siteIDRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,40}$`)
)

// NormalizeDomain lowercases, trims scheme/path/trailing dot.
func NormalizeDomain(raw string) string {
	d := strings.TrimSpace(strings.ToLower(raw))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	if h, _, err := net.SplitHostPort(d); err == nil {
		d = h
	}
	return d
}

func ValidateDomain(raw string) error {
	d := NormalizeDomain(raw)
	if d == "" || len(d) > 253 {
		return fmt.Errorf("domain is empty or too long")
	}
	if strings.Contains(d, "..") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return fmt.Errorf("invalid domain %q", raw)
	}
	if ip := net.ParseIP(d); ip != nil {
		return fmt.Errorf("domain must be a hostname, not an IP address")
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("domain %q must contain a TLD", raw)
	}
	for _, l := range labels {
		if !domainLabelRe.MatchString(l) {
			return fmt.Errorf("invalid domain label %q", l)
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return fmt.Errorf("invalid TLD %q", tld)
	}
	return nil
}

func ValidateDBName(name string) error {
	if !dbNameRe.MatchString(name) {
		return fmt.Errorf("database name %q must start with a letter, be 3-64 chars of [a-z0-9_]", name)
	}
	for _, reserved := range []string{"mysql", "information_schema", "performance_schema", "sys"} {
		if name == reserved {
			return fmt.Errorf("database name %q is reserved", name)
		}
	}
	return nil
}

func ValidateDBUser(name string) error {
	if !dbUserRe.MatchString(name) {
		return fmt.Errorf("database user %q must start with a letter, be 3-32 chars of [a-z0-9_]", name)
	}
	return nil
}

// ValidateSQLIdentifier is the strict whitelist used before any identifier
// is interpolated into DDL. Identifiers are never taken from raw user SQL.
func ValidateSQLIdentifier(name string) error {
	if err := ValidateDBName(name); err != nil {
		if err2 := ValidateDBUser(name); err2 != nil {
			return err
		}
	}
	if strings.ContainsAny(name, "`'\";\\ \t\n-") {
		return fmt.Errorf("identifier %q contains forbidden characters", name)
	}
	return nil
}

var tablePrefixRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// ValidateTablePrefix checks a WordPress table prefix detected in an
// imported SQL dump before it is written into the site's generated
// configuration (WORDPRESS_TABLE_PREFIX). The value is interpolated
// into generated files, so the charset is deliberately minimal.
func ValidateTablePrefix(p string) error {
	if !tablePrefixRe.MatchString(p) {
		return fmt.Errorf("table prefix %q must be 1-32 chars of [A-Za-z0-9_]", p)
	}
	return nil
}

func ValidateAdminUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return fmt.Errorf("username must be 3-60 chars of letters, digits, _ . -")
	}
	return nil
}

func ValidateSiteID(id string) error {
	if !siteIDRe.MatchString(id) {
		return fmt.Errorf("invalid site id %q", id)
	}
	return nil
}

// emailLocalRe is the RFC 5322 atext set for the local part. The
// local part used to go unchecked entirely, so values with spaces
// or control characters passed as long as the domain validated.
var emailLocalRe = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]{1,64}$`)

func ValidateEmail(e string) error {
	e = strings.TrimSpace(e)
	if len(e) < 5 || len(e) > 254 || !strings.Contains(e, "@") {
		return fmt.Errorf("invalid email address")
	}
	parts := strings.Split(e, "@")
	if len(parts) != 2 || !emailLocalRe.MatchString(parts[0]) {
		return fmt.Errorf("invalid email address")
	}
	return ValidateDomain(parts[1])
}
