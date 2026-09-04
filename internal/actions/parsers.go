package actions

import (
	"regexp"
	"strconv"
	"strings"
)

// A parser turns raw stdout into a JSON-able structure the UI renders
// directly. Parsers are pure functions of the text; they never talk to the
// host.
//
// Principle: prefer machine-readable output where it exists, and where it
// doesn't, parse the plainest format the tool offers (--plain, -P, -H) rather
// than the human-decorated one.
type parser func(stdout string, exitCode int) any

var parsers = map[string]parser{
	"raw":           parseRaw,
	"lines":         parseLines,
	"exit":          parseExit,
	"overview":      parseOverview,
	"systemd_units": parseSystemdUnits,
	"ps":            parsePS,
	"ss":            parseSS,
	"du":            parseDU,
}

func parseRaw(out string, _ int) any { return map[string]any{"text": out} }

func parseLines(out string, _ int) any {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = []string{}
	}
	return map[string]any{"lines": lines}
}

func parseExit(out string, code int) any {
	return map[string]any{"ok": code == 0, "text": strings.TrimSpace(out)}
}

// --- overview ---------------------------------------------------------------

type Disk struct {
	Source  string `json:"source"`
	Mount   string `json:"mount"`
	Total   int64  `json:"total"` // bytes
	Used    int64  `json:"used"`
	Avail   int64  `json:"avail"`
	UsedPct int    `json:"usedPct"`
}

type Overview struct {
	Now            int64   `json:"now"`
	UptimeSeconds  int64   `json:"uptimeSeconds"`
	Load           []float64 `json:"load"`
	CPUs           int     `json:"cpus"`
	MemTotal       int64   `json:"memTotal"` // bytes
	MemUsed        int64   `json:"memUsed"`
	SwapTotal      int64   `json:"swapTotal"`
	SwapUsed       int64   `json:"swapUsed"`
	Disks          []Disk  `json:"disks"`
	Users          int     `json:"users"`
	RebootRequired bool    `json:"rebootRequired"`
}

// pseudoFS are filesystems nobody wants on an overview card.
var pseudoFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true,
	"udev": true, "efivarfs": true, "cgroup": true, "cgroup2": true,
	"proc": true, "sysfs": true, "devpts": true, "shm": true, "none": true,
}

func parseOverview(out string, _ int) any {
	o := Overview{Load: []float64{}, Disks: []Disk{}}
	var memAvail, swapFree int64

	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "now":
			o.Now = atoi64(v)
		case "uptime_s":
			o.UptimeSeconds = atoi64(v)
		case "cpus":
			o.CPUs = int(atoi64(v))
		case "load":
			for _, f := range strings.Fields(v) {
				if x, err := strconv.ParseFloat(f, 64); err == nil {
					o.Load = append(o.Load, x)
				}
			}
		case "mem_total_kb":
			o.MemTotal = atoi64(v) * 1024
		case "mem_avail_kb":
			memAvail = atoi64(v) * 1024
		case "swap_total_kb":
			o.SwapTotal = atoi64(v) * 1024
		case "swap_free_kb":
			swapFree = atoi64(v) * 1024
		case "users":
			o.Users = int(atoi64(v))
		case "reboot_required":
			o.RebootRequired = v == "1"
		case "disk":
			// source|total_kb|used_kb|avail_kb|mount
			p := strings.Split(v, "|")
			if len(p) != 5 {
				continue
			}
			src := p[0]
			if pseudoFS[src] || strings.HasPrefix(src, "/dev/loop") {
				continue
			}
			d := Disk{
				Source: src, Mount: p[4],
				Total: atoi64(p[1]) * 1024,
				Used:  atoi64(p[2]) * 1024,
				Avail: atoi64(p[3]) * 1024,
			}
			if d.Total > 0 {
				d.UsedPct = int(d.Used * 100 / d.Total)
			}
			o.Disks = append(o.Disks, d)
		}
	}
	if o.MemTotal > 0 {
		o.MemUsed = o.MemTotal - memAvail
	}
	if o.SwapTotal > 0 {
		o.SwapUsed = o.SwapTotal - swapFree
	}
	return o
}

// --- systemd ----------------------------------------------------------------

type Unit struct {
	Name        string `json:"name"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// parseSystemdUnits reads `list-units --plain --no-legend` lines:
//
//	nginx.service loaded active running A high performance web server
func parseSystemdUnits(out string, _ int) any {
	units := []Unit{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		u := Unit{Name: f[0], Load: f[1], Active: f[2], Sub: f[3]}
		if len(f) > 4 {
			u.Description = strings.Join(f[4:], " ")
		}
		units = append(units, u)
	}
	return map[string]any{"units": units}
}

// --- ps ---------------------------------------------------------------------

type Process struct {
	PID     int     `json:"pid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"` // bytes
	Elapsed int64   `json:"elapsed"`
	Command string  `json:"command"`
}

// parsePS reads `ps -eo pid,user:20,pcpu,pmem,rss,etimes,comm --no-headers`.
func parsePS(out string, _ int) any {
	procs := []Process{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		cpu, _ := strconv.ParseFloat(f[2], 64)
		mem, _ := strconv.ParseFloat(f[3], 64)
		procs = append(procs, Process{
			PID:     int(atoi64(f[0])),
			User:    f[1],
			CPU:     cpu,
			Mem:     mem,
			RSS:     atoi64(f[4]) * 1024,
			Elapsed: atoi64(f[5]),
			Command: strings.Join(f[6:], " "),
		})
	}
	return map[string]any{"processes": procs}
}

// --- ss ---------------------------------------------------------------------

type Listener struct {
	Proto   string `json:"proto"`
	Local   string `json:"local"`
	Port    int    `json:"port"`
	Process string `json:"process"`
	PID     int    `json:"pid"`
}

// users:(("nginx",pid=1234,fd=6),("nginx",pid=1235,fd=6))
var reSSProc = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// parseSS reads `ss -tulpnH`:
//
//	tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=1234,fd=6))
func parseSS(out string, _ int) any {
	ls := []Listener{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		l := Listener{Proto: f[0], Local: f[4]}
		if i := strings.LastIndex(f[4], ":"); i >= 0 {
			l.Port = int(atoi64(f[4][i+1:]))
		}
		if len(f) > 6 {
			if m := reSSProc.FindStringSubmatch(f[6]); m != nil {
				l.Process = m[1]
				l.PID = int(atoi64(m[2]))
			}
		}
		ls = append(ls, l)
	}
	return map[string]any{"listeners": ls}
}

// --- du ---------------------------------------------------------------------

type DirSize struct {
	Path string `json:"path"`
	Size int64  `json:"size"` // bytes
}

func parseDU(out string, _ int) any {
	dirs := []DirSize{}
	for _, line := range strings.Split(out, "\n") {
		size, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		dirs = append(dirs, DirSize{Path: path, Size: atoi64(size) * 1024})
	}
	return map[string]any{"dirs": dirs}
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
