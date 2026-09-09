# CloudRoof v1.5.1

A packaging fix over v1.5.0 (see **Fixes** below). CloudRoof is a self-hosted, agentless dashboard for
your Linux fleet across every cloud you rent from: power instances on/off
through the provider API, and run the day-2 ops you keep re-typing over SSH —
from one place, with nothing installed on the target machines and no credential
leaving your box.

(The 1.5 line reflects the internal build milestones — proof → useful →
providers → fleet. v1.5.0 was the first public tag; v1.5.1 fixes how it is
built and installed.)

## Fixes in v1.5.1
- **`go install` now produces the full binary, UI included.** The built
  frontend (`web/dist/`) is committed to the repo, so a `go install` — which
  compiles module source and can't run the JS build — still embeds the web UI.
  v1.5.0 failed with `pattern all:dist: no matching files found` because the
  bundle was gitignored and absent from the published module.
- **A UI-less build (if you ever produce one) says so** with a clear
  build-from-source message instead of a bare 404.

## Highlights

### Multi-cloud, one fleet
- **Six providers:** Hetzner Cloud, DigitalOcean, AWS EC2, Azure, Google Cloud,
  and Vultr. Add an account, sync, and every instance shows up in one list.
- **Power control** via the provider API — start, shut down, reboot,
  force-stop, force-reboot — for when SSH itself is dead.
- **Two-state status:** each machine shows what the cloud reports (running /
  stopped) *and* what SSH actually saw (reachable / refused / timeout / auth /
  host-key-changed). Their disagreement is the signal, and reachability
  refreshes on its own so the dots stay honest.

### Agentless SSH operations
A curated, capability-detected action catalog rendered as buttons — tabs appear
only for what a host actually has:
- **Overview** — uptime, load, memory, filesystems, sessions, reboot-required.
- **Services** (systemd) — start/stop/restart, journal snapshot, live
  `journalctl -f`.
- **Programs** (supervisor) — start/stop/restart and live `tail -f`.
- **Processes** — top by CPU with TERM/KILL.
- **Containers** (Docker) — start/stop/restart, live `docker logs -f`, disk
  usage, prune.
- **Network** — listening ports and established connections mapped to the
  owning process, plus a reach-a-URL probe run from the host.
- **Web** — nginx config test + gated reload, and a TLS certificate expiry
  check for any host:port.
- **Disk** — largest directories, inode usage, journal vacuum, apt cache clean.
- **Terminal** — a real shell over the pooled SSH connection.
- **Activity** — an append-only audit log of every command CloudRoof has run,
  with output, exit code, and duration.

### Fleet operations
- **Groups** — a named set of tags; a machine belongs when it carries *all* of
  them, resolved live so membership tracks the fleet as tags change.
- **Bulk actions** — run one action across a group or an ad-hoc selection.
  Confirmation escalates over the single-host gate: a read needs a click, a
  write makes you type the number of affected hosts after a preview that names
  them. Every host is gated, sudo-decided, and audited individually, and you
  get a per-host result.
- **Custom actions** — save your own command as a button with named params
  (shell-quoted), a declared danger tier the gate enforces, and optional
  capability requirements.

### Safety
- Four danger tiers: **Tier 0** runs on click, **Tier 1** confirms once,
  **Tier 2** makes you type the machine's name. **Tier 3** (`rm -rf`, `mkfs`,
  terminate) has no button and never will — it is refused at creation and by
  the gate.
- Every button shows the exact command before it runs and logs it after.
  Root-needing actions use passwordless `sudo -n` where the host allows it and
  are disabled — never left hanging on a prompt — where it doesn't.

### Security posture
- **Loopback by default** (`127.0.0.1:7070`); exposing it beyond the machine is
  an explicit opt-in with a `Host` allowlist.
- A browser guard on every API request refuses cross-origin (CSRF) and unknown
  `Host` (DNS-rebinding) requests.
- SSH keys and cloud tokens are sealed at rest with **AES-256-GCM** under a
  master key kept outside the database.
- SSH host keys are **pinned on first sight** and block on change until you
  review and accept the new key.

## Install

**With Go** (full binary, UI bundled):
```
go install github.com/mohitsolanki026/cloudroof/cmd/cloudroof@v1.5.1 && cloudroof
```

**From source:**
```
git clone https://github.com/mohitsolanki026/cloudroof
cd cloudroof && make web && make build && ./bin/cloudroof
```

**Prebuilt binaries** for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and
`darwin/arm64` are attached to this release (with `checksums.txt`). Then open
http://localhost:7070.

### Slim builds
All six providers make a ~59 MB binary; the AWS, Azure, and GCP SDKs are the
bulk of it. Hetzner, DigitalOcean, and Vultr are plain REST clients, so a build
with just those is ~16 MB. Providers are opt-out at build time:
```
make slim                                      # drops AWS + Azure + GCP
go build -tags "noaws noazure nogcp novultr" ./cmd/cloudroof
```
A provider compiled out never registers, and its account type doesn't appear in
the UI.

## Not in this release (yet)
- **Metrics history / charts** — CloudRoof reads live state, it does not store a
  time series.
- **Multi-user, RBAC, SSO** — single admin only; put it behind an authenticating
  reverse proxy if you expose it.
- A hosted `curl … | sh` installer and a published Docker image — planned once
  a domain and release pipeline are in place.

## Verification
Unit tests cover the output parsers (a fixture per format), the catalog
invariants, command rendering/quoting, the keyring, and store migrations. An
end-to-end script (`make e2e`) drives the real binary against a throwaway local
`sshd` through the gates, the browser guard, probing, the action catalog,
streaming, bulk, groups, custom actions, and host-key rotation.

## License
AGPL-3.0-only (see `LICENSE`). A separate commercial license is available for
organizations that cannot use AGPL software — contact the maintainer.
