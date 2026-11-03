package websites

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

// GenerateSiteID returns e.g. wp-a1b2c3d4e5f60718 (unique internal
// identifier). 64 bits of randomness: the previous 32-bit IDs made
// collisions a matter of time as the fleet grows, and the ignored
// rand error could silently mint an all-zero ID.
func GenerateSiteID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is practically unreachable; fall
		// back to time-derived bytes rather than zeros.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	return "wp-" + hex.EncodeToString(b)
}

func ContainerName(siteID string) string { return "own-hpanel-" + siteID }
func NetworkName(siteID string) string   { return "own-hpanel-net-" + siteID }

// DBNameFor derives a valid unique database name from a site id.
func DBNameFor(siteID string) string {
	return strings.ReplaceAll(siteID, "-", "_") + "_db"
}
func DBUserFor(siteID string) string {
	return strings.ReplaceAll(siteID, "-", "_") + "_usr"
}

// ValidateCreateInput validates the create-wizard input end to end.
type CreateInput struct {
	Domain        string
	Title         string
	DBName        string
	AdminUser     string
	AdminEmail    string
	AdminPassword string
	AdminDomain   string
	PHPVersion    string
	Profile       string
	CPULimit      float64
	MemLimitMB    int
}

func (in *CreateInput) Validate() error {
	in.Domain = security.NormalizeDomain(in.Domain)
	if err := security.ValidateDomain(in.Domain); err != nil {
		return err
	}
	if in.Title == "" {
		in.Title = in.Domain
	}
	// The title is interpolated into generated files (site .env as
	// WORDPRESS_TITLE, compose labels, the WP install): a newline in
	// it would inject extra lines/keys into those files.
	if strings.ContainsAny(in.Title, "\r\n") {
		return fmt.Errorf("site title must not contain line breaks")
	}
	if in.DBName != "" {
		if err := security.ValidateDBName(in.DBName); err != nil {
			return err
		}
	}
	if err := security.ValidateAdminUsername(in.AdminUser); err != nil {
		return fmt.Errorf("wordpress admin username: %w", err)
	}
	if err := security.ValidateEmail(in.AdminEmail); err != nil {
		return err
	}
	if len(in.AdminPassword) < 12 {
		return fmt.Errorf("wordpress admin password must be at least 12 characters")
	}
	if in.AdminDomain == "" {
		in.AdminDomain = "wpadmin." + in.Domain
	}
	in.AdminDomain = security.NormalizeDomain(in.AdminDomain)
	if err := security.ValidateDomain(in.AdminDomain); err != nil {
		return fmt.Errorf("admin domain: %w", err)
	}
	if in.PHPVersion == "" {
		in.PHPVersion = "8.3"
	}
	if in.Profile == "" {
		in.Profile = "lightweight"
	}
	// Validate end to end, as documented: unsupported PHP
	// versions, unknown profiles and invalid custom resources are
	// rejected HERE instead of relying on every later caller to
	// re-check them downstream.
	if in.PHPVersion != "8.3" && in.PHPVersion != "8.2" {
		return fmt.Errorf("PHP version %q is not supported (use 8.3 or 8.2)", in.PHPVersion)
	}
	switch in.Profile {
	case "lightweight", "standard", "custom":
	default:
		return fmt.Errorf("unknown resource profile %q", in.Profile)
	}
	if in.Profile == "custom" {
		if math.IsNaN(in.CPULimit) || math.IsInf(in.CPULimit, 0) || in.CPULimit < 0.1 || in.CPULimit > 4 {
			return fmt.Errorf("cpu limit must be between 0.1 and 4")
		}
		if in.MemLimitMB < 256 || in.MemLimitMB > 4096 {
			return fmt.Errorf("memory limit must be between 256 MB and 4096 MB")
		}
	}
	return nil
}
