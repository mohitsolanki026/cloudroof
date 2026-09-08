// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package facts fingerprints a host on first connect.
//
// The result drives the entire UI: an action is only rendered if the host
// advertised the capability it needs. This is what lets CloudRoof not assume
// anything about the distro, the init system, or whether sudo works.
package facts

import (
	"context"
	"strings"
	"time"

	"cloudroof/internal/sshx"
	"cloudroof/internal/store"
)

// script emits one key=value per line. It is POSIX sh — no bashisms — because
// it has to run on Alpine, BusyBox, and whatever a stray VPS has as /bin/sh.
// It is fed via stdin to `sh -s`, so quoting is a non-issue.
const script = `
echo "hostname=$(hostname 2>/dev/null)"
echo "kernel=$(uname -r 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"

if [ -r /etc/os-release ]; then
  . /etc/os-release
  echo "os_name=$ID"
  echo "os_version=$VERSION_ID"
  echo "os_pretty=$PRETTY_NAME"
fi

if [ -d /run/systemd/system ]; then
  echo "init=systemd"
elif command -v rc-service >/dev/null 2>&1; then
  echo "init=openrc"
elif [ -d /etc/init.d ]; then
  echo "init=sysv"
else
  echo "init=unknown"
fi

if [ "$(id -u)" = "0" ]; then
  echo "sudo=root"
elif command -v sudo >/dev/null 2>&1; then
  if sudo -n true >/dev/null 2>&1; then
    echo "sudo=nopasswd"
  else
    echo "sudo=password"
  fi
else
  echo "sudo=none"
fi

for c in systemctl journalctl supervisorctl docker podman nginx caddy apache2 \
         apt-get dnf yum apk pacman ss netstat curl wget jq openssl; do
  command -v "$c" >/dev/null 2>&1 && echo "cap=$c"
done
`

// Fingerprint runs the probe and returns structured facts. It never fails on
// a partially-populated result: an unknown value is a fact too.
func Fingerprint(ctx context.Context, b *sshx.Broker, t sshx.Target) (store.HostFacts, error) {
	res, err := b.Exec(ctx, t, "sh -s", sshx.ExecOpts{
		Stdin:   strings.NewReader(script),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return store.HostFacts{}, err
	}
	f := parse(res.Stdout)
	f.MachineID = t.ID
	f.FetchedAt = time.Now().UTC()
	return f, nil
}

func parse(out string) store.HostFacts {
	f := store.HostFacts{
		InitSystem:   "unknown",
		SudoMode:     "unknown",
		Capabilities: []string{},
	}
	var pretty string

	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "hostname":
			f.Hostname = v
		case "kernel":
			f.Kernel = v
		case "arch":
			f.Arch = v
		case "os_name":
			f.OSName = v
		case "os_version":
			f.OSVersion = v
		case "os_pretty":
			pretty = v
		case "init":
			f.InitSystem = v
		case "sudo":
			f.SudoMode = v
		case "cap":
			if v != "" {
				f.Capabilities = append(f.Capabilities, v)
			}
		}
	}

	// PRETTY_NAME is nicer for display when the ID/VERSION pair is sparse
	// (e.g. Arch has no VERSION_ID).
	if f.OSName == "" && pretty != "" {
		f.OSName = pretty
	}
	return f
}
