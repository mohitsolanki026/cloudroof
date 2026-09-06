#!/usr/bin/env bash
# End-to-end test against a throwaway, unprivileged sshd on localhost.
#
# Touches nothing outside $E2E_DIR: generates its own host key, its own client
# key, its own sshd_config, and only accepts that client key. Bosun runs with
# its own data dir. Everything is killed on exit.
#
# Needs: go, node >= 22 (native WebSocket), python3, /usr/sbin/sshd, ssh-keygen.
# Usage: scripts/e2e.sh            (from anywhere)
#        E2E_KEEP=1 scripts/e2e.sh (leave $E2E_DIR behind for inspection)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
D=${E2E_DIR:-$(mktemp -d -t bosun-e2e.XXXXXX)}
SSH_PORT=${E2E_SSH_PORT:-2222}
API_PORT=${E2E_API_PORT:-7171}
B="http://127.0.0.1:$API_PORT"
export D B SSH_PORT

# sshd forks a child per connection; SIGTERM to the listener leaves those
# children serving live sessions. Kill everything started from our config.
stop_sshd() {
  pkill -f "sshd -f $D/sshd/sshd_config" 2>/dev/null || true
  for _ in $(seq 1 50); do pgrep -f "sshd -f $D/sshd/sshd_config" >/dev/null || break; sleep 0.1; done
  wait "${SSHD_PID:-}" 2>/dev/null || true
}

cleanup() {
  set +e
  [[ -n "${BOSUN_PID:-}" ]] && kill "$BOSUN_PID" 2>/dev/null
  stop_sshd
  wait 2>/dev/null
  [[ -z "${E2E_KEEP:-}" ]] && rm -rf "$D" || echo "kept $D"
}
trap cleanup EXIT

test -x /usr/sbin/sshd || { echo "need /usr/sbin/sshd"; exit 1; }
node -e 'if (typeof WebSocket !== "function") process.exit(1)' || { echo "need node >= 22"; exit 1; }

echo "== build =="
(cd "$ROOT/web" && npm run build >/dev/null 2>&1) && echo "web ok"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$D/bosun" ./cmd/bosun) && echo "go ok"

echo "== throwaway sshd on 127.0.0.1:$SSH_PORT =="
# Start from empty: a reused E2E_DIR would otherwise make ssh-keygen prompt
# to overwrite, and with no stdin that prompt fails under set -e.
rm -rf "$D/sshd" "$D/data" "$D/client" "$D/client.pub" "$D/bosun.log" "$D/mid" "$D/fails1" "$D/fails3"
mkdir -p "$D/sshd" "$D/data"; chmod 700 "$D/sshd"
ssh-keygen -q -t ed25519 -N '' -f "$D/client" -C bosun-e2e
ssh-keygen -q -t ed25519 -N '' -f "$D/sshd/hostkey"
cp "$D/client.pub" "$D/sshd/authorized_keys"; chmod 600 "$D/sshd/authorized_keys"

write_sshd_config() {
  cat > "$D/sshd/sshd_config" <<EOF
Port $SSH_PORT
ListenAddress 127.0.0.1
HostKey $D/sshd/hostkey
PidFile $D/sshd/sshd.pid
AuthorizedKeysFile $D/sshd/authorized_keys
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
LogLevel VERBOSE
EOF
}
start_sshd() {
  /usr/sbin/sshd -f "$D/sshd/sshd_config" -D -e >> "$D/sshd/log" 2>&1 &
  SSHD_PID=$!
  for _ in $(seq 1 50); do (echo >"/dev/tcp/127.0.0.1/$SSH_PORT") 2>/dev/null && return 0; sleep 0.1; done
  echo "sshd did not come up"; cat "$D/sshd/log"; exit 1
}
write_sshd_config; start_sshd
ssh -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes \
    -i "$D/client" -p "$SSH_PORT" "$USER@127.0.0.1" 'echo direct-ssh-ok'

echo "== bosun on $B =="
"$D/bosun" -addr "127.0.0.1:$API_PORT" -data "$D/data" > "$D/bosun.log" 2>&1 &
BOSUN_PID=$!
for _ in $(seq 1 50); do curl -sf "$B/api/health" >/dev/null && break; sleep 0.1; done

# ---------------------------------------------------------------------------
# Phase 1: setup, gates, probe, actions, sudo semantics, audit.
# ---------------------------------------------------------------------------
python3 - <<'PY'
import base64, hashlib, json, os, sys, urllib.request, urllib.error

B, D, USER = os.environ["B"], os.environ["D"], os.environ["USER"]
SSH_PORT = int(os.environ["SSH_PORT"])
fails = []

def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else (b"" if method == "POST" else None)
    req = urllib.request.Request(B + path, data=data, method=method, headers={"Content-Type": "application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=120); t = r.read()
        return r.status, (json.loads(t) if t else None)
    except urllib.error.HTTPError as e:
        t = e.read()
        try: return e.code, json.loads(t)
        except Exception: return e.code, t.decode()

def check(name, cond, detail=""):
    print(("  ok   " if cond else "  FAIL ") + name + (f"  ({detail})" if detail else ""))
    if not cond: fails.append(name)

def fp_of_pubkey_file(path):
    blob = base64.b64decode(open(path).read().split()[1])
    return "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")

print("-- setup --")
st, cred = call("POST", "/api/credentials", {"name": "e2e key", "kind": "ssh_key",
    "privateKey": open(D + "/client").read(), "passphrase": "", "password": ""})
check("create credential", st == 201 and cred["fingerprint"] == fp_of_pubkey_file(D + "/client.pub"))
st, m = call("POST", "/api/machines", {"name": "localbox", "tags": ["e2e"], "sshHost": "127.0.0.1",
    "sshPort": SSH_PORT, "sshUser": USER, "credentialId": cred["id"], "cloudAccountId": None, "instanceId": "", "provider": ""})
check("create machine", st == 201); mid = m["id"]

print("-- gates & validation, before any host contact --")
st, r = call("POST", f"/api/machines/{mid}/actions/services.restart", {"params": {"unit": "nginx"}, "confirm": False, "confirmName": ""})
check("tier1 without confirm -> 428 needs_confirm", st == 428 and r["code"] == "needs_confirm")
st, r = call("POST", f"/api/machines/{mid}/actions/processes.kill", {"params": {"pid": "1"}, "confirm": True, "confirmName": "wrong"})
check("tier2 wrong name -> 428 needs_name", st == 428 and r["code"] == "needs_name")
st, r = call("POST", f"/api/machines/{mid}/actions/services.status", {"params": {"unit": "nginx; rm -rf /"}, "confirm": False, "confirmName": ""})
check("shell injection in param -> 400", st == 400 and r["code"] == "bad_request")
st, r = call("POST", f"/api/machines/{mid}/actions/services.status", {"params": {"unit": "$(id)"}, "confirm": False, "confirmName": ""})
check("subshell in param -> 400", st == 400)
st, r = call("POST", f"/api/machines/{mid}/actions/disk.largest", {"params": {"path": "../etc"}, "confirm": False, "confirmName": ""})
check("relative path param -> 400", st == 400)
st, r = call("POST", f"/api/machines/{mid}/actions/services.status", {"params": {}, "confirm": False, "confirmName": ""})
check("missing param -> 400", st == 400)
st, r = call("POST", f"/api/machines/{mid}/actions/nope.nothing")
check("unknown action -> 400", st == 400)

print("-- browser guard: CSRF and DNS rebinding --")
API_PORT = B.rsplit(":", 1)[1]
def raw(method, path, headers, body=b""):
    req = urllib.request.Request(B + path, data=body, method=method, headers=headers)
    try: return urllib.request.urlopen(req, timeout=30).status
    except urllib.error.HTTPError as e: return e.code
check("text/plain body -> 415 (no-preflight CSRF shape)", raw("POST", f"/api/machines/{mid}/actions/system.overview", {"Content-Type": "text/plain"}, b"{}") == 415)
check("foreign Origin -> 403", raw("POST", f"/api/machines/{mid}/actions/system.overview", {"Content-Type": "application/json", "Origin": "http://attacker.example"}, b"{}") == 403)
check("cross-site Sec-Fetch-Site -> 403", raw("POST", f"/api/machines/{mid}/probe", {"Sec-Fetch-Site": "cross-site"}) == 403)
check("unknown Host header -> 421 (rebinding)", raw("GET", "/api/machines", {"Host": "attacker.example"}) == 421)
check("same-origin Origin passes", raw("GET", "/api/machines", {"Origin": f"http://127.0.0.1:{API_PORT}"}) == 200)
st, r = call("POST", f"/api/machines/{mid}/actions/services.list")
check("valid action before facts -> 409", st == 409 and r["code"] == "not_available")
st, runs = call("GET", f"/api/runs?machine={mid}")
check("no audit rows from refused requests", len(runs) == 0)

print("-- probe --")
st, r = call("POST", f"/api/machines/{mid}/probe")
check("probe ok", st == 200 and r["reachState"] == "ok", f"{st} {r if st != 200 else ''}")
if st != 200: sys.exit(1)
f = r["facts"]; sudo = f["sudoMode"]
print(f"       {f['osName']} {f['osVersion']} · {f['kernel']} · {f['initSystem']} · sudo={sudo} · caps={' '.join(f['capabilities'])}")
st, hk = call("GET", f"/api/machines/{mid}/hostkey")
check("pinned key matches sshd's real key", hk["fingerprint"] == fp_of_pubkey_file(D + "/sshd/hostkey.pub"))
check("no seen-mismatch recorded", hk["seenFingerprint"] == "")

print("-- catalog derived from facts --")
st, acts = call("GET", f"/api/machines/{mid}/actions")
ids = {a["id"] for a in acts}
if "systemctl" in f["capabilities"]: check("systemd host exposes services.*", "services.list" in ids)
else: check("non-systemd host hides services.*", "services.list" not in ids)
check("overview always present", "system.overview" in ids)

print("-- read actions --")
st, r = call("POST", f"/api/machines/{mid}/actions/system.overview")
d = r["data"]
check("overview exit 0", st == 200 and r["run"]["exitCode"] == 0)
check("overview parsed", d["uptimeSeconds"] > 0 and d["cpus"] > 0 and d["memTotal"] > 0 and len(d["load"]) == 3 and len(d["disks"]) >= 1)
print(f"       up {d['uptimeSeconds']}s load {d['load']} mem {d['memUsed']>>20}/{d['memTotal']>>20} MiB disks {[x['mount'] for x in d['disks']]}")

if "services.list" in ids:
    st, r = call("POST", f"/api/machines/{mid}/actions/services.list")
    u = r["data"]["units"]
    check("services.list parsed", st == 200 and len(u) > 0 and all(k in u[0] for k in ("name", "active", "sub")))
    unit = next((x["name"] for x in u if x["name"].startswith("ssh")), u[0]["name"])
    st, r = call("POST", f"/api/machines/{mid}/actions/services.status", {"params": {"unit": unit}, "confirm": False, "confirmName": ""})
    check("services.status quoted param", st == 200 and f"'{unit}'" in r["run"]["command"])
    st, r = call("POST", f"/api/machines/{mid}/actions/services.journal", {"params": {"unit": unit}, "confirm": False, "confirmName": ""})
    check("services.journal (preferred sudo) runs", st == 200, f"{st} {r.get('error','') if isinstance(r, dict) else r}")

st, r = call("POST", f"/api/machines/{mid}/actions/processes.top")
check("processes.top parsed", st == 200 and len(r["data"]["processes"]) > 0 and r["data"]["processes"][0]["pid"] > 0)

print(f"-- sudo semantics on a sudo={sudo} host --")
can_root = sudo in ("root", "nopasswd")
st, r = call("POST", f"/api/machines/{mid}/actions/network.ports")
check("network.ports (preferred) runs", st == 200)
if st == 200:
    ls = r["data"]["listeners"]
    check("our sshd appears in listeners", any(x["port"] == SSH_PORT for x in ls))
    check("sudo prefix matches host", r["run"]["command"].startswith("sudo -n ") == (sudo == "nopasswd"))
st, r = call("POST", f"/api/machines/{mid}/actions/disk.largest", {"params": {"path": "/tmp"}, "confirm": False, "confirmName": ""})
check("disk.largest (preferred) runs", st == 200 and isinstance(r["data"]["dirs"], list))
st, r = call("POST", f"/api/machines/{mid}/actions/processes.terminate", {"params": {"pid": "999999"}, "confirm": True, "confirmName": ""})
check("processes.terminate (tier1, preferred) executes", st == 200 and r["run"]["exitCode"] not in (None, 0),
      f"exit={r['run']['exitCode'] if st == 200 else r}")
st, r = call("POST", f"/api/machines/{mid}/actions/services.restart", {"params": {"unit": "bosun-e2e-nonexistent"}, "confirm": True, "confirmName": ""})
if can_root: check("services.restart (required) executes with sudo", st == 200 and r["run"]["exitCode"] != 0)
else: check("services.restart (required) refused without sudo", st == 409 and r["code"] == "not_available")

print("-- v1 catalog: providers, new actions --")
st, specs = call("GET", "/api/providers")
names = {s["name"] for s in specs}
check("five providers registered", names == {"hetzner", "digitalocean", "aws", "azure", "gcp"}, str(sorted(names)))
check("aws spec declares key-pair fields", any(s["name"] == "aws" and {f["name"] for f in s["fields"]} >= {"accessKeyId", "secretAccessKey"} for s in specs))
check("azure spec declares service-principal fields", any(s["name"] == "azure" and {f["name"] for f in s["fields"]} >= {"subscriptionId", "tenantId", "clientId", "clientSecret"} for s in specs))
check("gcp spec declares a textarea JSON key field", any(s["name"] == "gcp" and any(f["name"] == "serviceAccountJson" and f["kind"] == "textarea" for f in s["fields"]) for s in specs))
if "docker" in f["capabilities"]:
    check("docker host exposes containers.list", "containers.list" in ids)
if "supervisorctl" not in f["capabilities"]:
    check("no supervisor -> programs.* hidden", "programs.list" not in ids)
check("openssl -> web.tls_cert offered" if "openssl" in f["capabilities"] else "no openssl -> tls hidden",
      ("web.tls_cert" in ids) == ("openssl" in f["capabilities"]))

st, r = call("POST", f"/api/machines/{mid}/actions/network.established")
check("network.established parsed", st == 200 and isinstance(r["data"]["listeners"], list))
st, r = call("POST", f"/api/machines/{mid}/actions/disk.inodes")
check("disk.inodes parsed", st == 200 and len(r["data"]["inodes"]) >= 1 and r["data"]["inodes"][0]["total"] > 0)
st, r = call("POST", f"/api/machines/{mid}/actions/network.reach", {"params": {"url": f"http://127.0.0.1:{API_PORT}/api/health"}, "confirm": False, "confirmName": ""})
check("network.reach hits bosun's own health from the host", st == 200 and "status=200" in r["data"]["text"])
st, r = call("POST", f"/api/machines/{mid}/actions/web.tls_cert", {"params": {"host": "127.0.0.1", "port": str(SSH_PORT)}, "confirm": False, "confirmName": ""})
check("tls check on a non-TLS port reports ok=false, not blanks", st == 200 and r["data"]["ok"] is False)
st, r = call("POST", f"/api/machines/{mid}/actions/network.reach", {"params": {"url": "ftp://nope"}, "confirm": False, "confirmName": ""})
check("bad URL scheme rejected by pattern -> 400", st == 400)

print("-- audit --")
st, runs = call("GET", f"/api/runs?machine={mid}")
check("every executed action audited", len(runs) >= 6, f"{len(runs)} rows")
check("audit rows carry command + exit", all(x["command"] and x["exitCode"] is not None for x in runs))
check("terminate audited even though it failed", any(x["actionId"] == "processes.terminate" for x in runs))

print("-- re-probe reuses pin --")
st, r = call("POST", f"/api/machines/{mid}/probe")
check("re-probe ok", st == 200 and r["reachState"] == "ok")

open(D + "/mid", "w").write(str(mid))
open(D + "/fails1", "w").write("\n".join(fails))
sys.exit(1 if fails else 0)
PY

# ---------------------------------------------------------------------------
# Phase 2: terminal websocket — echo round-trip, resize, clean close, audited.
# ---------------------------------------------------------------------------
echo "-- terminal --"
MID=$(cat "$D/mid")
cat > "$D/term.mjs" <<EOF
const ws = new WebSocket("ws://127.0.0.1:$API_PORT/api/machines/$MID/terminal?cols=100&rows=30");
ws.binaryType = "arraybuffer";
const enc = new TextEncoder(), dec = new TextDecoder(); let buf = "", echoed = false;
ws.onopen = () => setTimeout(() => {
  ws.send(JSON.stringify({ type: "resize", cols: 120, rows: 40 }));
  ws.send(enc.encode('echo BOSUN-TERM-\$((40+2)); stty size\n'));
}, 400);
ws.onmessage = (e) => {
  buf += typeof e.data === "string" ? e.data : dec.decode(e.data);
  if (!echoed && buf.includes("BOSUN-TERM-42")) { echoed = true; setTimeout(() => ws.send(enc.encode("exit\n")), 300); }
};
ws.onclose = (e) => {
  const m = buf.match(/\n(\d+) (\d+)\r?\n/);
  const resized = !!m && m[1] === "40" && m[2] === "120";
  console.log((echoed ? "  ok   " : "  FAIL ") + "echo round-trip");
  console.log((resized ? "  ok   " : "  FAIL ") + "resize honored (stty size " + (m ? m[1] + "x" + m[2] : "?") + ")");
  console.log((e.code === 1000 ? "  ok   " : "  FAIL ") + "clean close " + e.code + " " + JSON.stringify(e.reason));
  process.exit(echoed && resized && e.code === 1000 ? 0 : 1);
};
setTimeout(() => { console.log("  FAIL terminal timeout; tail:", JSON.stringify(buf.slice(-200))); process.exit(1); }, 15000);
EOF
node "$D/term.mjs"
curl -s "$B/api/runs?machine=$MID" | python3 -c '
import json,sys; ok = any(r["actionId"]=="terminal.open" for r in json.load(sys.stdin))
print(("  ok   " if ok else "  FAIL ") + "terminal open audited"); sys.exit(0 if ok else 1)'

echo "-- streaming (journal follow over websocket) --"
UNIT=$(curl -s -X POST "$B/api/machines/$MID/actions/services.list" | python3 -c '
import json,sys
u=json.load(sys.stdin)["data"]["units"]
# a unit likely to have journal output; fall back to the first one
pick=next((x["name"] for x in u if x["name"] in ("systemd-journald.service","systemd-logind.service")), u[0]["name"] if u else "systemd-journald.service")
print(pick)')
cat > "$D/stream.mjs" <<EOF
const ws = new WebSocket("ws://127.0.0.1:$API_PORT/api/machines/$MID/actions/services.journal_follow/stream?unit=" + encodeURIComponent("$UNIT"));
let opened = false, frames = 0;
ws.onopen = () => { opened = true; };
ws.onmessage = (e) => { if (typeof e.data === "string") { frames++; if (frames >= 1) setTimeout(() => ws.close(), 400); } };
ws.onclose = () => {
  console.log((opened ? "  ok   " : "  FAIL ") + "journal-follow websocket connected");
  console.log((frames > 0 ? "  ok   " : "  note ") + "journal-follow delivered " + frames + " line(s)");
  process.exit(opened ? 0 : 1);
};
ws.onerror = () => {};
setTimeout(() => { if (!opened) { console.log("  FAIL journal-follow did not connect"); process.exit(1); } else { console.log("  ok   journal-follow connected (" + frames + " line(s))"); process.exit(0); } }, 5000);
// Hard backstop: never hang the suite even if onclose is missed.
setTimeout(() => { console.log("  FAIL journal-follow hard timeout"); process.exit(1); }, 9000);
EOF
timeout 20 node "$D/stream.mjs"
curl -s "$B/api/runs?machine=$MID" | python3 -c '
import json,sys; ok = any(r["actionId"]=="services.journal_follow" for r in json.load(sys.stdin))
print(("  ok   " if ok else "  FAIL ") + "stream open audited"); sys.exit(0 if ok else 1)'

# ---------------------------------------------------------------------------
# Phase 3: host key change — the machine is "rebuilt" (new host key). Bosun
# must refuse, record what it saw, reject a stale trust, accept the real one.
# ---------------------------------------------------------------------------
echo "-- host key change (machine rebuilt: all sessions gone, new host key) --"
stop_sshd
rm -f "$D/sshd/hostkey" "$D/sshd/hostkey.pub"
ssh-keygen -q -t ed25519 -N '' -f "$D/sshd/hostkey"
start_sshd
python3 - <<'PY'
import base64, hashlib, json, os, sys, urllib.request, urllib.error
B, D = os.environ["B"], os.environ["D"]
mid = int(open(D + "/mid").read()); fails = []
def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else (b"" if method == "POST" else None)
    req = urllib.request.Request(B + path, data=data, method=method, headers={"Content-Type": "application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=60); t = r.read(); return r.status, (json.loads(t) if t else None)
    except urllib.error.HTTPError as e:
        t = e.read()
        try: return e.code, json.loads(t)
        except Exception: return e.code, t.decode()
def check(name, cond, detail=""):
    print(("  ok   " if cond else "  FAIL ") + name + (f"  ({detail})" if detail else ""))
    if not cond: fails.append(name)
blob = base64.b64decode(open(D + "/sshd/hostkey.pub").read().split()[1])
newfp = "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")

st, r = call("POST", f"/api/machines/{mid}/probe")
check("probe refused with hostkey_changed", st == 502 and r["code"] == "hostkey_changed" and r["machine"]["reachState"] == "hostkey_changed", f"{st} {r.get('code')}")
st, r = call("POST", f"/api/machines/{mid}/actions/system.overview")
check("action refused with hostkey_changed", st == 409 and r["code"] == "hostkey_changed")
check("refusal carries the observed fingerprint", r.get("details", {}).get("fingerprint") == newfp)
st, hk = call("GET", f"/api/machines/{mid}/hostkey")
check("seen key recorded server-side", hk["seenFingerprint"] == newfp and hk["fingerprint"] != newfp)
st, r = call("POST", f"/api/machines/{mid}/hostkey/trust", {"algorithm": "ssh-ed25519", "fingerprint": "SHA256:not-the-key"})
check("stale/forged trust rejected", st == 409 and r["code"] == "stale")
st, r = call("POST", f"/api/machines/{mid}/hostkey/trust", {"algorithm": hk["seenAlgorithm"], "fingerprint": hk["seenFingerprint"]})
check("trusting the seen key accepted", st == 204)
st, r = call("POST", f"/api/machines/{mid}/probe")
check("probe ok after trust", st == 200 and r["reachState"] == "ok")
st, hk = call("GET", f"/api/machines/{mid}/hostkey")
check("pin replaced and seen cleared", hk["fingerprint"] == newfp and hk["seenFingerprint"] == "")
st, r = call("POST", f"/api/machines/{mid}/hostkey/trust", {"algorithm": hk["algorithm"], "fingerprint": hk["fingerprint"]})
check("trust with nothing pending rejected", st == 409)
open(D + "/fails3", "w").write("\n".join(fails))
sys.exit(1 if fails else 0)
PY

echo "-- server log: warnings/errors --"
grep -E 'level=(WARN|ERROR)' "$D/bosun.log" || echo "  (none)"
echo "== e2e passed =="
