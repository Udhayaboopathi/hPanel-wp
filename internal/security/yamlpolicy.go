package security

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ApprovedImages is the closed allowlist of images the panel may run.
var ApprovedImages = []string{
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
}

// AllowedCapAdd is the tiny capability allowlist.
var AllowedCapAdd = map[string]bool{"CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "NET_BIND_SERVICE": true, "SETGID": true, "SETUID": true}

// ValidateComposePolicy parses generated/edited compose YAML and rejects any
// dangerous Docker setting. siteDir is the only host path bind mounts may use.
func ValidateComposePolicy(yamlText, siteDir string) error {
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
	for name, svc := range cf.Services {
		if svc.Privileged {
			return fmt.Errorf("service %s: privileged mode is forbidden", name)
		}
		if svc.NetworkMode == "host" || svc.Pid == "host" || svc.Ipc == "host" {
			return fmt.Errorf("service %s: host network/pid/ipc is forbidden", name)
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
		for _, v := range svc.Volumes {
			src := v
			if i := strings.Index(v, ":"); i >= 0 {
				src = v[:i]
			}
			if strings.HasPrefix(src, "/") {
				if siteDir == "" || (src != siteDir && !strings.HasPrefix(src, siteDir+"/")) {
					return fmt.Errorf("service %s: bind mount %q is outside the site directory", name, src)
				}
			}
			if strings.Contains(v, "docker.sock") {
				return fmt.Errorf("service %s: docker socket mount is forbidden", name)
			}
		}
		// Ownership guard: any service the panel applies must be labelled.
		if svc.Labels["own-hpanel.managed"] != "true" {
			return fmt.Errorf("service %s: missing own-hpanel.managed=true label", name)
		}
	}
	return nil
}
