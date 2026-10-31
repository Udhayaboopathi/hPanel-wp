package wordpress

import (
	"strings"
	"testing"
)

func TestAllowlistedCommands(t *testing.T) {
	c, err := Build("core_version", "")
	if err != nil || strings.Join(c.Args, " ") != "wp core version --allow-root" {
		t.Fatalf("unexpected command %+v %v", c, err)
	}
	if _, err := Build("shell", ""); err == nil {
		t.Error("arbitrary shell action must be rejected")
	}
	if _, err := Build("plugin_activate", "akismet; rm -rf /"); err == nil {
		t.Error("injected plugin slug must be rejected")
	}
	c, err = Build("plugin_activate", "akismet")
	if err != nil || c.Args[3] != "akismet" {
		t.Fatalf("unexpected plugin command %+v %v", c, err)
	}
	args := InstallArgs("https://example.com", "T", "admin", "pw123456789012", "a@example.com")
	if !strings.Contains(strings.Join(args, " "), "--url=https://example.com") {
		t.Error("install args missing url")
	}
}
