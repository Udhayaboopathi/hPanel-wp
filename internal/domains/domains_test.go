package domains

import "testing"

func TestCloudflareDetection(t *testing.T) {
	if !IsCloudflareIP("104.16.0.10") {
		t.Error("104.16.0.10 is a Cloudflare IP")
	}
	if !IsCloudflareIP("172.64.5.5") {
		t.Error("172.64.5.5 is a Cloudflare IP")
	}
	if IsCloudflareIP("130.210.42.20") {
		t.Error("origin IP must not be detected as Cloudflare")
	}
	if IsCloudflareIP("not-an-ip") {
		t.Error("invalid IP must not be Cloudflare")
	}
}
