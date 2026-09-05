package actions

import (
	"encoding/json"
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
	"supervisor":    parseSupervisor,
	"docker_ps":     parseDockerPS,
	"inodes":        parseInodes,
	"tls":           parseTLS,
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
	Now            int64     `json:"now"`
	UptimeSeconds  int64     `json:"uptimeSeconds"`
	Load           []float64 `json:"load"`
	CPUs           int       `json:"cpus"`
	MemTotal       int64     `json:"memTotal"` // bytes
	MemUsed        int64     `json:"memUsed"`
	SwapTotal      int64     `json:"swapTotal"`
	SwapUsed       int64     `json:"swapUsed"`
	Disks          []Disk    `json:"disks"`
	Users          int       `json:"users"`
	RebootRequired bool      `json:"rebootRequired"`
}

// pseudoFS are filesystems nobody wants on an overview card.
var pseudoFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true,
	"udev": true, "efivarfs": true, "cgroup": true, "cgroup2": true,
	"proc": true, "sysfs": true, "devpts": true, "shm": true, "none": true,
}

func parseOverview(out string, _ int) any {
	o := Overview{Load: []float64{}, Disks: []Disk{}}
	var memAvail, memFree, memBuffers, memCached, swapFree int64

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
		case "mem_free_kb":
			memFree = atoi64(v) * 1024
		case "mem_buffers_kb":
			memBuffers = atoi64(v) * 1024
		case "mem_cached_kb":
			memCached = atoi64(v) * 1024
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
		if memAvail == 0 {
			// Kernels before 3.14 have no MemAvailable; approximate the way
			// `free` used to.
			memAvail = memFree + memBuffers + memCached
		}
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
		if len(procs) >= 60 {
			break // ps already sorted by CPU; the UI shows the top 60
		}
	}
	return map[string]any{"processes": procs}
}

// --- ss ---------------------------------------------------------------------

type Listener struct {
	Proto   string `json:"proto"`
	Local   string `json:"local"`
	Peer    string `json:"peer"`
	Port    int    `json:"port"`
	Process string `json:"process"`
	PID     int    `json:"pid"`
}

// users:(("nginx",pid=1234,fd=6),("nginx",pid=1235,fd=6))
var reSSProc = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// parseSS reads ss output, tolerating both column layouts we ask for:
//
//	ss -tulpnH:  tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=1234,fd=6))
//	ss -tnpH:    ESTAB 0 0 10.0.0.2:22 10.0.0.9:53410 users:(("sshd",pid=1234,fd=3))
//
// The listening form leads with a Netid (tcp/udp); the established form leads
// with the State. Rather than index by position, pick the address fields
// (host:port or host:*) in order — ss prints Local then Peer in both — and the
// users:(...) field wherever it lands.
func parseSS(out string, _ int) any {
	ls := []Listener{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		var addrs []string
		for _, tok := range f {
			if looksAddr(tok) {
				addrs = append(addrs, tok)
			}
		}
		if len(addrs) == 0 {
			continue
		}
		proto := "tcp"
		if f[0] == "tcp" || f[0] == "udp" {
			proto = f[0]
		}
		l := Listener{Proto: proto, Local: addrs[0]}
		if len(addrs) > 1 {
			l.Peer = addrs[1]
		}
		if i := strings.LastIndex(addrs[0], ":"); i >= 0 {
			l.Port = int(atoi64(addrs[0][i+1:]))
		}
		// A process name may contain spaces ("Web Content"); match the whole line.
		if m := reSSProc.FindStringSubmatch(line); m != nil {
			l.Process = m[1]
			l.PID = int(atoi64(m[2]))
		}
		ls = append(ls, l)
	}
	return map[string]any{"listeners": ls}
}

// looksAddr reports whether a field is a host:port / host:* address, so the
// users:(...) and numeric queue columns are not mistaken for one. Handles the
// IPv6 bracket form [::1]:22 via LastIndex.
func looksAddr(s string) bool {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return false
	}
	tail := s[i+1:]
	if tail == "*" {
		return true
	}
	_, err := strconv.Atoi(tail)
	return err == nil
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

// --- supervisor -------------------------------------------------------------

type Program struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Info  string `json:"info"`
}

// parseSupervisor reads `supervisorctl status`:
//
//	web       RUNNING   pid 1234, uptime 1:02:03
//	worker    STOPPED   Jun 01 12:00 PM
//	beat      FATAL     Exited too quickly (process log may have details)
func parseSupervisor(out string, _ int) any {
	progs := []Program{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		progs = append(progs, Program{
			Name:  f[0],
			State: f[1],
			Info:  strings.Join(f[2:], " "),
		})
	}
	return map[string]any{"programs": progs}
}

// --- docker -----------------------------------------------------------------

type Container struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`  // running | exited | created | paused …
	Status string `json:"status"` // "Up 3 hours", "Exited (0) 2 days ago"
	Ports  string `json:"ports"`
	RunFor string `json:"runningFor"`
}

// parseDockerPS reads `docker ps -a --format '{{json .}}'` — one JSON object
// per line. Field names are docker's PascalCase.
func parseDockerPS(out string, _ int) any {
	cs := []Container{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var d struct {
			ID, Names, Image, State, Status, Ports, RunningFor string
		}
		if json.Unmarshal([]byte(line), &d) != nil {
			continue
		}
		id := d.ID
		if len(id) > 12 {
			id = id[:12] // ps shows short IDs; commands accept them
		}
		cs = append(cs, Container{
			ID: id, Name: d.Names, Image: d.Image,
			State: d.State, Status: d.Status, Ports: d.Ports, RunFor: d.RunningFor,
		})
	}
	return map[string]any{"containers": cs}
}

// --- inodes -----------------------------------------------------------------

type Inode struct {
	Source  string `json:"source"`
	Mount   string `json:"mount"`
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	Free    int64  `json:"free"`
	UsedPct int    `json:"usedPct"`
}

// parseInodes reads `df -Pi`: Filesystem Inodes IUsed IFree IUse% Mounted.
// The -P (POSIX) format guarantees one line per filesystem.
func parseInodes(out string, _ int) any {
	rows := []Inode{}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if i == 0 { // header
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		src := f[0]
		if pseudoFS[src] || strings.HasPrefix(src, "/dev/loop") {
			continue
		}
		total := atoi64(f[1])
		row := Inode{
			Source: src, Mount: strings.Join(f[5:], " "),
			Total: total, Used: atoi64(f[2]), Free: atoi64(f[3]),
		}
		if total > 0 {
			row.UsedPct = int(row.Used * 100 / total)
		}
		rows = append(rows, row)
	}
	return map[string]any{"inodes": rows}
}

// --- tls --------------------------------------------------------------------

// parseTLS reads the subject/issuer/dates that `openssl x509` prints:
//
//	subject=CN = example.com
//	issuer=C = US, O = Let's Encrypt, CN = R3
//	notBefore=Jun  1 00:00:00 2026 GMT
//	notAfter=Aug 30 23:59:59 2026 GMT
func parseTLS(out string, _ int) any {
	m := map[string]any{"subject": "", "issuer": "", "notBefore": "", "notAfter": "", "ok": false}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "subject":
			m["subject"] = v
		case "issuer":
			m["issuer"] = v
		case "notBefore":
			m["notBefore"] = v
		case "notAfter":
			m["notAfter"] = v
		}
	}
	// A cert was returned only if we got an end date; otherwise the handshake
	// failed and the UI should say so rather than show blanks.
	m["ok"] = m["notAfter"] != ""
	return m
}
