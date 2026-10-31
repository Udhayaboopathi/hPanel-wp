package websites

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

// GenerateSiteID returns e.g. wp-a1b2c3d4 (unique internal identifier).
func GenerateSiteID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
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
	return nil
}
