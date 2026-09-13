# CloudRoof — Technical Documentation

How CloudRoof is built and how its moving parts actually work. This is the
implementation-level companion to the README; for the design rationale and the
invariants that must not regress, see [`.agents/README.md`](../.agents/README.md).

- [1. Architecture at a glance](#1-architecture-at-a-glance)
- [2. Process model & startup](#2-process-model--startup)
- [3. Data model (SQLite)](#3-data-model-sqlite)
- [4. The two-identity model & status](#4-the-two-identity-model--status)
- [5. HTTP layer & the browser guard](#5-http-layer--the-browser-guard)
- [6. The SSH connection broker](#6-the-ssh-connection-broker)
- [7. SSH reachability checks (frequency & backoff)](#7-ssh-reachability-checks-frequency--backoff)
- [8. Cloud power-state sync](#8-cloud-power-state-sync)
- [9. The action engine](#9-the-action-engine)
- [10. Live streaming (logs / terminal)](#10-live-streaming-logs--terminal)
- [11. Groups, bulk actions, custom actions](#11-groups-bulk-actions-custom-actions)
- [12. Providers](#12-providers)
- [13. How credentials are stored](#13-how-credentials-are-stored)
- [14. Security posture](#14-security-posture)
- [15. Configuration reference](#15-configuration-reference)
- [16. Build & deployment](#16-build--deployment)

---

## 1. Architecture at a glance

CloudRoof is a single Go binary with the React frontend embedded. It talks
**outward** in two directions and accepts **no inbound** connections to your
machines:

```
                        ┌──────────────────────── your box ────────────────────────┐
   browser (SPA) ─────►  HTTP API + WebSocket  ──►  cloud provider APIs (HTTPS)      │  → power on/off, inventory
   http://localhost:7070 │        │                                                  │
                         │        └─────────────►  your machines (SSH :22)           │  → facts, actions, terminal
                         │                                                           │
                         │   SQLite (cloudroof.db)  +  sealed keystore (master.key)  │
                         └───────────────────────────────────────────────────────────┘
```

- **Frontend** — React 18 + TypeScript, built by Vite into `web/dist/`, embedded
  into the binary with `//go:embed all:dist` (`web/embed.go`). No separate web
  server; the Go process serves it.
- **Backend** — Go. Packages under `internal/`:
  - `config` — flags + environment.
  - `keyring` — seal/open secrets (AES-256-GCM).
  - `store` — SQLite persistence + migrations.
  - `sshx` — the SSH connection broker (pooled clients, exec, PTY, streams).
  - `facts` — host fingerprinting over SSH.
  - `provider` — cloud adapter interface + registry; `provider/<name>` adapters.
  - `actions` — the declarative action catalog and the execution engine.
  - `api` — HTTP routes, the request guard, credential resolution, sync, the
    reachability poller, the websocket bridges.
- **Storage** — one SQLite file (`cloudroof.db`) plus a `master.key` file, both
  in the data directory. The whole install backs up by copying that directory.

Everything is sized for **1–20 machines**: SQLite, in-process pooling, no
external services.

---

## 2. Process model & startup

`cmd/cloudroof/main.go` wires the process in this order: `config.Load` →
`keyring.Open` → `store.Open` (runs migrations) → `sshx.NewBroker` →
`api.New` → start background loops → `http.Server.ListenAndServe`.

Two background goroutines are launched, both bound to a context cancelled on
`SIGINT`/`SIGTERM`:

| Loop | Started by | Cadence | Job |
|---|---|---|---|
| Power-state sync | `StartBackground(ctx, 2m)` | every **2 minutes** | re-read cloud power state for every account (§8) |
| Reachability poller | `StartReachPoller(ctx)` | ticks every **20s**, per-host schedule (§7) | keep the SSH status dot honest |

Shutdown drains in-flight HTTP handlers (5s grace) **before** the deferred
broker/DB `Close`, so a request mid-flight isn't cut off.

---

## 3. Data model (SQLite)

Pure-Go driver (`modernc.org/sqlite`, so `CGO_ENABLED=0` stays possible). WAL
mode, `foreign_keys=ON`, `busy_timeout=5s`. Timestamps are stored as Unix
milliseconds. Migrations live in `internal/store/schema.go` as an ordered slice
and are **append-only** — three have shipped:

1. **001** — `credentials`, `cloud_accounts`, `machines`, `host_facts`,
   `host_keys`, `runs`.
2. **002** — adds `host_keys.seen_algorithm` / `seen_fingerprint`.
3. **003** — `groups`, `custom_actions`.

Key tables:

- **`machines`** — the join of a cloud identity and a host identity (see §4).
  Either half may be null.
- **`credentials`** — SSH auth material, `sealed` (never plaintext).
- **`cloud_accounts`** — a provider account, `sealed_token` (never plaintext).
- **`host_facts`** — the fingerprint (OS, kernel, init system, sudo mode,
  capabilities); drives which action tabs appear.
- **`host_keys`** — the pinned SSH host key, plus the last mismatching key seen.
- **`runs`** — the append-only audit log. `machine_id` is `ON DELETE SET NULL`
  and `machine_name` is denormalized, so deleting a machine never erases the
  record of what was done to it.
- **`groups`** / **`custom_actions`** — see §11.

The migration runner (`execMigration`) strips `--` comments before splitting on
`;`, and tolerates a `duplicate column name` (so a DB from an earlier in-place
edit still upgrades). See [migration notes](../.agents/README.md).

---

## 4. The two-identity model & status

A **machine** has two independent halves:

- **Cloud identity** — `provider`, `instance_id`, `power_state`, IPs — learned
  from the provider API. `HasCloud()` is true when both an account and an
  instance id are set.
- **Host identity** — `ssh_host`, `ssh_port`, `ssh_user`, `credential_id` —
  what CloudRoof needs to SSH in. `HasHost()` is true when host, user, and
  credential are all set.

They are tracked **separately on purpose**, which is why every machine shows
**two status dots**:

- **Power** (`power_state`): `running | stopped | pending | unknown` — what the
  provider reports.
- **Reachability** (`reach_state`): `ok | refused | timeout | auth_failed |
  hostkey_changed | unknown` — what SSH actually observed.

The interesting state is **running + unreachable**: the box is billed and "up"
according to the cloud, but SSH is dead (kernel panic, full disk, `sshd` down,
firewall). CloudRoof surfaces both facts on one card and offers a
provider-side hard reboot — the case a cloud console alone or an SSH client
alone each miss.

---

## 5. HTTP layer & the browser guard

Routing is the Go 1.22 `net/http.ServeMux` (method + path patterns), no router
dependency. Two wrappers sit in front of every handler:

1. `logRequests` — debug logging.
2. `guard` — the browser-side defense that stands in for authentication until
   multi-user lands. On every `/api` request it enforces:
   - **Host allowlist** — `Host` must be loopback or a name passed via
     `-hosts`. Blocks **DNS rebinding** (a malicious page resolving its own
     name to `127.0.0.1`). Rejected with `421`.
   - **Same-origin** — if `Origin`/`Sec-Fetch-Site` are present they must be
     same-origin. Blocks **CSRF**. Rejected with `403`.
   - **Content-Type** — state-changing requests with a body must be
     `application/json`, which forces a browser to preflight a cross-site call.
     Rejected with `415`.

   Non-browser clients (curl, the binary's own tooling) send none of these and
   pass. The websocket upgrader shares the same origin check.

Errors map to stable `code` strings the frontend switches on (`needs_confirm`,
`needs_name`, `not_available`, `hostkey_changed`, `unreachable`, …).

---

## 6. The SSH connection broker

`internal/sshx` owns **one pooled `*ssh.Client` per machine ID** and multiplexes
both non-interactive `Exec` (for actions and reachability) and interactive
`OpenShell`/`OpenStream` (PTY / log follow) over it.

- **Lazy dial, keyed by machine ID.** First use dials; the connection is cached
  and reused. A race to dial the same machine keeps one client and drops the
  loser.
- **Keepalive every 30s** — a `keepalive@openssh.com` global request with its
  own reply deadline; a black-holed peer is evicted within a tick so the next
  call redials cleanly.
- **Deadlines everywhere.** Dial (10s), and every `NewSession` / `Start` /
  `RequestPty` / `Shell` go through a `withDeadline` wrapper that drops the
  connection on expiry — so a half-open transport can't wedge a request past
  the kernel's TCP timeout. `Exec` bounds the whole command (default 60s;
  per-action overrides exist).
- **Output is capped** (`limitedBuffer`, default 1 MiB) so a runaway command
  can't exhaust memory.
- **Host-key policy** is injected per target (see §13).

`Drop(id)` closes and forgets a machine's connection; it's called on power
changes, host edits, host-key rejections, and reachability failures so the next
call starts fresh.

---

## 7. SSH reachability checks (frequency & backoff)

This is what keeps the second status dot honest **without anyone clicking
Probe**. Implemented in `internal/api/reach.go`.

### What the check does

For each eligible machine it runs the **cheapest possible liveness probe** over
the pooled SSH connection:

```
sh -s   with stdin:  exit 0
```

- `ExecOpts` timeout **12s**, output cap **256 bytes**, wrapped in a **15s**
  context.
- On success → `reach_state = ok`.
- On failure → the error is classified (`auth_failed`, `hostkey_changed`,
  `timeout`, `refused`, …), stored with its message, and the pooled connection
  is dropped so the next attempt redials.

This is deliberately **not** a full fingerprint — it reuses the warm pooled
connection (kept alive by the 30s keepalive), so a healthy check is nearly
free. (The manual **"Probe & refresh facts"** button is the heavyweight path:
it drops the connection first and re-runs the full fingerprint.)

### How often it runs

A single ticker wakes the poller **every 20 seconds** (`reachTick`) and runs
everything currently *due*, at most **4 checks concurrently** (`reachFanout`).
Each machine carries its own next-due time:

- **Healthy host → re-checked every ~60s** (`reachHealthy`). (Because the
  sweep ticks every 20s, the actual check lands on the first tick at or after
  the due time — so 60–80s in practice.)
- **Failing host → exponential backoff**, so a dead box isn't hammered:

  ```
  next_delay = 60s << fails      (capped at 15 minutes)
  ```

  | Consecutive failures | Delay until next check |
  |---|---|
  | 1 | 120s |
  | 2 | 240s |
  | 3 | 480s |
  | 4 | 900s (hits the 15-minute cap) |
  | 5+ | 900s |

  A single success resets `fails` to 0 → back to the 60s cadence.

- **Newly added hosts** are scheduled immediately (`next = now`) so they're
  checked on the very next tick.

### What it skips

- Machines with **no host identity** (`!HasHost()`) — nothing to SSH to.
- **Stopped cloud instances** (`HasCloud() && power_state == stopped`) — being
  unreachable is expected, so they aren't probed and don't show a red dot. On
  power-on, `powerMachine` drops the connection and resets reach to `unknown`,
  and the next sweep re-checks.

The scheduler map is pruned each sweep so entries for deleted machines don't
accumulate. The whole loop stops when the process context is cancelled.

---

## 8. Cloud power-state sync

`StartBackground(ctx, 2m)` runs `refreshPowerStates` **every 2 minutes**: for
each cloud account it calls `sync()` with a 60-second per-account timeout (one
stalled provider can't wedge the others). `sync()` is also what the **"Sync
now"** button and account creation call, and it is serialized by a mutex so the
timer and a manual sync can't race.

`sync()` is **additive**:

- New instances become machines (public IP pre-fills `ssh_host`).
- Known instances get only their **cloud-derived** columns refreshed —
  `credential_id`, `name`, and a user-set `ssh_host` are never touched.
- The **one exception**: if `ssh_host` was auto-tracking the public IP
  (`ssh_host == old public_ip`) and the IP changed (an ephemeral address across
  a stop/start), `ssh_host` follows the new IP, the stale pooled connection is
  dropped, and reach resets to `unknown`. A DNS name / bastion / Elastic IP you
  set yourself is left alone.
- Instances that vanished from the API are flagged `missing`, never deleted.

---

## 9. The action engine

`internal/actions` is a **declarative catalog** (`catalog.go`) plus an engine
(`engine.go`). Each `Action` declares: id, label, category, required
capabilities, a command template, typed params, a danger tier, a sudo policy,
and a parser. The UI renders from `GET /api/machines/{id}/actions`, which
returns only the actions the host's fingerprinted capabilities satisfy — so a
box without Docker never shows a Containers tab.

`engine.Prepare` (shared by the buffered and streaming paths) does, in order:

1. **Resolve** the action (built-in catalog, then saved custom actions).
2. **Gate** by danger tier (see below).
3. **Render** the command: every param is checked against an allowlist regex
   (or, for custom actions, a control-char + length check) and **single-quoted**
   before substitution — belt and braces against injection.
4. **Requires-check** against fingerprinted capabilities.
5. **Sudo decision** (see below).
6. **Resolve the target** (unseal the credential — §13).

Then `Run` executes over the broker, **parses** stdout into JSON the UI renders
(`parsers.go` — one parser per output shape, tested against fixtures), and
**writes an audit `Run` row before returning** — even on failure. An unrecorded
action is considered worse than a failed one.

**Transport.** Every action runs as `sh -s` with the rendered command fed on
**stdin**, not as an argv. This makes the remote login shell irrelevant (fish,
zsh, restricted shells), lets a single `sudo -n` prefix cover a whole `a || b`
chain, and keeps all quoting in one place. The audit row and UI preview show
the rendered command; `sh -s` is just the pipe.

### Danger tiers (the gate)

| Tier | Name | Gate | Examples |
|---|---|---|---|
| 0 | read | runs on click | `uptime`, `systemctl list-units`, `ss -tulpn` |
| 1 | reversible | one confirmation | `systemctl restart`, `docker restart` |
| 2 | disruptive | type the machine's name | `reboot`, `kill -9`, provider stop |
| 3 | forbidden | **no button, ever** | `rm -rf`, `mkfs`, provider *terminate* |

Tier 3 actions exist in the model only so the gate can refuse them; the UI
never renders one, and custom actions can't declare one.

### Sudo (never prompts)

`Action.Sudo` is tri-state and the `-n` flag guarantees CloudRoof never hangs
on a password prompt:

- `SudoRequired` — refuses (`ErrSudoUnavailable`) unless the host reports
  `root` or passwordless sudo (`nopasswd`).
- `SudoPreferred` — uses `sudo -n` when `nopasswd`, otherwise runs as the SSH
  user and lets the kernel refuse what it refuses.
- `SudoNone` — never prefixes.

The sudo mode is learned during fingerprinting (`root | nopasswd | password |
none`). The UI mirrors this in `canRun()` so it doesn't offer a button the
server would refuse.

---

## 10. Live streaming (logs / terminal)

Two websocket endpoints reuse the pooled connection:

- **Terminal** (`GET …/terminal`) — `OpenShell` requests a PTY and starts the
  login shell; binary frames carry bytes both ways, a JSON text frame carries
  `resize`. Opening a terminal writes an audit row (it's the one place a user
  can do anything).
- **Follow** (`GET …/actions/{action}/stream`) — for `Action.Stream` actions
  (`journalctl -f`, `docker logs -f`, `supervisorctl tail -f`). It goes through
  the same `engine.Prepare` gate, then `OpenStream` pipes merged stdout+stderr
  one line per frame until the client disconnects or the command ends. Stream
  actions are always Tier 0.

---

## 11. Groups, bulk actions, custom actions

- **Groups** (`internal/store` + `api/groups.go`) — a named set of tags. A
  machine belongs when it carries **all** the group's tags. Membership is
  **resolved live** from each machine's tags (never stored), so it tracks the
  fleet as tags change. A tagless group matches nothing (by design, so an empty
  group can't accidentally target everything).
- **Bulk** (`api/bulk.go`) — one action across a group or an explicit set,
  host-capable machines only. Confirmation **escalates** over the single-host
  gate: a Tier-0 read needs a click; a write requires typing the **number of
  affected hosts** after a preview that names them. Each machine then runs
  through the normal `engine.Run` (its own name satisfies the per-machine
  Tier-2 gate), audited individually, at a bounded concurrency of 5. You get a
  per-host result list.
- **Custom actions** (`api/custom_actions.go`) — admin-authored commands saved
  as buttons, resolved by the same engine path as built-ins. They carry named
  free-text params (shell-quoted, control-char and length checked — no
  author-supplied regex), a declared tier the gate enforces identically, and
  optional capability requirements. **Tier 3 is refused at creation** and by
  the gate. Output is shown raw.

---

## 12. Providers

`internal/provider` defines a deliberately small interface — `ListInstances`,
`GetPowerState`, `Power` — plus a registry. Each adapter self-registers in
`init()` with a **`Spec`** (the credential fields the UI should render) and a
factory; the account form and server-side validation both derive from the Spec,
so adding a provider needs **no UI changes**.

Power maps onto five normalized actions (`start`, `stop`, `reboot`,
`force_stop`, `force_reboot`). Whatever addressing a provider needs is encoded
**inside `instance_id`** so the interface stays cloud-agnostic:

| Provider | Auth | `instance_id` shape | SDK |
|---|---|---|---|
| Hetzner | API token | numeric id | hcloud-go v2 |
| DigitalOcean | API token | numeric id | godo |
| Vultr | API key | uuid | none (plain REST) |
| AWS EC2 | access key pair | `region:i-…` | aws-sdk-go-v2 |
| Azure | service principal | full ARM resource id | armcompute + armnetwork |
| Google Cloud | service-account JSON | `project/zone/name` | compute/v1 |

Each adapter is blank-imported behind a `//go:build !no<name>` tag in
`cmd/cloudroof/prov_*.go`, so any provider can be compiled out (`make slim`
drops the heavy AWS/Azure/GCP SDKs). A provider compiled out never registers.

---

## 13. How credentials are stored

CloudRoof holds the two most sensitive things in your fleet — **SSH private
keys** and **cloud API credentials** — so custody is designed around one rule:
**secrets are encrypted at rest, and plaintext exists only in memory for the
life of a connection.**

### Encryption (`internal/keyring`)

- **Cipher:** AES-256-GCM (authenticated encryption). A 32-byte **master key**
  is loaded once at startup into an AEAD.
- **Master key location & precedence:**
  1. `CLOUDROOF_MASTER_KEY` environment variable (base64 of 32 bytes) if set —
     intended for container users who inject secrets.
  2. Otherwise the **key file** at `<data-dir>/master.key`.
  3. If neither exists (first run) a key is generated with `crypto/rand` and
     written to the key file, **mode `0600`**, inside the data directory
     (**mode `0700`**).
- **Sealed blob format:** `bsn1:` + base64( `nonce ‖ ciphertext‖tag` ). The
  `bsn1:` prefix versions the format so a future algorithm/key-rotation change
  can be told apart. Each seal uses a fresh random 12-byte nonce.
- **Tamper/So-what:** GCM authenticates the ciphertext, so a modified blob fails
  to open. The **database alone is useless** to an attacker — decrypting needs
  the master key, which lives outside the DB. Moving the DB without its
  `master.key` yields a clear "wrong master key" error, not a crash.

### What is sealed, and where

- **SSH credentials** → `credentials.sealed`. The plaintext is a small JSON
  object (`sealedSecret`): `{privateKey, passphrase}` for a key, or `{password}`
  for a password. The `credentials` API type has `json:"-"` on the sealed
  field, so it is **never serialized to the frontend**; the API only ever
  returns the credential's name, kind, and fingerprint.
- **Cloud credentials** → `cloud_accounts.sealed_token`. The plaintext is a
  `provider.Credentials` JSON object (token, or key pair, or service principal,
  or service-account JSON, depending on the provider).

### Where plaintext appears

Exactly two places, both transient and in-memory:

- `api.resolveTarget` unseals an SSH credential to build an `sshx.Target` for
  one operation; the plaintext lives on that request's stack and inside the
  broker's live connection, never on disk.
- `api.providerFor` unseals a cloud token to construct a provider adapter for
  one call.

Nothing logs a command that contains a secret, and the catalog has no action
that takes a secret as a parameter.

### SSH host keys (trust on first use)

Separate from credentials, `host_keys` pins a machine's SSH **host key** on
first connect. On a later mismatch the observed key is recorded in `seen_*`,
the reach state becomes `hostkey_changed`, and **every action refuses** until
you review pinned-vs-seen in the UI and explicitly accept — at which point the
`hostkey/trust` endpoint requires you to echo back the exact seen fingerprint,
so nothing can be trusted that the host didn't actually present.

---

## 14. Security posture

- **Loopback by default.** Binds `127.0.0.1:7070`; reaching it from elsewhere
  is an explicit `-addr 0.0.0.0:7070 -hosts <name,ip>`. A non-loopback bind
  without `-hosts` logs a warning and accepts any Host (the exposure `-hosts`
  closes).
- **No authentication yet** (single admin, v1.5). The `guard` (§5) blocks the
  browser attack classes, but anyone who can reach the port can drive the API —
  so put CloudRoof behind an authenticating reverse proxy before exposing it.
  Multi-user/RBAC/SSO is a later milestone.
- **Secrets** — §13.
- **Blast radius** — danger tiers, bulk escalation, no Tier-3 buttons, per-host
  audit, and per-action timeouts/cancellation.

---

## 15. Configuration reference

Flags (and their environment equivalents), from `internal/config`:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-addr` | `CLOUDROOF_ADDR` | `127.0.0.1:7070` | listen address |
| `-hosts` | `CLOUDROOF_HOSTS` | (empty) | comma-separated Host-header allowlist for non-loopback binds |
| `-data` | `CLOUDROOF_DATA` | `~/.config/cloudroof` | data directory (holds `cloudroof.db` + `master.key`) |
| `-dev` | `CLOUDROOF_DEV` | off | serve the frontend from `web/dist` on disk instead of the embedded bundle |
| — | `CLOUDROOF_MASTER_KEY` | (unset) | base64 master key; overrides the key file |
| — | `CLOUDROOF_LOG` | `info` | `debug` / `info` / `warn` |

The data directory is created `0700`; `master.key` is written `0600`.

---

## 16. Build & deployment

- **`make build`** — `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X
  main.version=<v>"`. A static, path-trimmed single binary at `bin/cloudroof`.
- **`make web`** — Vite build into `web/dist/`, which **is committed** so that
  `go install` (which can't run the JS build) still embeds the UI. Rebuild and
  commit `web/dist/` whenever `web/src/` changes.
- **`make slim`** — build with `-tags "noaws noazure nogcp"`, dropping the heavy
  SDKs (~59 MB → ~16 MB). Any provider can be dropped with its `no<name>` tag.
- **`make release`** — cross-compiled tarballs (`linux/amd64`, `linux/arm64`,
  `darwin/amd64`, `darwin/arm64`) + `checksums.txt` in `dist/`.
- **`make test`** — `go vet` + unit tests. **`make e2e`** — the full stack
  against a throwaway local `sshd`.

Requires **Go ≥ 1.26** (a floor set by `golang.org/x/crypto`); `GOTOOLCHAIN=auto`
fetches it automatically.
