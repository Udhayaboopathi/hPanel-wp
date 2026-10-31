package domains

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Udhayaboopathi/hPanel-wp/internal/security"
)

func Validate(domain string) error   { return security.ValidateDomain(domain) }
func Normalize(domain string) string { return security.NormalizeDomain(domain) }

// Cloudflare IPv4 ranges (published by Cloudflare; checked for proxy detection).
var cloudflareCIDRs = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

func IsCloudflareIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, c := range cloudflareCIDRs {
		_, cidr, err := net.ParseCIDR(c)
		if err == nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

type DNSResult struct {
	Domain         string
	Resolved       bool
	IPs            []string
	PointsToOrigin bool
	Cloudflare     bool
	Detail         string
}

func Lookup(ctx context.Context, domain, originIP string) DNSResult {
	r := DNSResult{Domain: domain}
	ips, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		r.Detail = "DNS lookup failed: " + err.Error()
		return r
	}
	r.IPs = ips
	r.Resolved = len(ips) > 0
	for _, ip := range ips {
		if ip == originIP {
			r.PointsToOrigin = true
		}
		if IsCloudflareIP(ip) {
			r.Cloudflare = true
		}
	}
	switch {
	case r.PointsToOrigin:
		r.Detail = "resolves directly to the origin server"
	case r.Cloudflare:
		r.Detail = "resolves to Cloudflare (proxied); origin is hidden, which is expected when the proxy is on"
	default:
		r.Detail = "resolves, but not to the origin IP " + originIP
	}
	return r
}

type SSLResult struct {
	Domain    string
	OK        bool
	Issuer    string
	Subject   string
	ExpiresAt time.Time
	DaysLeft  int
	Detail    string
}

func CheckTLS(ctx context.Context, domain string) SSLResult {
	r := SSLResult{Domain: domain}
	d := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(domain, "443"), &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12})
	if err != nil {
		r.Detail = fmt.Sprintf("TLS check failed: %v", err)
		return r
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		r.Detail = "no peer certificate presented"
		return r
	}
	cert := state.PeerCertificates[0]
	r.Issuer = cert.Issuer.CommonName
	if r.Issuer == "" && len(cert.Issuer.Organization) > 0 {
		r.Issuer = strings.Join(cert.Issuer.Organization, ", ")
	}
	r.Subject = cert.Subject.CommonName
	r.ExpiresAt = cert.NotAfter
	r.DaysLeft = int(time.Until(cert.NotAfter).Hours() / 24)
	r.OK = time.Now().Before(cert.NotAfter) && cert.VerifyHostname(domain) == nil
	if r.OK {
		r.Detail = "certificate valid"
	} else {
		r.Detail = "certificate expired or hostname mismatch"
	}
	return r
}
