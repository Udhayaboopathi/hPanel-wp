package configgen

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

type Profile struct {
	Name      string
	CPUs      float64
	MemMB     int
	ReserveMB int
}

var Profiles = map[string]Profile{
	"lightweight": {Name: "lightweight", CPUs: 0.5, MemMB: 512, ReserveMB: 128},
	"standard":    {Name: "standard", CPUs: 1.0, MemMB: 768, ReserveMB: 192},
	"custom":      {Name: "custom", CPUs: 0.5, MemMB: 512, ReserveMB: 128},
}

func ProfileFor(name string, cpu float64, memMB int) (Profile, error) {
	p, ok := Profiles[name]
	if !ok {
		return p, fmt.Errorf("unknown resource profile %q", name)
	}
	if name == "custom" {
		p.CPUs, p.MemMB = cpu, memMB
	}
	if err := ValidateResources(p.CPUs, p.MemMB); err != nil {
		return p, err
	}
	return p, nil
}

func ValidateResources(cpu float64, memMB int) error {
	// NaN fails every comparison below and would otherwise sail
	// through into a generated "NaN" CPU value; the stated
	// minimum is 0.1, not "anything above zero".
	if math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0.1 || cpu > 4 {
		return fmt.Errorf("cpu limit must be between 0.1 and 4")
	}
	if memMB < 256 || memMB > 4096 {
		return fmt.Errorf("memory limit must be between 256 MB and 4096 MB")
	}
	return nil
}

// PHPImage maps a verified PHP version to the pinned WordPress image.
func PHPImage(php string) (string, error) {
	switch php {
	case "8.3":
		return "wordpress:7.1.4-php8.3-apache", nil
	case "8.2":
		return "wordpress:7.1.4-php8.2-apache", nil
	default:
		return "", fmt.Errorf("PHP version %q is not verified compatible", php)
	}
}

func WPVersion() string { return "7.1.4" }

type SiteSpec struct {
	SiteID        string
	Domain        string
	AdminDomain   string
	Title         string
	DBName        string
	DBUser        string
	DBPassword    string
	DBHost        string
	AdminUser     string
	AdminEmail    string
	PHPVersion    string
	Profile       Profile
	SiteDir       string // host path, e.g. /opt/own-hpanel/websites/wp-xxxx
	ContainerName string
	Network       string // per-site network
	PHPMemory     string
	UploadSize    string
	// PostMax is the PHP post_max_size; empty means "same as
	// UploadSize" (the historical behaviour). The env editor
	// exposes POST_MAX_SIZE separately, so it needs its own field —
	// without one the editor's value was saved and silently
	// ignored on apply.
	PostMax string
	Debug   bool
	// TablePrefix is the WordPress table prefix for the site ("" or
	// "wp_" = default). Set for imported sites whose database dump
	// uses a custom prefix; emitted as WORDPRESS_TABLE_PREFIX.
	TablePrefix string
}

// TraefikLabels returns the exact label set for a site container.
// No secrets ever appear in labels.
func TraefikLabels(s SiteSpec) map[string]string {
	router := "own-hpanel-" + s.SiteID
	labels := map[string]string{
		"own-hpanel.managed":                                            "true",
		"own-hpanel.site-id":                                            s.SiteID,
		"own-hpanel.resource":                                           "wordpress",
		"own-hpanel.domain":                                             s.Domain,
		"traefik.enable":                                                "true",
		"traefik.docker.network":                                        "redberyl-internal",
		"traefik.http.routers." + router + ".rule":                      "Host(`" + s.Domain + "`)",
		"traefik.http.routers." + router + ".entrypoints":               "websecure",
		"traefik.http.routers." + router + ".tls":                       "true",
		"traefik.http.routers." + router + ".tls.certresolver":          "letsencrypt",
		"traefik.http.routers." + router + ".service":                   router,
		"traefik.http.services." + router + ".loadbalancer.server.port": "80",
	}
	if s.AdminDomain != "" {
		ar := router + "-admin"
		labels["traefik.http.routers."+ar+".rule"] = "Host(`" + s.AdminDomain + "`)"
		labels["traefik.http.routers."+ar+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+ar+".tls"] = "true"
		labels["traefik.http.routers."+ar+".tls.certresolver"] = "letsencrypt"
		labels["traefik.http.routers."+ar+".middlewares"] = ar + "-redir"
		// Redirect-only: admin alias never serves WP login independently (v1).
		labels["traefik.http.middlewares."+ar+"-redir.redirectregex.regex"] = "^https://" + s.AdminDomain + "/(.*)$"
		labels["traefik.http.middlewares."+ar+"-redir.redirectregex.replacement"] = "https://" + s.Domain + "/wp-admin/"
		labels["traefik.http.middlewares."+ar+"-redir.redirectregex.permanent"] = "false"
		labels["traefik.http.routers."+ar+".service"] = ar
		labels["traefik.http.services."+ar+".loadbalancer.server.port"] = "80"
	}
	return labels
}

// EnvMap is the approved, editable environment schema for a site.
func EnvMap(s SiteSpec) map[string]string {
	debug := "0"
	if s.Debug {
		debug = "1"
	}
	phpMem := s.PHPMemory
	if phpMem == "" {
		phpMem = "256M"
	}
	upload := s.UploadSize
	if upload == "" {
		upload = "64M"
	}
	postMax := s.PostMax
	if postMax == "" {
		postMax = upload
	}
	m := map[string]string{
		"WORDPRESS_DB_HOST":     s.DBHost,
		"WORDPRESS_DB_NAME":     s.DBName,
		"WORDPRESS_DB_USER":     s.DBUser,
		"WORDPRESS_DB_PASSWORD": s.DBPassword,
		"WORDPRESS_DEBUG":       debug,
		"PHP_MEMORY_LIMIT":      phpMem,
		"UPLOAD_MAX_FILESIZE":   upload,
		"POST_MAX_SIZE":         postMax,
		"SITE_DOMAIN":           s.Domain,
		"SITE_TITLE":            s.Title,
	}
	if s.TablePrefix != "" && s.TablePrefix != "wp_" {
		m["WORDPRESS_TABLE_PREFIX"] = s.TablePrefix
	}
	return m
}

// GenerateEnv renders a .env file (0600 on disk; values are secrets - never log).
func GenerateEnv(s SiteSpec) string {
	m := EnvMap(s)
	// Defence in depth: no value may ever inject extra lines into the
	// generated file (a title with a newline would otherwise smuggle
	// in attacker-chosen keys). Inputs are validated upstream too.
	for k, v := range m {
		m[k] = strings.ReplaceAll(strings.ReplaceAll(v, "\r", " "), "\n", " ")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Generated by Own hPanel for site " + s.SiteID + " - do not edit secrets manually\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, m[k])
	}
	return b.String()
}

// GenerateCompose renders the approved compose YAML for one site.
func GenerateCompose(s SiteSpec) (string, error) {
	img, err := PHPImage(s.PHPVersion)
	if err != nil {
		return "", err
	}
	labels := TraefikLabels(s)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&lb, "      %s: %q\n", k, labels[k])
	}
	dataDir := s.SiteDir + "/data"
	yaml := fmt.Sprintf(`services:
  wordpress:
    image: %s
    container_name: %s
    restart: unless-stopped
    env_file:
      - .env
    environment:
      WORDPRESS_DB_HOST: %s
      WORDPRESS_DB_NAME: %s
      WORDPRESS_DB_USER: %s
    volumes:
      - %s:/var/www/html
    networks:
      - site
      - traefik
    labels:
%s    deploy:
      resources:
        limits:
          cpus: "%.2f"
          memory: %dM
        reservations:
          memory: %dM
    mem_limit: %dm
    mem_reservation: %dm
    cpus: %.2f
    cap_drop:
      - ALL
    cap_add:
      - CHOWN
      - DAC_OVERRIDE
      - FOWNER
      - NET_BIND_SERVICE
      - SETGID
      - SETUID
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "php", "-r", "exit(@fsockopen('127.0.0.1', 80) ? 0 : 1);"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 40s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
networks:
  site:
    name: %s
    external: true
  traefik:
    name: redberyl-internal
    external: true
`, img, s.ContainerName, s.DBHost, s.DBName, s.DBUser, dataDir, lb.String(),
		s.Profile.CPUs, s.Profile.MemMB, s.Profile.ReserveMB,
		s.Profile.MemMB, s.Profile.ReserveMB, s.Profile.CPUs, s.Network)
	if err := security.ValidateComposePolicy(yaml, s.SiteDir, s.Domain, s.AdminDomain); err != nil {
		return "", fmt.Errorf("generated compose failed policy validation: %w", err)
	}
	return yaml, nil
}

// DNSRecords returns the manual Cloudflare records for a domain.
// The names are the site's actual fully-qualified domains: the
// previous "@"/"wpadmin" pair was only correct for an apex domain
// whose admin alias is exactly wpadmin.<apex> — for a subdomain
// site (cloud.example.com) it instructed records for the wrong
// names entirely.
func DNSRecords(domain, serverIP string) [][3]string {
	return [][3]string{
		{"A", domain, serverIP},
		{"A", "wpadmin." + domain, serverIP},
	}
}
