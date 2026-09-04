# Bosun — brief for agents and models

Read this first. It says what the project is, how it is laid out, and the
rules that are not obvious from the code.

## What this is

**Bosun** is a self-hosted, agentless dashboard for small fleets (1–20) of
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
cmd/bosun/            entrypoint; wires config → keyring → store → broker → api
internal/config/      flags + env; data dir default ~/.config/bosun
internal/keyring/     Seal / OpenSealed. Master key file 0600, or BOSUN_MASTER_KEY env.
internal/store/       SQLite (modernc, pure Go). schema.go = migrations (append-only),
                      models.go = types, queries.go = all SQL.
internal/sshx/        Connection broker. One pooled *ssh.Client per machine ID.
                      Exec (with stdin + timeout + output cap) and OpenShell (PTY).
                      Typed errors: ErrAuth, ErrTimeout, ErrRefused, ErrHostKeyMismatch.
internal/facts/       Fingerprint script (POSIX sh via `sh -s` on stdin) → HostFacts.
internal/provider/    Provider interface + registry. Adapters self-register in init().
internal/provider/hetzner/   First adapter (hcloud-go v2).
internal/actions/     catalog.go = declarative Action list; engine.go = gate → render
                      → exec → parse → audit; parsers.go = stdout → JSON.
internal/api/         HTTP. server.go = routes, resolveTarget, host-key policy, error
                      mapping. handlers.go = REST. sync.go = cloud → machines.
                      terminal.go = websocket PTY bridge.
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
- **Sync is additive.** `UpsertFromCloud` refreshes only cloud-derived columns
  and never touches `ssh_*`, `credential_id`, or `name`. Vanished instances
  are flagged `missing`, never deleted.
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
- **Migrations are append-only.** Edit `store/schema.go` by adding a new
  string to the slice. Never modify a shipped one.

## How things flow

**Add SSH machine** → `POST /api/machines` → user probes → `probeMachine` →
`resolveTarget` (unseal) → `facts.Fingerprint` (dial, pin host key, run
script) → `UpsertFacts` + `SetReach(ok)`.

**Connect cloud** → `POST /api/accounts` validates the token with one
`ListInstances` → `sync()` → `UpsertFromCloud` per instance (public IP
pre-fills `ssh_host`) → user links via Settings "Link synced instances" (adds
user + credential) → probe.

**Run a button** → UI checks `action.danger`; Tier 1/2 opens `Confirm` with
the rendered command → `POST /api/machines/{id}/actions/{action}` with
`confirm` / `confirmName` → `engine.Run`: gate → requires-check against facts
→ render (regex + quote) → sudo prefix → `broker.Exec` → parse → `InsertRun`
→ return `{run, data}`.

**Terminal** → `GET /api/machines/{id}/terminal` upgrades to WS → `OpenShell`
(PTY on the pooled connection) → binary frames both ways, JSON `resize`.

## Running it

```
make web && make build && ./bin/bosun -data ./data     # production-shaped
make dev   # backend on :7070 serving web/dist from disk
cd web && npm run dev   # vite on :5173 proxying /api → :7070
```

Data dir holds `bosun.db` and `master.key`. Deleting `master.key` makes every
stored credential unreadable; there is no recovery by design.

## Adding things

**A provider**: implement `provider.Provider` in
`internal/provider/<name>/`, call `provider.Register` in `init()`, import it
for side effect in `cmd/bosun/main.go`. Map the provider's states onto
`store.PowerState` and its power calls onto the five `PowerAction`s. Do not
add provider-specific fields to `Machine`.

**An action**: append to `actions.Catalog`. Set `Requires` to the capability
the fingerprint script would need to detect (add it to the `for c in …` loop
in `facts.go` if new). Set `Danger` honestly. Give every param a `Pattern`.
Pick or write a parser. If output is multi-line or needs quoting, use
`Script` (fed to `sh -s`) instead of `Command`.

**A tab**: tabs are derived from action categories in `Machine.tsx`
(`TAB_ORDER` / `TAB_LABEL`). A new category needs a component and an entry
there.

## State of the build

v0.1 — proof slice. Hetzner only; systemd only for services; single admin
user; no metrics history; no bulk actions; no groups. See the design doc's
roadmap for what comes next and, more importantly, what is deliberately
excluded from v1.

Known gaps to be honest about:
- `processes.top` uses procps `ps` flags; BusyBox `ps` will return nothing useful.
- `network.ports` needs `ss` (iproute2); no `netstat` fallback yet.
- Reachability is refreshed on probe and on action failure, not on a timer.
  Power state *is* refreshed on a 2-minute timer via `StartBackground`.
- No auth on the HTTP API. Bind to localhost or put it behind a reverse
  proxy with auth. Multi-user auth is v2.5.
- Schema migration 001 is still being edited in place because nothing has
  shipped; the append-only rule starts at the first tagged release.

## Verified end-to-end — `make e2e` (2026-09-04)

`scripts/e2e.sh` starts a throwaway unprivileged `sshd` on 127.0.0.1:2222
(own host key, own client key, own config, touches nothing else) and drives
the real binary through 45 checks: gate codes (428/400/409) and injection
rejection before any host contact; probe pins a key equal to sshd's real
fingerprint; the catalog offered matches the host's capabilities; overview /
services.list / services.status / services.journal / processes.top /
network.ports / disk.largest parse real output; `SudoPreferred` actions run
plain on a password-sudo host and `SudoRequired` ones are refused; every
executed action (including failed ones) is audited and refused requests are
not; terminal websocket round-trips, honors resize, closes cleanly, and is
audited; then sshd is fully restarted with a new host key and the probe is
refused with `hostkey_changed`, the seen fingerprint is recorded and returned
in `details`, a forged trust is rejected, the real one is accepted, and the
pin is replaced. Run it after any change to sshx, actions, facts, or api.

One behavior it pinned down: **probe always drops the pooled connection
first** (`probeMachine`). A reused connection never re-checks the host key
or auth; a probe has to.
