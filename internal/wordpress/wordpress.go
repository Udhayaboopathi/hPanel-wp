// Package wordpress builds WP-CLI invocations as argument slices.
// Commands are never assembled into a shell string from user input.
package wordpress

import (
	"fmt"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`^[a-z0-9\-_]+$`)

// Command is an allowlisted WP-CLI operation.
type Command struct {
	Name string
	Args []string
}

func validateSlug(s string) error {
	if !slugRe.MatchString(s) {
		return fmt.Errorf("invalid plugin/theme slug %q", s)
	}
	return nil
}

// Build returns argv (starting with "wp") for an allowlisted action.
func Build(action string, arg string) (Command, error) {
	switch action {
	case "core_version":
		return Command{action, []string{"wp", "core", "version", "--allow-root"}}, nil
	case "core_check_update":
		return Command{action, []string{"wp", "core", "check-update", "--allow-root"}}, nil
	case "core_update":
		return Command{action, []string{"wp", "core", "update", "--allow-root"}}, nil
	case "plugin_list":
		return Command{action, []string{"wp", "plugin", "list", "--format=json", "--allow-root"}}, nil
	case "plugin_update_all":
		return Command{action, []string{"wp", "plugin", "update", "--all", "--allow-root"}}, nil
	case "theme_list":
		return Command{action, []string{"wp", "theme", "list", "--format=json", "--allow-root"}}, nil
	case "theme_update_all":
		return Command{action, []string{"wp", "theme", "update", "--all", "--allow-root"}}, nil
	case "cache_flush":
		return Command{action, []string{"wp", "cache", "flush", "--allow-root"}}, nil
	case "health":
		return Command{action, []string{"wp", "core", "verify-checksums", "--allow-root"}}, nil
	case "plugin_activate", "plugin_deactivate", "plugin_update":
		if err := validateSlug(arg); err != nil {
			return Command{}, err
		}
		verb := strings.TrimPrefix(action, "plugin_")
		return Command{action, []string{"wp", "plugin", verb, arg, "--allow-root"}}, nil
	default:
		return Command{}, fmt.Errorf("WP-CLI action %q is not allowlisted", action)
	}
}

// InstallArgs builds `wp core install` argv. The admin password is passed
// via argv to the one-shot WP-CLI container only; it is never logged by callers.
func InstallArgs(url, title, adminUser, adminPassword, adminEmail string) []string {
	return []string{"wp", "core", "install",
		"--url=" + url, "--title=" + title,
		"--admin_user=" + adminUser, "--admin_password=" + adminPassword,
		"--admin_email=" + adminEmail, "--skip-email", "--allow-root"}
}

func ResetPasswordArgs(user, newPassword string) []string {
	return []string{"wp", "user", "reset-password", user, "--new-pass=" + newPassword, "--skip-email", "--allow-root"}
}

// SearchReplaceArgs is serialization-safe URL replacement for migrations.
func SearchReplaceArgs(oldURL, newURL string) []string {
	return []string{"wp", "search-replace", oldURL, newURL, "--all-tables", "--precise", "--allow-root"}
}
