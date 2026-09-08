// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package actions

import "testing"

// These fixtures are real output shapes from the tools the catalog runs, on
// the distros v1 supports. They are the contract: if a parser changes, a
// fixture must prove it still reads what the wild actually prints. The review
// that shipped v0.1 flagged the absence of these; here they are.

func mapOf(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("parser did not return a map, got %T", v)
	}
	return m
}

func TestParseOverview(t *testing.T) {
	// Emitted by overviewScript: key=value, sizes in kB.
	out := `now=1725400000
uptime_s=90061
load=0.52 0.48 0.40
cpus=4
mem_total_kb=8000000
mem_avail_kb=5000000
mem_free_kb=1000000
mem_buffers_kb=200000
mem_cached_kb=3800000
swap_total_kb=2000000
swap_free_kb=1500000
disk=/dev/sda1|100000000|60000000|40000000|/
disk=tmpfs|1000000|1000|999000|/run
disk=/dev/sda15|500000|100000|400000|/boot/efi
users=2
reboot_required=1`
	o := parseOverview(out, 0).(Overview)
	if o.UptimeSeconds != 90061 {
		t.Errorf("uptime = %d", o.UptimeSeconds)
	}
	if o.CPUs != 4 || len(o.Load) != 3 || o.Load[0] != 0.52 {
		t.Errorf("cpus/load = %d %v", o.CPUs, o.Load)
	}
	if o.MemTotal != 8000000*1024 || o.MemUsed != (8000000-5000000)*1024 {
		t.Errorf("mem total/used = %d/%d", o.MemTotal, o.MemUsed)
	}
	if !o.RebootRequired || o.Users != 2 {
		t.Errorf("reboot=%v users=%d", o.RebootRequired, o.Users)
	}
	// tmpfs is a pseudo-fs and must be dropped; two real disks remain.
	if len(o.Disks) != 2 {
		t.Fatalf("want 2 real disks, got %d: %+v", len(o.Disks), o.Disks)
	}
	if o.Disks[0].Mount != "/" || o.Disks[0].UsedPct != 60 {
		t.Errorf("root disk = %+v", o.Disks[0])
	}
}

func TestParseOverviewNoMemAvailable(t *testing.T) {
	// Pre-3.14 kernels: no MemAvailable line — fall back to free+buffers+cached.
	out := `mem_total_kb=1000000
mem_avail_kb=0
mem_free_kb=200000
mem_buffers_kb=100000
mem_cached_kb=500000`
	o := parseOverview(out, 0).(Overview)
	want := int64(1000000-(200000+100000+500000)) * 1024
	if o.MemUsed != want {
		t.Errorf("MemUsed fallback = %d, want %d", o.MemUsed, want)
	}
}

func TestParseSystemdUnits(t *testing.T) {
	// systemctl list-units --type=service --all --plain --no-legend
	out := "ssh.service                loaded active   running OpenBSD Secure Shell server\n" +
		"nginx.service              loaded active   running A high performance web server and a reverse proxy server\n" +
		"cron.service               loaded active   running Regular background program processing daemon\n" +
		"apt-daily.service          loaded inactive dead    Daily apt download activities"
	u := mapOf(t, parseSystemdUnits(out, 0))["units"].([]Unit)
	if len(u) != 4 {
		t.Fatalf("want 4 units, got %d", len(u))
	}
	if u[1].Name != "nginx.service" || u[1].Active != "active" || u[1].Sub != "running" {
		t.Errorf("nginx row = %+v", u[1])
	}
	if u[1].Description != "A high performance web server and a reverse proxy server" {
		t.Errorf("description not joined: %q", u[1].Description)
	}
	if u[3].Active != "inactive" || u[3].Sub != "dead" {
		t.Errorf("inactive row = %+v", u[3])
	}
}

func TestParsePS(t *testing.T) {
	// ps -eo pid,user:20,pcpu,pmem,rss,etimes,comm --no-headers --sort=-pcpu
	out := "  1234 www-data            12.5  3.2  20480   3600 nginx\n" +
		"      1 root                 0.0  0.1   9800 900000 systemd\n" +
		"  9999 postgres             5.5  8.1 512000  86400 postgres"
	p := mapOf(t, parsePS(out, 0))["processes"].([]Process)
	if len(p) != 3 {
		t.Fatalf("want 3 procs, got %d", len(p))
	}
	if p[0].PID != 1234 || p[0].User != "www-data" || p[0].CPU != 12.5 || p[0].RSS != 20480*1024 {
		t.Errorf("row0 = %+v", p[0])
	}
	if p[2].Command != "postgres" || p[2].Elapsed != 86400 {
		t.Errorf("row2 = %+v", p[2])
	}
}

func TestParseSSListening(t *testing.T) {
	// ss -tulpnH — leading Netid, IPv4 + IPv6, one line with no process column.
	out := `tcp   LISTEN 0 511  0.0.0.0:80    0.0.0.0:* users:(("nginx",pid=1234,fd=6))
tcp   LISTEN 0 128  [::]:22       [::]:*    users:(("sshd",pid=800,fd=3))
udp   UNCONN 0 0    0.0.0.0:68    0.0.0.0:*
tcp   LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=9999,fd=5))`
	ls := mapOf(t, parseSS(out, 0))["listeners"].([]Listener)
	if len(ls) != 4 {
		t.Fatalf("want 4 listeners, got %d: %+v", len(ls), ls)
	}
	if ls[0].Proto != "tcp" || ls[0].Port != 80 || ls[0].Process != "nginx" || ls[0].PID != 1234 {
		t.Errorf("nginx = %+v", ls[0])
	}
	if ls[1].Port != 22 || ls[1].Process != "sshd" { // IPv6 [::]:22
		t.Errorf("ipv6 sshd = %+v", ls[1])
	}
	if ls[2].Proto != "udp" || ls[2].Port != 68 || ls[2].Process != "" { // no process column
		t.Errorf("udp no-proc = %+v", ls[2])
	}
}

func TestParseSSEstablished(t *testing.T) {
	// ss -tnpH state established — leads with State, no Netid column.
	out := `ESTAB 0 0 10.0.0.2:22      10.0.0.9:53410 users:(("sshd",pid=1234,fd=3))
ESTAB 0 0 10.0.0.2:443     203.0.113.7:44122`
	ls := mapOf(t, parseSS(out, 0))["listeners"].([]Listener)
	if len(ls) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(ls), ls)
	}
	// Local is the first address, peer the second — even without a Netid.
	if ls[0].Local != "10.0.0.2:22" || ls[0].Peer != "10.0.0.9:53410" || ls[0].Port != 22 {
		t.Errorf("established row0 = %+v", ls[0])
	}
	if ls[0].Process != "sshd" || ls[0].PID != 1234 {
		t.Errorf("established process = %+v", ls[0])
	}
	if ls[1].Peer != "203.0.113.7:44122" {
		t.Errorf("row1 peer = %+v", ls[1])
	}
}

func TestParseDU(t *testing.T) {
	// du -xk -d 2 <path> | sort -rn | head -30 — size (kB) TAB path
	out := "2048000\t/var/lib/docker\n512000\t/var/log\n1024\t/var/cache/apt"
	d := mapOf(t, parseDU(out, 0))["dirs"].([]DirSize)
	if len(d) != 3 || d[0].Path != "/var/lib/docker" || d[0].Size != 2048000*1024 {
		t.Errorf("du = %+v", d)
	}
}

func TestParseSupervisor(t *testing.T) {
	out := `web                              RUNNING   pid 3145, uptime 1:02:03
worker                           STOPPED   Jun 01 12:00 PM
beat                             FATAL     Exited too quickly (process log may have details)`
	p := mapOf(t, parseSupervisor(out, 3))["programs"].([]Program)
	if len(p) != 3 {
		t.Fatalf("want 3, got %d", len(p))
	}
	if p[0].Name != "web" || p[0].State != "RUNNING" || p[0].Info != "pid 3145, uptime 1:02:03" {
		t.Errorf("web = %+v", p[0])
	}
	if p[2].State != "FATAL" {
		t.Errorf("beat = %+v", p[2])
	}
}

func TestParseDockerPS(t *testing.T) {
	// docker ps -a --no-trunc --format '{{json .}}' — one JSON object per line.
	out := `{"ID":"abc123def4567890","Names":"api","Image":"myorg/api:v2","State":"running","Status":"Up 3 hours","Ports":"0.0.0.0:8080->80/tcp","RunningFor":"3 hours ago"}
{"ID":"beef0000","Names":"cache","Image":"redis:7","State":"exited","Status":"Exited (0) 2 days ago","Ports":"","RunningFor":"2 days ago"}
garbage-not-json`
	c := mapOf(t, parseDockerPS(out, 0))["containers"].([]Container)
	if len(c) != 2 { // the garbage line is skipped
		t.Fatalf("want 2 containers, got %d: %+v", len(c), c)
	}
	if c[0].ID != "abc123def456" { // truncated to 12
		t.Errorf("id not shortened: %q", c[0].ID)
	}
	if c[0].Name != "api" || c[0].State != "running" || c[0].Ports != "0.0.0.0:8080->80/tcp" {
		t.Errorf("api = %+v", c[0])
	}
	if c[1].State != "exited" {
		t.Errorf("cache = %+v", c[1])
	}
}

func TestParseInodes(t *testing.T) {
	// df -Pi — header line then one row per filesystem; pseudo-fs dropped.
	out := `Filesystem       Inodes  IUsed    IFree IUse% Mounted on
/dev/sda1       6553600 250000  6303600    4% /
tmpfs            999999     30   999969    1% /run
/dev/sda15            0      0        0    0% /boot/efi`
	rows := mapOf(t, parseInodes(out, 0))["inodes"].([]Inode)
	if len(rows) != 2 { // tmpfs dropped; sda15 with 0 total kept but 0%
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Mount != "/" || rows[0].Total != 6553600 || rows[0].UsedPct != 3 {
		t.Errorf("root = %+v", rows[0])
	}
	if rows[1].Mount != "/boot/efi" || rows[1].UsedPct != 0 { // zero total -> no divide
		t.Errorf("efi = %+v", rows[1])
	}
}

func TestParseTLS(t *testing.T) {
	out := `subject=CN = example.com
issuer=C = US, O = Let's Encrypt, CN = R3
notBefore=Jun  1 00:00:00 2026 GMT
notAfter=Aug 30 23:59:59 2026 GMT`
	m := parseTLS(out, 0).(map[string]any)
	if m["ok"] != true {
		t.Errorf("ok = %v", m["ok"])
	}
	if m["subject"] != "CN = example.com" || m["notAfter"] != "Aug 30 23:59:59 2026 GMT" {
		t.Errorf("tls = %+v", m)
	}
}

func TestParseTLSHandshakeFailure(t *testing.T) {
	// A failed handshake yields no x509 output at all.
	m := parseTLS("", 1).(map[string]any)
	if m["ok"] != false {
		t.Errorf("empty output should be ok=false, got %+v", m)
	}
}

// TestCatalogIntegrity guards the invariants a human might break editing the
// catalog: no Tier-3 button, every param has a pattern, stream actions are
// read-tier, and IDs are unique.
func TestCatalogIntegrity(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Catalog {
		if seen[a.ID] {
			t.Errorf("duplicate action id %q", a.ID)
		}
		seen[a.ID] = true
		if a.Danger == DangerForbidden {
			t.Errorf("%s is a Tier-3 button, which must never exist", a.ID)
		}
		if a.Danger < DangerRead || a.Danger > DangerDisruptive {
			t.Errorf("%s has danger %d out of range", a.ID, a.Danger)
		}
		for _, p := range a.Params {
			if p.Pattern == nil {
				t.Errorf("%s param %q has no Pattern (injection risk)", a.ID, p.Name)
			}
		}
		if a.Stream && a.Danger != DangerRead {
			t.Errorf("%s streams but is not Tier 0", a.ID)
		}
		if a.Command == "" && a.Script == "" {
			t.Errorf("%s has neither Command nor Script", a.ID)
		}
	}
}

// TestRenderQuotingAndTemplates checks the two things render() must get right:
// a param value is single-quoted (injection), and literal Go-template braces
// like docker's {{json .}} survive (no false "unresolved placeholder").
func TestRenderQuotingAndTemplates(t *testing.T) {
	restart, _ := Lookup("services.restart")
	cmd, err := render(restart, map[string]string{"unit": "nginx.service"})
	if err != nil || cmd != "systemctl restart 'nginx.service'" {
		t.Errorf("restart render = %q, %v", cmd, err)
	}

	// A value that passes the regex is still single-quoted.
	_, err = render(restart, map[string]string{"unit": "a b; rm -rf /"})
	if err == nil {
		t.Errorf("shell metacharacters should fail the unit pattern")
	}

	list, _ := Lookup("containers.list")
	cmd, err = render(list, nil)
	if err != nil {
		t.Fatalf("docker ps render errored on literal braces: %v", err)
	}
	if cmd != "docker ps -a --no-trunc --format '{{json .}}'" {
		t.Errorf("docker template mangled: %q", cmd)
	}
}
