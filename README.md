# Bosun

One self-hosted dashboard for every Linux machine you own, across every cloud
you rent from — power control from the provider API, and a button layer over
the commands you keep re-typing through SSH.

Nothing is installed on your machines. No credential leaves your box.

```
docker run -d -p 7070:7070 -v bosun:/data bosun
```

or

```
make web && make build
./bin/bosun -data ./data
```

then open http://localhost:7070.

## What it does

- **Fleet view** — every machine across every provider in one list, with two
  status dots: what the cloud says (running / stopped) and what SSH saw
  (reachable / refused / timeout / auth failed). Their disagreement is the
  signal.
- **Power** — start, shut down, reboot, force-stop, force-reboot via the
  provider API. For when SSH is dead.
- **Overview** — uptime, load, memory, filesystems, sessions, reboot-required.
  One batched probe per refresh.
- **Services** — systemd units with start / stop / restart / journal per row.
- **Processes** — top by CPU with TERM / KILL.
- **Network** — listening ports mapped to owning process.
- **Disk** — largest directories, journal vacuum.
- **Terminal** — a real shell on the pooled SSH connection.
- **Activity** — every command Bosun has ever run, with output, append-only.

## Safety

Every action carries a tier. Tier 0 runs on click. Tier 1 asks once. Tier 2
makes you type the machine's name. Tier 3 — `rm -rf`, `mkfs`, terminate —
does not exist as a button and never will.

Every button shows the exact command before it runs and logs it after.

## Providers

v0.1: Hetzner Cloud. DigitalOcean and AWS EC2 are next.

Machines that are not on a supported provider (bare metal, a Pi, a provider
we don't speak yet) work fine — add them by SSH and you get everything except
power control.

## Development

```
make dev              # Go backend on :7070, serving web/dist from disk
cd web && npm run dev # Vite on :5173 with /api proxied to :7070
make test
```

See `.agents/README.md` for the architecture brief and invariants.

## Status

v0.1 — proof slice. Not yet suitable for anything you'd be sad to lose.
