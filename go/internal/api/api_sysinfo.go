package api

import (
	"net"
	"net/http"
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// diskRoot returns the filesystem root to probe for disk usage. Unix-like
// systems use "/"; Windows uses the drive letter from %SystemDrive% (the
// `make release` target ships a windows/amd64 build, so this path matters).
func diskRoot() string {
	if runtime.GOOS == "windows" {
		if sd := os.Getenv("SystemDrive"); sd != "" {
			return sd + `\`
		}
		return `C:\`
	}
	return "/"
}

type sysInfoCPU struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type sysInfoMemory struct {
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	Percent    float64 `json:"percent"`
}

type sysInfoDisk struct {
	Path       string  `json:"path"`
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	Percent    float64 `json:"percent"`
}

type sysInfoIface struct {
	Iface string   `json:"iface"`
	IPs   []string `json:"ips"`
}

type sysInfoResponse struct {
	Hostname string         `json:"hostname"`
	UptimeS  uint64         `json:"uptime_s"`
	CPU      sysInfoCPU     `json:"cpu"`
	Memory   sysInfoMemory  `json:"memory"`
	Disk     sysInfoDisk    `json:"disk"`
	Network  []sysInfoIface `json:"network"`
}

// handleSysInfo returns a snapshot of the host's primary resources for the
// settings → System tab. CPU sample uses gopsutil's cached delta (interval=0)
// so the call doesn't block; the frontend polls every 5 s, which gives the
// delta a meaningful window between reads.
func (s *Server) handleSysInfo(w http.ResponseWriter, r *http.Request) {
	root := diskRoot()
	resp := sysInfoResponse{
		CPU:  sysInfoCPU{Cores: runtime.NumCPU()},
		Disk: sysInfoDisk{Path: root},
	}

	if hi, err := host.InfoWithContext(r.Context()); err == nil {
		resp.Hostname = hi.Hostname
		resp.UptimeS = hi.Uptime
	}

	if pcts, err := cpu.PercentWithContext(r.Context(), 0, false); err == nil && len(pcts) > 0 {
		resp.CPU.Percent = pcts[0]
	}

	if vm, err := mem.VirtualMemoryWithContext(r.Context()); err == nil {
		resp.Memory = sysInfoMemory{
			TotalBytes: vm.Total,
			UsedBytes:  vm.Used,
			Percent:    vm.UsedPercent,
		}
	}

	if du, err := disk.UsageWithContext(r.Context(), root); err == nil {
		resp.Disk = sysInfoDisk{
			Path:       du.Path,
			TotalBytes: du.Total,
			UsedBytes:  du.Used,
			Percent:    du.UsedPercent,
		}
	}

	resp.Network = collectNonLoopbackIPs()

	writeJSON(w, http.StatusOK, resp)
}

// collectNonLoopbackIPs returns one entry per UP, non-loopback interface
// that holds at least one IPv4 or IPv6 address. Link-local IPv6 (fe80::/10)
// is included — useful for diagnosing mDNS/discovery issues.
func collectNonLoopbackIPs() []sysInfoIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]sysInfoIface, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		ips := make([]string, 0, len(addrs))
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip == nil || ip.IsLoopback() {
				continue
			}
			ips = append(ips, ip.String())
		}
		if len(ips) == 0 {
			continue
		}
		out = append(out, sysInfoIface{Iface: ifi.Name, IPs: ips})
	}
	return out
}
