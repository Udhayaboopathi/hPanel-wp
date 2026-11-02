package configgen

import (
	"strings"
	"testing"
)

func TestTablePrefixEnvEmission(t *testing.T) {
	base := testSpec()
	if _, ok := EnvMap(base)["WORDPRESS_TABLE_PREFIX"]; ok {
		t.Error("default site must not emit WORDPRESS_TABLE_PREFIX")
	}
	wp := base
	wp.TablePrefix = "wp_"
	if _, ok := EnvMap(wp)["WORDPRESS_TABLE_PREFIX"]; ok {
		t.Error("explicit wp_ prefix must not emit WORDPRESS_TABLE_PREFIX")
	}
	custom := base
	custom.TablePrefix = "fc7_"
	if got := EnvMap(custom)["WORDPRESS_TABLE_PREFIX"]; got != "fc7_" {
		t.Errorf("custom prefix env: got %q want fc7_", got)
	}
	// The prefix reaches the container through the site .env file
	// (compose loads it via env_file), which GenerateEnv renders
	// from EnvMap.
	if env := GenerateEnv(custom); !strings.Contains(env, "WORDPRESS_TABLE_PREFIX=fc7_") {
		t.Errorf("generated .env does not carry the custom table prefix:\n%s", env)
	}
	if _, err := GenerateCompose(custom); err != nil {
		t.Fatal(err)
	}
}
