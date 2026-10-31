package monitoring

import "testing"

func TestSampleAndAlerts(t *testing.T) {
	h := Sample("/")
	if h.MemTotal == 0 {
		t.Error("expected real memory total from /proc")
	}
	// Disk of this machine must be readable.
	if h.DiskTotal == 0 {
		t.Error("expected real disk total")
	}
	alerts := Alerts(HostStats{DiskTotal: 100, DiskFree: 5, MemTotal: 100, MemAvail: 50})
	found := false
	for _, a := range alerts {
		if a != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected a low-disk alert")
	}
}
