package main

import (
	"testing"

	"github.com/Udhayaboopathi/hPanel-wp/internal/store"
)

// The background sampler depends on parsing EVERY site line of an
// OpStats response, not just one.
func TestParseAllStats(t *testing.T) {
	out := "own-hpanel-wp-aaa site=wp-aaa cpu=12.5% mem=1048576/536870912\n" +
		"own-hpanel-wp-bbb site=wp-bbb cpu=0.0% mem=0/268435456\n" +
		"some unrelated line\n"
	all := parseAllStats(out)
	if len(all) != 2 {
		t.Fatalf("parsed %d sites, want 2", len(all))
	}
	if all["wp-aaa"].CPUPercent != 12.5 || all["wp-aaa"].MemUsage != 1048576 || all["wp-aaa"].MemLimit != 536870912 {
		t.Errorf("wp-aaa parsed wrong: %+v", all["wp-aaa"])
	}
	if got := parseSiteStats(out, "wp-bbb"); got == nil || got.CPUPercent != 0 {
		t.Errorf("parseSiteStats regression: %+v", got)
	}
	if got := parseSiteStats(out, "wp-missing"); got != nil {
		t.Errorf("missing site should be nil, got %+v", got)
	}
}

// Chart bars normalize memory against the limit and clamp heights.
func TestMakeBars(t *testing.T) {
	samples := []store.ResourceSample{
		{CreatedAt: "t1", CPUPercent: 50, MemBytes: 256},
		{CreatedAt: "t2", CPUPercent: 250, MemBytes: 2048},
	}
	bars := makeBars(samples, 1024)
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	if bars[0].CPUBH != 50 || bars[0].MemBH != 25 {
		t.Errorf("bar0 = %+v, want CPU 50 / Mem 25", bars[0])
	}
	if bars[1].CPUBH != 100 || bars[1].MemBH != 100 {
		t.Errorf("bar1 should clamp to 100/100, got %+v", bars[1])
	}
	if got := makeBars(samples, 0); got[0].MemPct != 0 {
		t.Errorf("zero limit must not divide by zero, MemPct=%v", got[0].MemPct)
	}
}
