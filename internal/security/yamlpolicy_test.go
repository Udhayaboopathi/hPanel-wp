package security

import "testing"

// goodCompose mirrors the REAL generated shape (configgen.GenerateCompose):
// two external networks with pinned names, ownership labels for THIS site,
// one bind mount inside the site dir, no-new-privileges only.
const goodCompose = `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    container_name: own-hpanel-wp-test
    labels:
      own-hpanel.managed: "true"
      own-hpanel.site-id: "wp-test"
    volumes:
      - /opt/own-hpanel/websites/wp-test/data:/var/www/html
    networks:
      - site
      - traefik
    security_opt:
      - no-new-privileges:true
networks:
  site:
    name: own-hpanel-net-wp-test
    external: true
  traefik:
    name: redberyl-internal
    external: true
`

func TestPolicyAcceptsGeneratedShape(t *testing.T) {
	if err := ValidateComposePolicy(goodCompose, "/opt/own-hpanel/websites/wp-test"); err != nil {
		t.Fatalf("expected valid compose: %v", err)
	}
}

func TestPolicyRejectsDangerous(t *testing.T) {
	cases := map[string]string{
		"privileged": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    privileged: true
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
`,
		"host network": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    network_mode: host
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
`,
		"namespace sharing": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    network_mode: "container:own-hpanel-agent"
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
`,
		"docker sock": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    volumes: ["/var/run/docker.sock:/var/run/docker.sock"]
`,
		"host bind": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    volumes: ["/etc:/etc"]
`,
		"named volume": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    volumes: ["other-stack-data:/var/www/html"]
`,
		"bad image": `services:
  wordpress:
    image: evil/miner:latest
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
`,
		"published port": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    ports: ["8080:80"]
`,
		"cap add": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    cap_add: ["SYS_ADMIN"]
`,
		"missing label": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
`,
		"forged site id": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-victim"}
`,
		"device": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    devices: ["/dev/sda:/dev/sda"]
`,
		"seccomp unconfined": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    security_opt: ["seccomp=unconfined"]
`,
		"build section": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    build: .
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
`,
		"foreign network join": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
    networks: ["own-hpanel-db"]
`,
		"top-level named volume": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
volumes:
  shared:
    external: true
`,
		"renamed traefik network": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true", own-hpanel.site-id: "wp-test"}
networks:
  traefik:
    name: some-other-network
    external: true
`,
	}
	for name, y := range cases {
		if err := ValidateComposePolicy(y, "/opt/own-hpanel/websites/wp-test"); err == nil {
			t.Errorf("case %q: expected rejection", name)
		}
	}
}

// With the site's domains supplied, a Traefik rule for any other host
// (route hijack) is rejected; the site's own hosts pass.
func TestPolicyPinsTraefikHosts(t *testing.T) {
	hijack := "services:\n  wordpress:\n    image: wordpress:6.7.2-php8.3-apache\n    labels:\n      own-hpanel.managed: \"true\"\n      own-hpanel.site-id: \"wp-test\"\n      traefik.http.routers.r.rule: \"Host(`krwebsolution.com`)\"\n"
	if err := ValidateComposePolicy(hijack, "/opt/own-hpanel/websites/wp-test", "example.com", "wpadmin.example.com"); err == nil {
		t.Fatal("expected route-hijack rejection")
	}
	own := "services:\n  wordpress:\n    image: wordpress:6.7.2-php8.3-apache\n    labels:\n      own-hpanel.managed: \"true\"\n      own-hpanel.site-id: \"wp-test\"\n      traefik.http.routers.r.rule: \"Host(`example.com`)\"\n"
	if err := ValidateComposePolicy(own, "/opt/own-hpanel/websites/wp-test", "example.com", "wpadmin.example.com"); err != nil {
		t.Fatalf("own host must pass: %v", err)
	}
}
