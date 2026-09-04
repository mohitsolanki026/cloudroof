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

## Network exposure

There is no login yet. Bosun therefore binds to `127.0.0.1:7070` by default,
refuses requests whose `Host` header is not itself (DNS rebinding), and
refuses cross-origin browser requests (CSRF). To reach it from another
machine, opt in explicitly and name the hosts you will use:

```
./bin/bosun -addr 0.0.0.0:7070 -hosts bosun.lan,10.0.0.5
docker run -d -p 7070:7070 -e BOSUN_HOSTS=bosun.lan -v bosun:/data bosun
```

Binding a network address without `-hosts` works but accepts any `Host` and
logs a warning at startup. Put Bosun behind a reverse proxy that adds
authentication and TLS before exposing it beyond a trusted network.
Multi-user auth is on the roadmap (v2.5).

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
