// Package actions is the declarative catalog and the engine that runs it.
//
// The catalog is data, not code. Each Action says what capability it needs,
// what command it runs, what params it takes, how dangerous it is, and how to
// parse the result. The UI renders from this; adding an action is adding a
// spec, not writing a screen.
package actions

import (
	"regexp"
	"time"
)

// Danger tiers. The gate is decided by the tier and is not negotiable per
// user — that's the point.
const (
	DangerRead       = 0 // runs on click, cannot mutate state
	DangerReversible = 1 // single-click confirm
	DangerDisruptive = 2 // type the hostname to confirm
	DangerForbidden  = 3 // never a button; terminal only
)

// Sudo says how an action relates to root.
//
//	SudoNone      — runs as the SSH user, always.
//	SudoPreferred — uses `sudo -n` when it is passwordless, otherwise runs as
//	                the SSH user and lets the kernel refuse what it refuses.
//	                Right for kill, du, ss: useful without root, better with it.
//	SudoRequired  — refuses to run unless root or passwordless sudo. Right
//	                for systemctl start/stop, reboot, journal vacuum.
//
// Nothing ever answers a sudo password prompt.
type Sudo string

const (
	SudoNone      Sudo = ""
	SudoPreferred Sudo = "preferred"
	SudoRequired  Sudo = "required"
)

// Param is one templated input. Pattern is mandatory: every value that reaches
// a shell must match an allowlist regex AND be single-quoted. Belt and braces.
type Param struct {
	Name    string         `json:"name"`
	Label   string         `json:"label"`
	Pattern *regexp.Regexp `json:"-"`
	// Source names a Tier-0 action whose output supplies valid values, so the
	// UI can render a picker instead of a text box.
	Source string `json:"source,omitempty"`
}

type Action struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Category string `json:"category"`
	// Requires lists capabilities the host must advertise (from facts).
	// All must be present.
	Requires []string `json:"requires"`
	// Command is a shell template. {{name}} is replaced with the quoted param.
	Command string        `json:"command"`
	Params  []Param       `json:"params"`
	Danger  int           `json:"danger"`
	Sudo    Sudo          `json:"sudo"`
	Timeout time.Duration `json:"-"`
	// Parse names a parser in parsers.go; "" means raw text.
	Parse string `json:"parse"`
	// Script, when set, is fed to `sh -s` over stdin instead of running
	// Command. Used for multi-line probes where quoting would be miserable.
	Script string `json:"-"`
}

// Patterns shared across params. Deliberately tight.
var (
	// Anchored to an alphanumeric so a value can never be read as an option
	// (`systemctl stop --now` style) — quoting stops shell breakout, not that.
	reUnit = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:\\-]{0,254}$`)
	rePID  = regexp.MustCompile(`^[0-9]{1,10}$`)
	reInt  = regexp.MustCompile(`^[0-9]{1,6}$`)
	rePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,255}$`)
)

// overviewScript is the batched probe behind the Overview tab. One round-trip,
// POSIX sh, key=value out. Sizes are in kB because that is what /proc and
// BusyBox df agree on; the parser converts.
const overviewScript = `
echo "now=$(date -u +%s 2>/dev/null)"
echo "uptime_s=$(cut -d. -f1 /proc/uptime 2>/dev/null)"
echo "load=$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null)"
echo "cpus=$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null)"
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} /^MemFree:/{f=$2} /^Buffers:/{b=$2} /^Cached:/{c=$2} /^SwapTotal:/{st=$2} /^SwapFree:/{sf=$2}
     END{print "mem_total_kb=" t; print "mem_avail_kb=" a; print "mem_free_kb=" f; print "mem_buffers_kb=" b; print "mem_cached_kb=" c; print "swap_total_kb=" st; print "swap_free_kb=" sf}' /proc/meminfo 2>/dev/null
df -Pk 2>/dev/null | awk 'NR>1 {print "disk=" $1 "|" $2 "|" $3 "|" $4 "|" $6}'
echo "users=$(who 2>/dev/null | wc -l | tr -d ' ')"
if [ -f /var/run/reboot-required ]; then echo "reboot_required=1"; else echo "reboot_required=0"; fi
`

// Catalog is the v0.1 action set. Order within a category is display order.
var Catalog = []Action{
	// --- system -----------------------------------------------------------
	{
		ID: "system.overview", Label: "Overview", Category: "system",
		Script: overviewScript, Parse: "overview", Danger: DangerRead,
		Timeout: 15 * time.Second,
	},

	// --- services (systemd) -----------------------------------------------
	{
		ID: "services.list", Label: "List services", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl list-units --type=service --all --no-pager --plain --no-legend",
		Parse:    "systemd_units", Danger: DangerRead,
	},
	{
		ID: "services.status", Label: "Status", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl status {{unit}} --no-pager -l",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "raw", Danger: DangerRead,
	},
	{
		ID: "services.journal", Label: "Journal", Category: "services",
		Requires: []string{"journalctl"},
		Command:  "journalctl -u {{unit}} -n 200 --no-pager -o short-iso",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "lines", Danger: DangerRead, Sudo: SudoPreferred,
	},
	{
		ID: "services.start", Label: "Start", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl start {{unit}}",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "exit", Danger: DangerReversible, Sudo: SudoRequired,
	},
	{
		ID: "services.stop", Label: "Stop", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl stop {{unit}}",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "exit", Danger: DangerReversible, Sudo: SudoRequired,
	},
	{
		ID: "services.restart", Label: "Restart", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl restart {{unit}}",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "exit", Danger: DangerReversible, Sudo: SudoRequired,
	},
	{
		ID: "services.enable", Label: "Enable at boot", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl enable {{unit}}",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "exit", Danger: DangerReversible, Sudo: SudoRequired,
	},
	{
		ID: "services.disable", Label: "Disable at boot", Category: "services",
		Requires: []string{"systemctl"},
		Command:  "systemctl disable {{unit}}",
		Params:   []Param{{Name: "unit", Label: "Unit", Pattern: reUnit, Source: "services.list"}},
		Parse:    "exit", Danger: DangerReversible, Sudo: SudoRequired,
	},

	// --- processes --------------------------------------------------------
	{
		ID: "processes.top", Label: "Top processes", Category: "processes",
		Command: "ps -eo pid,user:20,pcpu,pmem,rss,etimes,comm --no-headers --sort=-pcpu",
		Parse:   "ps", Danger: DangerRead,
	},
	{
		ID: "processes.terminate", Label: "Terminate", Category: "processes",
		Command: "kill -TERM {{pid}}",
		Params:  []Param{{Name: "pid", Label: "PID", Pattern: rePID}},
		Parse:   "exit", Danger: DangerReversible, Sudo: SudoPreferred,
	},
	{
		ID: "processes.kill", Label: "Force kill", Category: "processes",
		Command: "kill -KILL {{pid}}",
		Params:  []Param{{Name: "pid", Label: "PID", Pattern: rePID}},
		Parse:   "exit", Danger: DangerDisruptive, Sudo: SudoPreferred,
	},

	// --- network ----------------------------------------------------------
	{
		// Without root, ss -p only names processes the SSH user owns; the
		// port list itself is still complete.
		ID: "network.ports", Label: "Listening ports", Category: "network",
		Requires: []string{"ss"},
		Command:  "ss -tulpnH",
		Parse:    "ss", Danger: DangerRead, Sudo: SudoPreferred,
	},

	// --- disk -------------------------------------------------------------
	{
		ID: "disk.largest", Label: "Largest directories", Category: "disk",
		// stderr is not discarded: "permission denied" lines are exactly
		// what explains a partial result, and they belong in the audit row.
		Command: "du -xk -d 2 {{path}} | sort -rn | head -30",
		Params:  []Param{{Name: "path", Label: "Path", Pattern: rePath}},
		Parse:   "du", Danger: DangerRead, Sudo: SudoPreferred,
		Timeout: 120 * time.Second,
	},
	{
		ID: "disk.vacuum_journal", Label: "Vacuum journal (keep 7d)", Category: "disk",
		Requires: []string{"journalctl"},
		Command:  "journalctl --vacuum-time=7d",
		Parse:    "raw", Danger: DangerReversible, Sudo: SudoRequired,
	},

	// --- host -------------------------------------------------------------
	{
		ID: "host.reboot", Label: "Reboot (via SSH)", Category: "host",
		Command: "systemctl reboot || reboot",
		Parse:   "exit", Danger: DangerDisruptive, Sudo: SudoRequired,
		Timeout: 10 * time.Second,
	},
}

var byID = func() map[string]Action {
	m := make(map[string]Action, len(Catalog))
	for _, a := range Catalog {
		m[a.ID] = a
	}
	return m
}()

// Lookup finds an action by ID.
func Lookup(id string) (Action, bool) {
	a, ok := byID[id]
	return a, ok
}

// Available filters the catalog to what a host with the given capabilities
// can run. This is what the UI renders from; a host without systemctl never
// sees a services tab.
func Available(caps []string) []Action {
	have := make(map[string]bool, len(caps))
	for _, c := range caps {
		have[c] = true
	}
	out := make([]Action, 0, len(Catalog))
	for _, a := range Catalog {
		ok := true
		for _, r := range a.Requires {
			if !have[r] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, a)
		}
	}
	return out
}

func init() {
	// JSON null for a nil slice is a foot-gun for every client; hand out
	// empty lists instead.
	for i := range Catalog {
		if Catalog[i].Requires == nil {
			Catalog[i].Requires = []string{}
		}
		if Catalog[i].Params == nil {
			Catalog[i].Params = []Param{}
		}
	}
}
