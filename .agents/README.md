# CloudRoof — brief for agents and models

Read this first. It says what the project is, how it is laid out, and the
rules that are not obvious from the code.

## What this is

**CloudRoof** is a self-hosted, agentless dashboard for small fleets (1–20) of
Linux machines across multiple cloud providers. One Go binary, one SQLite
file, one port. It does two things and links them:

1. **Cloud identity** — via provider APIs: list instances, read power state,
   start / stop / reboot.
2. **Host identity** — via SSH: fingerprint the box, run a curated catalog of
   ops actions as buttons, open a real terminal.

A `Machine` row holds both halves and either may be empty. Both present is
the "linked" case the product is built around: when the API says *running*
and SSH says *unreachable*, one card shows both and offers a hard reboot.

The product design doc (principles, safety tiers, scope, roadmap) is the
canonical statement of intent. Its principles are enforced in code:

| Principle | Where it lives |
|---|---|
| Every button shows its command | `renderCommand` in `web/src/api.ts`; `Confirm.tsx` shows it before Tier 1/2 |
| Read is free, write is deliberate | `gate()` in `internal/actions/engine.go` |
| Machine-readable output first | `internal/actions/parsers.go` — `--plain`, `-P`, `-H`, kv scripts |
| Detect, never assume | `internal/facts` → `actions.Available(caps)` → tabs |
| Terminal one click away | `internal/api/terminal.go` + `web/src/components/Terminal.tsx` |
| Keys never leave your box | `internal/keyring` — AES-256-GCM, master key on disk, plaintext only in `api.resolveTarget` |
| Everything is audited | `store.Run` — engine writes one per action, including failures; terminal opens are logged |

## Layout

```
cmd/cloudroof/            entrypoint; wires config → keyring → store → broker → api
internal/config/      flags + env; data dir default ~/.config/cloudroof
internal/keyring/     Seal / OpenSealed. Master key file 0600, or CLOUDROOF_MASTER_KEY env.
internal/store/       SQLite (modernc, pure Go). schema.go = migrations (append-only),
                      models.go = types, queries.go = all SQL.
internal/sshx/        Connection broker. One pooled *ssh.Client per machine ID.
                      Exec (with stdin + timeout + output cap) and OpenShell (PTY).
                      Typed errors: ErrAuth, ErrTimeout, ErrRefused, ErrHostKeyMismatch.
internal/facts/       Fingerprint script (POSIX sh via `sh -s` on stdin) → HostFacts.
internal/provider/    Provider interface + registry + Credentials/Spec. Adapters
                      self-register in init() with a Spec (the fields the UI
                      renders) and a Factory. New(name, creds) validates
                      against the spec before constructing.
internal/provider/hetzner/       token          (hcloud-go v2)
internal/provider/digitalocean/  token          (godo)
internal/provider/amazon/        key pair       (aws-sdk-go-v2/ec2); id = "region:i-…"
internal/provider/azure/         service principal (armcompute+armnetwork); id = full ARM resource id
internal/provider/google/        service acct JSON (compute/v1); id = "project/zone/name"
internal/provider/vultr/         token          (plain REST, no SDK); id = instance uuid
                      Each cmd/cloudroof/prov_*.go blank-imports one adapter behind
                      a build tag (no<name>) so it can be compiled out.
internal/actions/     catalog.go = declarative Action list; engine.go = Prepare
                      (gate → render → requires → sudo → resolve) shared by
                      Run (buffered) and streaming; parsers.go = stdout → JSON;
                      parsers_test.go = a fixture per format.
internal/api/         HTTP. server.go = routes, guard, resolveTarget, host-key
                      policy, error mapping. handlers.go = REST. sync.go =
                      cloud → machines (serialized by syncMu). terminal.go =
                      websocket PTY bridge. stream.go = websocket log follow
                      (journalctl -f / docker logs -f). reach.go = background
                      reachability poller with backoff.
web/                  Vite + React 18 + TS. embed.go embeds dist/ into the binary.
web/src/api.ts        Typed client; mirrors Go DTOs by hand.
web/src/pages/        Fleet, Machine (tabs), Activity, Settings.
```

## Invariants — do not break these

- **Never a Tier 3 button.** `DangerForbidden` actions exist in the catalog only
  as documentation of what is refused. `gate()` returns `ErrForbidden` for
  them. Do not add `rm -rf`, `mkfs`, `dd`, `userdel`, or provider *terminate*
  as actions.
- **Every param has a `Pattern`** and every rendered value is single-quoted.
  A param without a regex is a shell-injection bug, not a style issue.
- **Audit before return.** `engine.Run` inserts the `Run` row before returning
  to the caller, even on failure. If the insert fails, that is the error.
- **Sync is additive, with one deliberate exception.** `UpsertFromCloud`
  refreshes cloud-derived columns and never touches `credential_id` or `name`.
  It updates `ssh_host` *only* when it was auto-tracking the public IP
  (`ssh_host == old public_ip`) — an ephemeral cloud IP changes across a
  stop/start, so a machine that was following it keeps following. A user-set
  `ssh_host` (DNS name, bastion, Elastic IP) is never overwritten. When it
  does follow, `UpsertFromCloud` returns `hostChanged` and the sync layer
  drops the stale pooled connection and resets reachability. Vanished
  instances are flagged `missing`, never deleted. (`TestUpsertFollowsPublicIP`.)
- **Audit outlives machines.** `runs.machine_id` is `ON DELETE SET NULL` and
  `machine_name` is denormalized. Do not change this to CASCADE.
- **Host keys pin on first sight and block on change.** On mismatch the policy
  in `api.hostKeyPolicy` records the observed key into `host_keys.seen_*`,
  surfaces `ReachHostKeyChanged`, and every action refuses. `POST
  hostkey/trust` accepts only a fingerprint equal to `seen_fingerprint` — the
  UI shows pinned vs. seen and echoes the seen one back. Nothing can be
  trusted that the host did not actually present.
- **Plaintext secrets exist only in `api.resolveTarget`** and the broker's
  live connection. Nothing logs a command with a secret in it; the catalog
  has no actions that take secrets as params.
- **Sudo never prompts.** `Action.Sudo` is tri-state (`actions.Sudo`):
  `SudoRequired` refuses with `ErrSudoUnavailable` unless facts report `root`
  or `nopasswd`; `SudoPreferred` prefixes `sudo -n` only when `nopasswd` and
  otherwise runs as the SSH user; `SudoNone` never prefixes. The `-n` flag is
  what guarantees no prompt. Never buffer a password into a PTY. The UI
  mirrors this in `canRun()` in `web/src/api.ts`.
- **Every `/api` request passes `guard`** (`server.go`): the Host must be in
  the allowlist (loopback + `-hosts`), `Origin` / `Sec-Fetch-Site` must be
  same-origin when present, and a state-changing body must be
  `application/json`. The websocket upgrader uses the same check. This is
  the CSRF and DNS-rebinding defense standing in for auth until v2.5; do not
  loosen it to make a client "just work" — add the host to `-hosts`.
- **Every action executes via `sh -s`** with the rendered command on stdin
  (`engine.Run`). The audit row and the UI preview show the rendered command
  with its `sudo -n` prefix; `sh -s` is the transport. This is what makes the
  remote login shell irrelevant and lets sudo cover a whole `a || b`.
- **Nothing in the broker blocks without a deadline.** `NewSession`, `Start`,
  `RequestPty`, `Shell` and keepalive replies go through `withDeadline`,
  which drops the connection on expiry. Any new call into `*ssh.Client` or
  `*ssh.Session` that can block needs the same wrapper.
- **Custom actions share the built-in execution path.** A saved action is
  resolved by `engine.lookup` (built-ins win a name clash), converted with
  `actions.FromCustom`, and run through the same gate/render/sudo/audit as a
  built-in. Its params have no author regex, so `render` falls back to
  `safeFreeParam` (control-char + length check) and still single-quotes.
  Creation refuses danger 3 and the gate refuses it too — a custom Tier-3
  button cannot exist. Output is always `raw`.
- **Bulk escalates the gate, never bypasses it.** `runBulk` resolves targets
  (a group or an explicit set, host-capable only), applies an escalated gate
  (read → one confirm; write → type the host count, after a named preview),
  then runs each machine through `engine.Run` with that machine's own name
  satisfying the per-machine Tier-2 gate. Every host is audited as its own
  Run. Concurrency is bounded (`bulkConcurrency`).
- **Migrations are append-only — this is now load-bearing, not aspirational.**
  `store/schema.go` has two shipped migrations. 001 is frozen: it must match
  what v0.1 created. Editing 001 in place is exactly the bug that shipped the
  seen_* columns to fresh databases but not to existing ones (handshake failed
  with "no such column: seen_algorithm"); 002 repaired it with ALTER TABLE. Add
  a new string to the slice; never touch a shipped one. The runner
  (`execMigration`) strips `--` comments (so a ';' in a comment can't split a
  statement — that shipped as a bug in 003 and was fixed), applies statements
  one at a time, and treats "duplicate column name" as already-applied. Three
  migrations shipped: 001 base, 002 host-key seen_*, 003 groups + custom_actions.
- **Streaming actions are Tier 0.** An `Action.Stream` action runs over the
  websocket in `stream.go`, not the buffered path, and `TestCatalogIntegrity`
  fails the build if a stream action is not read-tier. It still goes through
  `engine.Prepare`, so the gate, requires-check, sudo decision and quoting are
  identical to a buffered action.
- **`render()` allows literal `{{…}}`** that is not a declared param, because
  some commands carry Go-template braces (docker `--format '{{json .}}'`). The
  safety net is instead: every declared param must appear in the template, and
  every param value is regex-checked and single-quoted. Static command text is
  trusted; only param values are not.
- **Providers register a Spec, not just a factory.** The Spec lists the
  credential fields; the account form and server validation both derive from
  it, so a new adapter needs no UI change. Secrets are sealed as a
  `provider.Credentials` JSON object (v0.1's bare-token blobs still load).

## How things flow

**Add SSH machine** → `POST /api/machines` → user probes → `probeMachine` →
`resolveTarget` (unseal) → `facts.Fingerprint` (dial, pin host key, run
script) → `UpsertFacts` + `SetReach(ok)`.

**Connect cloud** → `GET /api/providers` gives the UI each provider's Spec →
`POST /api/accounts` with `{name, provider, credentials}` validates against the
spec and with one real `ListInstances` → `sync()` → `UpsertFromCloud` per
instance (public IP pre-fills `ssh_host`) → user links via Settings "Link
synced instances" → probe.

**Follow logs** → `GET /api/machines/{id}/actions/{action}/stream?params…`
(a Stream action) upgrades to WS → `engine.Prepare` → `broker.OpenStream`
runs the command, merged stdout+stderr piped one line per frame until the
client closes.

**Reachability** → `reach.go` sweeps every host machine on a timer, runs a
cheap `exit 0` over the pooled connection, and records reach state with
per-host backoff (60s healthy → up to 15min while failing). Stopped cloud
instances are skipped, not flagged red.

**Run a button** → UI checks `action.danger`; Tier 1/2 opens `Confirm` with
the rendered command → `POST /api/machines/{id}/actions/{action}` with
`confirm` / `confirmName` → `engine.Run`: gate → requires-check against facts
→ render (regex + quote) → sudo decision → `broker.Exec("sh -s", stdin=command)`
→ parse → `InsertRun` → return `{run, data}`.

**Terminal** → `GET /api/machines/{id}/terminal` upgrades to WS → `OpenShell`
(PTY on the pooled connection) → binary frames both ways, JSON `resize`.

## Running it

```
make web && make build && ./bin/cloudroof -data ./data     # production-shaped
make dev   # backend on :7070 serving web/dist from disk
cd web && npm run dev   # vite on :5173 proxying /api → :7070
```

Data dir holds `cloudroof.db` and `master.key`. Deleting `master.key` makes every
stored credential unreadable; there is no recovery by design.

## Adding things

**A provider**: implement `provider.Provider` in
`internal/provider/<name>/`, call `provider.Register(Spec, Factory)` in
`init()` with the credential fields the UI should render, and add a
`cmd/cloudroof/prov_<name>.go` that blank-imports it behind a `//go:build !no<name>`
tag. Add any new credential fields to `provider.Credentials` (+ `Get`/`Empty`).
Map the provider's states onto `store.PowerState` and its power calls onto the
five `PowerAction`s; encode whatever addressing it needs (region, zone,
resource group) inside `instance_id` so the interface stays cloud-agnostic. Do
not add provider-specific columns to `Machine`. Instances must return an IP in
`PublicIP` where possible so `ssh_host` can pre-fill (Azure needs the network
API for this; it is best-effort there).

**An action**: append to `actions.Catalog`. Set `Requires` to the capability
the fingerprint script would need to detect (add it to the `for c in …` loop
in `facts.go` if new). Set `Danger` honestly. Give every param a `Pattern`.
Pick or write a parser. If output is multi-line or needs quoting, use
`Script` (fed to `sh -s`) instead of `Command`.

**A tab**: tabs are derived from action categories in `Machine.tsx`
(`TAB_ORDER` / `TAB_LABEL`). A new category needs a component and an entry
there.

## State of the build

v1.5. Six providers (Hetzner, DigitalOcean, AWS EC2, Azure, GCP, Vultr), each
compile-out-able via build tags; the systemd/supervisor/docker/nginx/web/
process/network/disk catalog; live log streaming; self-refreshing
reachability; tag-based groups; bulk actions with escalated confirmation;
saved custom actions; single admin user. No metrics history, no multi-user —
later milestones. Linode was on the v1.5 roadmap but deliberately skipped.

Known gaps to be honest about:
- `processes.top` uses procps `ps` flags; BusyBox `ps` will return nothing useful.
- `network.*` needs `ss` (iproute2); no `netstat` fallback yet.
- Cloud SDKs dominate the binary: all five providers ~59 MB stripped,
  Hetzner+DO alone ~16 MB. Build tags (`make slim`, or `-tags no<name>`) let a
  user drop the heavy ones. This is the one place the "one small binary" story
  needs a caveat.
- Azure IPs come from the network API and are best-effort: with only Compute
  permissions the VM lists and powers but `ssh_host` stays empty until set via
  Edit connection.
- No auth on the HTTP API. The default bind is loopback and `guard` blocks
  browser cross-site and rebinding attacks, but anyone who can reach the port
  can drive it. Put it behind a reverse proxy with auth. Multi-user auth is a
  later milestone.
- Migrations are append-only as of v1.0: two shipped migrations, 001 frozen.
  Add new ones; never edit a shipped one. See the invariant above.

## Adversarial review (2026-09-04)

Six reviewers (security, broker concurrency, store, handlers, parsers/facts,
frontend) each produced findings; every finding was then attacked by three
independent refuters and survived only on a majority. 29 survived and all
were fixed the same day; six were refuted. The ones worth knowing because
they shaped the code:

- REST had no CSRF or rebinding defense and bound all interfaces → `guard`
  and the loopback default.
- `runAction` was recreated on every `load()` and its error path called
  `load()` → tab effects could loop forever. It now reads the machine through
  a ref and is stable per machine.
- `sudo -n a || b` only covered `a` → every action runs under `sh -s`.
- Read-tier actions that failed on the host rendered as empty tables → the
  UI toasts exit code + first stderr line whenever the data looks empty.
- `NewSession` and keepalive could block past every deadline on a half-open
  transport → `withDeadline`.
- `deleteCredential` closed every pooled connection → it drops only the
  machines that used it. Deleting a cloud account left orphaned cloud columns
  → stripped in the same transaction.
- IPv6 SSH hosts were joined with `%s:%d` → `net.JoinHostPort`.
- Confirm's window-level Enter fired Run while Cancel had focus → Enter lives
  on the name input; Tier 1 relies on the focused button's own activation.

## Verified end-to-end — `make e2e` + `make test` (2026-09-04)

`go test ./...` covers every parser against a real fixture per format
(systemd, ps, ss listening *and* established, du, supervisor, docker ps,
inodes, tls, overview incl. the no-MemAvailable fallback), the catalog
invariants (no Tier-3 button, every param has a pattern, stream actions are
Tier 0), render quoting + literal-brace handling, and the keyring
(round-trip, persistence, wrong-key and tamper rejection).

`scripts/e2e.sh` drives the real binary through 62 checks against a throwaway
unprivileged sshd on 127.0.0.1:2222 (own keys and config, touches nothing
else): gate codes and injection rejection before any host contact; the
browser guard (415 on non-JSON, 403 on foreign Origin / cross-site, 421 on
unknown Host, same-origin passes); probe pins a key equal to sshd's real
fingerprint; the catalog offered matches the host's capabilities; overview /
services / processes / network (listening + established) / disk (usage +
inodes) parse real output; `network.reach` hits CloudRoof's own health endpoint
from the host; `web.tls_cert` on a non-TLS port reports `ok:false` not blanks;
the three providers register with the AWS key-pair spec; `SudoPreferred` runs
plain and `SudoRequired` is refused on a password-sudo host; every executed
action is audited and refused ones are not; the terminal round-trips, resizes,
closes cleanly and is audited; a `journalctl -f` follow streams over the
websocket and is audited; then sshd is restarted with a new host key and the
probe is refused, the seen fingerprint recorded and returned, a forged trust
rejected, the real one accepted, the pin replaced.

Run both after any change to sshx, actions, facts, providers, or api.

One behavior the suite pinned down: **probe always drops the pooled connection
first** (`probeMachine`). A reused connection never re-checks the host key or
auth; a probe has to.

## License

AGPL-3.0-only (`LICENSE`), dual-licensed for a commercial tier. Keep source
files' `SPDX-License-Identifier: AGPL-3.0-only` header; new Go files should
carry it (below the `//go:build` line where present). Contributions are under
the DCO — see `CONTRIBUTING.md`.
