package monitoring

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type HostStats struct {
	CPUPercent float64
	MemTotal   uint64
	MemAvail   uint64
	MemUsed    uint64
	DiskTotal  uint64
	DiskFree   uint64
	DiskUsed   uint64
	Load1      float64
	UptimeSec  uint64
	SampledAt  time.Time
}

var (
	mu      sync.Mutex
	lastCPU cpuTimes
	lastAt  time.Time
	cached  HostStats
	cacheAt time.Time
)

type cpuTimes struct{ total, idle uint64 }

func readCPU() cpuTimes {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)[1:]
			var total, idle uint64
			for i, v := range fields {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
				if i == 3 || i == 4 { // idle + iowait
					idle += n
				}
			}
			return cpuTimes{total, idle}
		}
	}
	return cpuTimes{}
}

func readMem() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 {
			v, _ := strconv.ParseUint(parts[1], 10, 64)
			switch parts[0] {
			case "MemTotal:":
				total = v * 1024
			case "MemAvailable:":
				avail = v * 1024
			}
		}
	}
	return
}

func readLoad() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		return v
	}
	return 0
}

func Disk(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = st.Blocks * uint64(st.Bsize)
	free = st.Bavail * uint64(st.Bsize)
	return
}

// Sample returns cached host stats (5s cache bounds /proc reads).
func Sample(diskPath string) HostStats {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(cacheAt) < 5*time.Second && !cached.SampledAt.IsZero() {
		return cached
	}
	cur := readCPU()
	var pct float64
	if !lastAt.IsZero() && cur.total > lastCPU.total {
		dTotal := float64(cur.total - lastCPU.total)
		dIdle := float64(cur.idle - lastCPU.idle)
		if dTotal > 0 {
			pct = 100 * (1 - dIdle/dTotal)
		}
	}
	lastCPU, lastAt = cur, time.Now()
	mt, ma := readMem()
	dt, df := Disk(diskPath)
	var up uint64
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			up = uint64(v)
		}
	}
	cached = HostStats{
		CPUPercent: pct, MemTotal: mt, MemAvail: ma, MemUsed: mt - ma,
		DiskTotal: dt, DiskFree: df, DiskUsed: dt - df,
		Load1: readLoad(), UptimeSec: up, SampledAt: time.Now(),
	}
	cacheAt = time.Now()
	return cached
}

// Alerts derives simple operator alerts from host stats.
func Alerts(h HostStats) []string {
	var out []string
	if h.DiskTotal > 0 && float64(h.DiskFree)/float64(h.DiskTotal) < 0.15 {
		out = append(out, "Low disk space: less than 15% free")
	}
	if h.MemTotal > 0 && float64(h.MemAvail)/float64(h.MemTotal) < 0.10 {
		out = append(out, "High memory usage: less than 10% available")
	}
	return out
}
