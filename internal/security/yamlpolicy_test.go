package security

import "testing"

const goodCompose = `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    container_name: own-hpanel-wp-test
    labels:
      own-hpanel.managed: "true"
    volumes:
      - /opt/own-hpanel/websites/wp-test/data:/var/www/html
networks:
  traefik:
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
    labels: {own-hpanel.managed: "true"}
`,
		"host network": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    network_mode: host
    labels: {own-hpanel.managed: "true"}
`,
		"docker sock": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true"}
    volumes: ["/var/run/docker.sock:/var/run/docker.sock"]
`,
		"host bind": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true"}
    volumes: ["/etc:/etc"]
`,
		"bad image": `services:
  wordpress:
    image: evil/miner:latest
    labels: {own-hpanel.managed: "true"}
`,
		"published port": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true"}
    ports: ["8080:80"]
`,
		"cap add": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true"}
    cap_add: ["SYS_ADMIN"]
`,
		"missing label": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
`,
		"device": `services:
  wordpress:
    image: wordpress:6.7.2-php8.3-apache
    labels: {own-hpanel.managed: "true"}
    devices: ["/dev/sda:/dev/sda"]
`,
	}
	for name, y := range cases {
		if err := ValidateComposePolicy(y, "/opt/own-hpanel/websites/wp-test"); err == nil {
			t.Errorf("case %q: expected rejection", name)
		}
	}
}
