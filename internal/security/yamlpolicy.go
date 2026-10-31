package security

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ApprovedImages is the closed allowlist of images the panel may run.
var ApprovedImages = []string{
	"wordpress:7.1.4-php8.3-apache",
	"wordpress:7.1.4-php8.2-apache",
	"wordpress:cli-2.12.0-php8.3",
	// Previous pins stay approved so existing sites' stored
	// configurations keep validating until they are regenerated.
	"wordpress:6.7.2-php8.3-apache",
	"wordpress:6.7.2-php8.2-apache",
	"wordpress:cli-2.11.0-php8.3",
	"mariadb:11.4",
}

func IsApprovedImage(img string) bool {
	for _, a := range ApprovedImages {
		if img == a {
			return true
		}
	}
	return false
}

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]any            `yaml:"networks"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	Image       string            `yaml:"image"`
	Build       any               `yaml:"build"`
	Privileged  bool              `yaml:"privileged"`
	NetworkMode string            `yaml:"network_mode"`
	Pid         string            `yaml:"pid"`
	Ipc         string            `yaml:"ipc"`
	CapAdd      []string          `yaml:"cap_add"`
	CapDrop     []string          `yaml:"cap_drop"`
	Devices     []string          `yaml:"devices"`
	Volumes     []string          `yaml:"volumes"`
	Ports       []any             `yaml:"ports"`
	Labels      map[string]string `yaml:"labels"`
	Environment map[string]string `yaml:"environment"`
	SecurityOpt []string          `yaml:"security_opt"`
	Container   string            `yaml:"container_name"`
	Networks    any               `yaml:"networks"`
}

// AllowedCapAdd is the tiny capability allowlist.
var AllowedCapAdd = map[string]bool{"CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "NET_BIND_SERVICE": true, "SETGID": true, "SETUID": true}

var hostRulePattern = regexp.MustCompile("Host\\(`([^`]+)`\\)")

// ValidateComposePolicy parses generated/edited compose YAML and rejects any
// dangerous Docker setting. siteDir is the only host path bind mounts may use;
// the site's identity is derived from it (sites live at <sites>/<site-id>).
// allowedHosts, when supplied, pins every Traefik Host() rule in the labels
// to the site's own domains, so an edited file cannot hijack another site's
// (or a production service's) route.
func ValidateComposePolicy(yamlText, siteDir string, allowedHosts ...string) error {
	if strings.TrimSpace(yamlText) == "" {
		return fmt.Errorf("empty compose configuration")
	}
	// Fast string-level tripwires in addition to structural checks.
	lower := strings.ToLower(yamlText)
	for _, bad := range []string{"/var/run/docker.sock", "docker.sock", "privileged: true"} {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("forbidden setting detected: %s", bad)
		}
	}
	var cf composeFile
	dec := yaml.NewDecoder(strings.NewReader(yamlText))
	dec.KnownFields(false)
	if err := dec.Decode(&cf); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	if len(cf.Services) == 0 {
		return fmt.Errorf("compose file defines no services")
	}
	if len(cf.Services) > 2 {
		return fmt.Errorf("compose file may define at most 2 services")
	}
	siteID := path.Base(strings.TrimSuffix(siteDir, "/"))
	// Top-level volumes are never part of a generated site file: a
	// named/external volume could reference another stack's storage.
	if len(cf.Volumes) > 0 {
		return fmt.Errorf("top-level named volumes are forbidden; site storage is a bind mount inside the site directory")
	}
	// Top-level networks: only the site's two generated entries, both
	// external with their exact generated names. An edited file must
	// not join arbitrary networks (another site's network, the stack's
	// database network) or invent new ones.
	for key, raw := range cf.Networks {
		if key != "site" && key != "traefik" {
			return fmt.Errorf("network %q is not one of the site's generated networks", key)
		}
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("network %q: malformed definition", key)
		}
		if m["external"] != true {
			return fmt.Errorf("network %q: must be external (the panel never creates shared networks from site files)", key)
		}
		want := "redberyl-internal"
		if key == "site" {
			want = "own-hpanel-net-" + siteID
		}
		if name, _ := m["name"].(string); name != want {
			return fmt.Errorf("network %q: name must be %q", key, want)
		}
	}
	hostSet := map[string]bool{}
	for _, h := range allowedHosts {
		if h != "" {
			hostSet[h] = true
		}
	}
	for name, svc := range cf.Services {
		if svc.Build != nil {
			return fmt.Errorf("service %s: build sections are forbidden; only approved prebuilt images may run", name)
		}
		if svc.Privileged {
			return fmt.Errorf("service %s: privileged mode is forbidden", name)
		}
		// Any namespace-mode value is rejected - not just "host":
		// "container:<name>" / "service:<name>" would share another
		// container's namespace just as surely.
		if svc.NetworkMode != "" || svc.Pid != "" || svc.Ipc != "" {
			return fmt.Errorf("service %s: network_mode/pid/ipc overrides are forbidden", name)
		}
		if len(svc.Devices) > 0 {
			return fmt.Errorf("service %s: devices are forbidden", name)
		}
		if len(svc.Ports) > 0 {
			return fmt.Errorf("service %s: published ports are forbidden; Traefik routes traffic", name)
		}
		if !IsApprovedImage(svc.Image) {
			return fmt.Errorf("service %s: image %q is not in the approved allowlist", name, svc.Image)
		}
		for _, c := range svc.CapAdd {
			if !AllowedCapAdd[strings.ToUpper(c)] {
				return fmt.Errorf("service %s: capability %s is not allowed", name, c)
			}
		}
		for _, so := range svc.SecurityOpt {
			if so != "no-new-privileges:true" && so != "no-new-privileges" {
				return fmt.Errorf("service %s: security_opt %q is forbidden (only no-new-privileges is allowed)", name, so)
			}
		}
		// The service may join only its two generated networks.
		for _, n := range serviceNetworks(svc.Networks) {
			if n != "site" && n != "traefik" {
				return fmt.Errorf("service %s: network %q is not one of the site's generated networks", name, n)
			}
		}
		for _, v := range svc.Volumes {
			src := v
			if i := strings.Index(v, ":"); i >= 0 {
				src = v[:i]
			}
			if !strings.HasPrefix(src, "/") {
				return fmt.Errorf("service %s: volume %q is a named or relative volume; only bind mounts inside the site directory are allowed", name, v)
			}
			if siteDir == "" || (src != siteDir && !strings.HasPrefix(src, siteDir+"/")) {
				return fmt.Errorf("service %s: bind mount %q is outside the site directory", name, src)
			}
			if strings.Contains(v, "docker.sock") {
				return fmt.Errorf("service %s: docker socket mount is forbidden", name)
			}
		}
		// Ownership guard: the labels the agent's container resolution
		// trusts must be present and must name THIS site - a forged
		// own-hpanel.site-id pointing at another site is rejected.
		if svc.Labels["own-hpanel.managed"] != "true" {
			return fmt.Errorf("service %s: missing own-hpanel.managed=true label", name)
		}
		if svc.Labels["own-hpanel.site-id"] != siteID {
			return fmt.Errorf("service %s: own-hpanel.site-id label must be %q (the site being edited)", name, siteID)
		}
		// Route hijack guard (when the caller supplied the site's
		// domains): every Traefik Host() rule must name one of them.
		if len(hostSet) > 0 {
			for _, v := range svc.Labels {
				for _, m := range hostRulePattern.FindAllStringSubmatch(v, -1) {
					if !hostSet[m[1]] {
						return fmt.Errorf("service %s: Traefik rule routes host %q, which is not one of this site's domains", name, m[1])
					}
				}
			}
		}
	}
	return nil
}

// serviceNetworks flattens a compose `networks:` value (list form or
// map form) into network names.
func serviceNetworks(v any) []string {
	switch n := v.(type) {
	case []any:
		out := []string{}
		for _, e := range n {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := []string{}
		for k := range n {
			out = append(out, k)
		}
		return out
	}
	return nil
}
