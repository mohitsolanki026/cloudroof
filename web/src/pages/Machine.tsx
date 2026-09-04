import { useCallback, useEffect, useMemo, useState } from 'react'
import { api, hasCloud, hasHost, renderCommand, canRun, ApiError, type Action, type Machine, type PowerAction, type HostKey } from '../api'
import { StatusDots, PowerChip, ReachChip, ProviderChip } from '../components/Status'
import { Confirm } from '../components/Confirm'
import { Terminal } from '../components/Terminal'
import { RunList } from './Activity'
import { bytes, duration, ago } from '../lib/format'
import { useToast } from '../App'

// Tabs render from the actions the host actually supports. A box without
// systemctl never shows a Services tab; that is the catalog doing its job.

const TAB_ORDER = ['overview', 'services', 'processes', 'network', 'disk', 'terminal', 'activity'] as const
const TAB_LABEL: Record<string, string> = {
  overview: 'Overview',
  services: 'Services',
  processes: 'Processes',
  network: 'Network',
  disk: 'Disk',
  terminal: 'Terminal',
  activity: 'Activity',
}

type Pending = {
  kind: 'action' | 'power'
  id: string
  label: string
  command: string
  danger: 1 | 2
  params: Record<string, string>
  resolve: (ok: boolean) => void
}

export function MachinePage({ id, tab }: { id: number; tab: string }) {
  const [m, setM] = useState<Machine | null>(null)
  const [actions, setActions] = useState<Action[]>([])
  const [pending, setPending] = useState<Pending | null>(null)
  const [busy, setBusy] = useState(false)
  const [probing, setProbing] = useState(false)
  const toast = useToast()

  const load = useCallback(async () => {
    const [mm, aa] = await Promise.all([api.machines.get(id), api.machines.actions(id)])
    setM(mm)
    setActions(aa)
  }, [id])

  useEffect(() => {
    load().catch((e) => toast(e.message, true))
  }, [load, toast])

  const probe = async () => {
    setProbing(true)
    try {
      await api.machines.probe(id)
      toast('Reachable · facts refreshed')
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setProbing(false)
      load()
    }
  }

  // gate() asks the user through the Confirm dialog for Tier 1/2 and resolves
  // true when they accept. Tier 0 resolves immediately.
  const gate = (p: Omit<Pending, 'resolve'>): Promise<boolean> =>
    new Promise((resolve) => setPending({ ...p, resolve }))

  // runAction is the one path every button goes through.
  const runAction = useCallback(
    async <T,>(a: Action, params: Record<string, string> = {}): Promise<T | null> => {
      if (!m) return null
      let confirm = false
      let confirmName = ''
      if (a.danger === 1 || a.danger === 2) {
        const ok = await gate({
          kind: 'action',
          id: a.id,
          label: a.label,
          command: renderCommand(a, params, m.facts),
          danger: a.danger,
          params,
        })
        setPending(null)
        if (!ok) return null
        confirm = true
        confirmName = m.name
      }
      setBusy(true)
      try {
        const res = await api.machines.run<T>(id, a.id, params, confirm, confirmName)
        if (a.danger > 0) toast(`${a.label}: ${res.run.exitCode === 0 ? 'ok' : 'exit ' + res.run.exitCode}`)
        return res.data
      } catch (e: any) {
        if (e instanceof ApiError && (e.code === 'unreachable' || e.code === 'auth_failed' || e.code === 'hostkey_changed')) load()
        toast(`${a.label}: ${e.message}`, true)
        return null
      } finally {
        setBusy(false)
      }
    },
    [m, id, toast, load],
  )

  const power = async (action: PowerAction) => {
    if (!m) return
    const danger: 1 | 2 = action === 'start' ? 1 : 2
    const ok = await gate({
      kind: 'power',
      id: action,
      label: `Power: ${action.replace('_', ' ')}`,
      command: `${m.provider} ${action} ${m.instanceId}`,
      danger,
      params: {},
    })
    setPending(null)
    if (!ok) return
    setBusy(true)
    try {
      await api.machines.power(id, action, true, m.name)
      toast(`${action.replace('_', ' ')} requested`)
      load()
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setBusy(false)
    }
  }

  const categories = useMemo(() => new Set(actions.map((a) => a.category)), [actions])
  const tabs = TAB_ORDER.filter((t) => {
    if (t === 'overview' || t === 'activity') return true
    if (t === 'terminal') return m ? hasHost(m) : false
    return categories.has(t)
  })

  if (!m) return <div className="empty">Loading…</div>

  const byId = (aid: string) => actions.find((a) => a.id === aid)
  const setTab = (t: string) => (location.hash = `#/machines/${id}?tab=${t}`)

  return (
    <>
      <div className="machine-head">
        <div className="machine-title">
          <StatusDots m={m} />
          <h1>{m.name}</h1>
          <ProviderChip provider={m.provider} />
          <PowerChip m={m} />
          <ReachChip m={m} />
          {m.missing && <span className="chip warn">missing from provider</span>}
        </div>
        <div className="machine-meta">
          {m.publicIp && (
            <span>
              public <span className="mono">{m.publicIp}</span>
            </span>
          )}
          {m.privateIp && (
            <span>
              private <span className="mono">{m.privateIp}</span>
            </span>
          )}
          {hasHost(m) && (
            <span>
              ssh{' '}
              <span className="mono">
                {m.sshUser}@{m.sshHost}:{m.sshPort}
              </span>
            </span>
          )}
          {m.instanceType && (
            <span>
              {m.instanceType} · {m.region}
            </span>
          )}
          {m.facts && (
            <span>
              {m.facts.osName} {m.facts.osVersion} · {m.facts.kernel} · {m.facts.arch} · {m.facts.initSystem} · sudo {m.facts.sudoMode}
            </span>
          )}
          <span className="muted">checked {ago(m.reachCheckedAt)}</span>
        </div>
        <div className="toolbar">
          {hasHost(m) && (
            <button className="btn" onClick={probe} disabled={probing}>
              {probing ? 'Probing…' : 'Probe & refresh facts'}
            </button>
          )}
          {hasCloud(m) && (
            <>
              <span className="muted" style={{ marginLeft: 8, fontSize: 12 }}>
                Power via {m.provider}:
              </span>
              {m.powerState !== 'running' && (
                <button className="btn primary" disabled={busy} onClick={() => power('start')}>
                  Start
                </button>
              )}
              {m.powerState === 'running' && (
                <>
                  <button className="btn" disabled={busy} onClick={() => power('reboot')}>
                    Reboot
                  </button>
                  <button className="btn warn" disabled={busy} onClick={() => power('stop')}>
                    Shut down
                  </button>
                  <button className="btn danger" disabled={busy} onClick={() => power('force_reboot')} title="Hard reset — for a hung machine">
                    Force reboot
                  </button>
                  <button className="btn danger" disabled={busy} onClick={() => power('force_stop')} title="Pull the plug">
                    Force stop
                  </button>
                </>
              )}
            </>
          )}
          {m.reachState === 'hostkey_changed' && <HostKeyWarning m={m} onTrusted={load} />}
        </div>
        {!hasHost(m) && (
          <div className="warn-box">
            This machine has no SSH user or credential yet. Link it in <a href="#/settings">Settings</a> to unlock the action tabs and terminal.
          </div>
        )}
        {m.facts && m.facts.sudoMode !== 'root' && m.facts.sudoMode !== 'nopasswd' && (
          <div className="warn-box">
            {m.facts.sudoMode === 'password' ? 'sudo on this host asks for a password' : 'this host has no usable sudo'}, so actions that need root (service
            start/stop, reboot, journal vacuum) are disabled and the rest run as <span className="mono">{m.sshUser}</span>. To unlock everything, add{' '}
            <span className="mono">{m.sshUser} ALL=(ALL) NOPASSWD:ALL</span> to sudoers or connect as root.
          </div>
        )}
      </div>

      <div className="tabs">
        {tabs.map((t) => (
          <button key={t} className={'tab' + (tab === t ? ' active' : '')} onClick={() => setTab(t)}>
            {TAB_LABEL[t]}
          </button>
        ))}
      </div>

      {tab === 'overview' && <Overview m={m} run={runAction} action={byId('system.overview')} />}
      {tab === 'services' && <Services m={m} run={runAction} actions={actions} />}
      {tab === 'processes' && <Processes run={runAction} actions={actions} />}
      {tab === 'network' && <Network run={runAction} action={byId('network.ports')} />}
      {tab === 'disk' && <Disk run={runAction} actions={actions} />}
      {tab === 'terminal' && hasHost(m) && <Terminal machineId={id} />}
      {tab === 'activity' && (
        <div className="card">
          <RunList machineId={id} />
        </div>
      )}

      {pending && (
        <Confirm
          title={pending.label}
          command={pending.command}
          danger={pending.danger}
          machineName={m.name}
          busy={busy}
          onConfirm={() => pending.resolve(true)}
          onCancel={() => {
            pending.resolve(false)
            setPending(null)
          }}
        />
      )}
    </>
  )
}

type Runner = <T>(a: Action, params?: Record<string, string>) => Promise<T | null>

// --- overview -------------------------------------------------------------------

interface OverviewData {
  uptimeSeconds: number
  load: number[]
  cpus: number
  memTotal: number
  memUsed: number
  swapTotal: number
  swapUsed: number
  disks: { source: string; mount: string; total: number; used: number; avail: number; usedPct: number }[]
  users: number
  rebootRequired: boolean
}

function Overview({ m, run, action }: { m: Machine; run: Runner; action?: Action }) {
  const [d, setD] = useState<OverviewData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [at, setAt] = useState<Date | null>(null)

  const refresh = useCallback(async () => {
    if (!action || !hasHost(m)) return
    setErr(null)
    const data = await run<OverviewData>(action)
    if (data) {
      setD(data)
      setAt(new Date())
    } else setErr('Could not read the overview — see the toast for the reason.')
  }, [action, m, run])

  useEffect(() => {
    refresh()
  }, [refresh])

  if (!hasHost(m)) return <div className="empty">Link this machine by SSH to see live state.</div>
  if (!d && !err) return <div className="empty">Reading host…</div>
  if (!d) return <div className="error-box">{err}</div>

  const memPct = d.memTotal ? Math.round((d.memUsed / d.memTotal) * 100) : 0
  const load1 = d.load[0] ?? 0
  const loadPct = d.cpus ? Math.min(100, Math.round((load1 / d.cpus) * 100)) : 0
  const barClass = (p: number) => (p >= 90 ? 'crit' : p >= 75 ? 'warn' : '')

  return (
    <>
      <div className="row" style={{ marginBottom: 12, justifyContent: 'space-between' }}>
        <span className="hint">{at ? `as of ${at.toLocaleTimeString()}` : ''}</span>
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      <div className="stats">
        <div className="stat">
          <div className="k">Uptime</div>
          <div className="v">{duration(d.uptimeSeconds)}</div>
          {d.rebootRequired && <div className="s" style={{ color: 'var(--warn)' }}>reboot required</div>}
        </div>
        <div className="stat">
          <div className="k">Load · {d.cpus} cpu</div>
          <div className="v">
            {d.load.map((x) => x.toFixed(2)).join('  ')}
          </div>
          <div className={'bar ' + barClass(loadPct)}>
            <i style={{ width: loadPct + '%' }} />
          </div>
        </div>
        <div className="stat">
          <div className="k">Memory</div>
          <div className="v">
            {bytes(d.memUsed)}
            <small>/ {bytes(d.memTotal)}</small>
          </div>
          <div className={'bar ' + barClass(memPct)}>
            <i style={{ width: memPct + '%' }} />
          </div>
        </div>
        {d.swapTotal > 0 && (
          <div className="stat">
            <div className="k">Swap</div>
            <div className="v">
              {bytes(d.swapUsed)}
              <small>/ {bytes(d.swapTotal)}</small>
            </div>
            <div className="bar">
              <i style={{ width: Math.round((d.swapUsed / d.swapTotal) * 100) + '%' }} />
            </div>
          </div>
        )}
        <div className="stat">
          <div className="k">Sessions</div>
          <div className="v">{d.users}</div>
          <div className="s">logged-in users</div>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>Filesystems</h3>
        </div>
        <div className="card-body" style={{ paddingTop: 4, paddingBottom: 4 }}>
          {d.disks.length === 0 && <div className="hint">No real filesystems reported.</div>}
          {d.disks.map((x) => (
            <div className="disk-row" key={x.mount}>
              <div>
                <div className="mount">{x.mount}</div>
                <div className="src">
                  {x.source} · {bytes(x.avail)} free of {bytes(x.total)}
                </div>
              </div>
              <div className={'bar ' + barClass(x.usedPct)}>
                <i style={{ width: x.usedPct + '%' }} />
              </div>
              <div className="pct">{x.usedPct}%</div>
            </div>
          ))}
        </div>
      </div>
    </>
  )
}

// --- services -------------------------------------------------------------------

interface Unit {
  name: string
  load: string
  active: string
  sub: string
  description: string
}

function Services({ m, run, actions }: { m: Machine; run: Runner; actions: Action[] }) {
  const [units, setUnits] = useState<Unit[] | null>(null)
  const [filter, setFilter] = useState('')
  const [journal, setJournal] = useState<{ unit: string; lines: string[] } | null>(null)
  const list = actions.find((a) => a.id === 'services.list')
  const byId = (id: string) => actions.find((a) => a.id === id)

  const refresh = useCallback(async () => {
    if (!list) return
    const d = await run<{ units: Unit[] }>(list)
    if (d) setUnits(d.units)
  }, [list, run])

  useEffect(() => {
    refresh()
  }, [refresh])

  const act = async (id: string, unit: string) => {
    const a = byId(id)
    if (!a) return
    const r = await run<unknown>(a, { unit })
    if (r !== null) refresh()
  }

  const showJournal = async (unit: string) => {
    const a = byId('services.journal')
    if (!a) return
    const d = await run<{ lines: string[] }>(a, { unit })
    if (d) setJournal({ unit, lines: d.lines })
  }

  const q = filter.toLowerCase()
  const shown = (units ?? []).filter((u) => !q || u.name.toLowerCase().includes(q) || u.description.toLowerCase().includes(q))
  const subClass = (u: Unit) => (u.active === 'active' ? 'ok' : u.active === 'failed' ? 'crit' : '')
  const restart = byId('services.restart')
  const canWrite = restart ? canRun(restart, m.facts) : false

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <input className="input" placeholder="Filter units…" value={filter} onChange={(e) => setFilter(e.target.value)} style={{ width: 240 }} />
        <span className="hint">{units ? `${shown.length} of ${units.length}` : ''}</span>
        <span className="grow" />
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      {journal && (
        <div className="card" style={{ marginBottom: 12 }}>
          <div className="card-head">
            <h3>
              journal · <span className="mono">{journal.unit}</span>
            </h3>
            <button className="btn sm ghost" onClick={() => setJournal(null)}>
              Close
            </button>
          </div>
          <pre className="output" style={{ border: 0, borderRadius: 0 }}>
            {journal.lines.join('\n') || '(empty)'}
          </pre>
        </div>
      )}
      <div className="card">
        {units === null ? (
          <div className="empty">Listing units…</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Unit</th>
                  <th>State</th>
                  <th>Description</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shown.map((u) => (
                  <tr key={u.name}>
                    <td className="mono">{u.name}</td>
                    <td>
                      <span className={'chip ' + subClass(u)}>
                        {u.active} · {u.sub}
                      </span>
                    </td>
                    <td className="muted" style={{ maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {u.description}
                    </td>
                    <td className="actions">
                      <button className="btn sm ghost" onClick={() => showJournal(u.name)}>
                        Journal
                      </button>
                      {canWrite && (
                        <>
                          {u.active === 'active' ? (
                            <>
                              <button className="btn sm" onClick={() => act('services.restart', u.name)}>
                                Restart
                              </button>
                              <button className="btn sm warn" onClick={() => act('services.stop', u.name)}>
                                Stop
                              </button>
                            </>
                          ) : (
                            <button className="btn sm primary" onClick={() => act('services.start', u.name)}>
                              Start
                            </button>
                          )}
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

// --- processes ------------------------------------------------------------------

interface Proc {
  pid: number
  user: string
  cpu: number
  mem: number
  rss: number
  elapsed: number
  command: string
}

function Processes({ run, actions }: { run: Runner; actions: Action[] }) {
  const [procs, setProcs] = useState<Proc[] | null>(null)
  const top = actions.find((a) => a.id === 'processes.top')
  const term = actions.find((a) => a.id === 'processes.terminate')
  const kill = actions.find((a) => a.id === 'processes.kill')

  const refresh = useCallback(async () => {
    if (!top) return
    const d = await run<{ processes: Proc[] }>(top)
    if (d) setProcs(d.processes)
  }, [top, run])

  useEffect(() => {
    refresh()
  }, [refresh])

  const signal = async (a: Action | undefined, pid: number) => {
    if (!a) return
    const r = await run<unknown>(a, { pid: String(pid) })
    if (r !== null) setTimeout(refresh, 500)
  }

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <span className="hint">Top 60 by CPU.</span>
        <span className="grow" />
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      <div className="card">
        {procs === null ? (
          <div className="empty">Reading…</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th className="num">PID</th>
                  <th>User</th>
                  <th className="num">CPU %</th>
                  <th className="num">Mem %</th>
                  <th className="num">RSS</th>
                  <th className="num">Age</th>
                  <th>Command</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {procs.map((p) => (
                  <tr key={p.pid}>
                    <td className="num">{p.pid}</td>
                    <td className="mono">{p.user}</td>
                    <td className="num">{p.cpu.toFixed(1)}</td>
                    <td className="num">{p.mem.toFixed(1)}</td>
                    <td className="num">{bytes(p.rss)}</td>
                    <td className="num muted">{duration(p.elapsed)}</td>
                    <td className="mono">{p.command}</td>
                    <td className="actions">
                      {term && (
                        <button className="btn sm warn" onClick={() => signal(term, p.pid)}>
                          TERM
                        </button>
                      )}
                      {kill && (
                        <button className="btn sm danger" onClick={() => signal(kill, p.pid)}>
                          KILL
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

// --- network --------------------------------------------------------------------

interface Listener {
  proto: string
  local: string
  port: number
  process: string
  pid: number
}

function Network({ run, action }: { run: Runner; action?: Action }) {
  const [ls, setLs] = useState<Listener[] | null>(null)
  const refresh = useCallback(async () => {
    if (!action) return
    const d = await run<{ listeners: Listener[] }>(action)
    if (d) setLs(d.listeners)
  }, [action, run])
  useEffect(() => {
    refresh()
  }, [refresh])

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <span className="hint">Listening sockets, mapped to the owning process.</span>
        <span className="grow" />
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      <div className="card">
        {ls === null ? (
          <div className="empty">Reading…</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Proto</th>
                  <th className="num">Port</th>
                  <th>Bind</th>
                  <th>Process</th>
                  <th className="num">PID</th>
                </tr>
              </thead>
              <tbody>
                {[...ls]
                  .sort((a, b) => a.port - b.port)
                  .map((l, i) => (
                    <tr key={i}>
                      <td className="mono">{l.proto}</td>
                      <td className="num">{l.port}</td>
                      <td className="mono muted">{l.local}</td>
                      <td className="mono">{l.process || <span className="muted">—</span>}</td>
                      <td className="num muted">{l.pid || ''}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

// --- disk -----------------------------------------------------------------------

function Disk({ run, actions }: { run: Runner; actions: Action[] }) {
  const [path, setPath] = useState('/')
  const [dirs, setDirs] = useState<{ path: string; size: number }[] | null>(null)
  const [busy, setBusy] = useState(false)
  const largest = actions.find((a) => a.id === 'disk.largest')
  const vacuum = actions.find((a) => a.id === 'disk.vacuum_journal')

  const scan = async () => {
    if (!largest) return
    setBusy(true)
    const d = await run<{ dirs: { path: string; size: number }[] }>(largest, { path })
    setBusy(false)
    if (d) setDirs(d.dirs)
  }

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} style={{ width: 260 }} />
        <button className="btn" onClick={scan} disabled={busy || !largest}>
          {busy ? 'Scanning…' : 'Find largest directories'}
        </button>
        <span className="grow" />
        {vacuum && (
          <button className="btn" onClick={() => run(vacuum)}>
            Vacuum journal to 7d
          </button>
        )}
      </div>
      <div className="card">
        {dirs === null ? (
          <div className="empty">Pick a path and scan. Two levels deep, same filesystem only.</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th className="num">Size</th>
                  <th>Path</th>
                </tr>
              </thead>
              <tbody>
                {dirs.map((d) => (
                  <tr key={d.path}>
                    <td className="num">{bytes(d.size)}</td>
                    <td className="mono">{d.path}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

// --- host key ---------------------------------------------------------------------

// The server records the key it saw when the pin failed. The user is shown
// pinned vs. seen and must accept that specific fingerprint; the server
// refuses anything else.
function HostKeyWarning({ m, onTrusted }: { m: Machine; onTrusted: () => void }) {
  const toast = useToast()
  const [k, setK] = useState<HostKey | null>(null)

  useEffect(() => {
    api.machines
      .hostKey(m.id)
      .then(setK)
      .catch(() => setK(null))
  }, [m.id, m.reachCheckedAt])

  const trust = async () => {
    if (!k?.seenFingerprint) return
    const msg =
      `The SSH host key for ${m.name} does not match the one Bosun pinned.\n\n` +
      `Pinned:  ${k.algorithm} ${k.fingerprint}\n` +
      `Seen:    ${k.seenAlgorithm} ${k.seenFingerprint}\n\n` +
      `Trust the new key only if you rebuilt this machine or rotated its keys yourself. ` +
      `Otherwise this could be a different machine answering on the same address.`
    if (!confirm(msg)) return
    try {
      await api.machines.trustHostKey(m.id, k.seenAlgorithm, k.seenFingerprint)
      await api.machines.probe(m.id).catch(() => {})
      onTrusted()
      toast('New host key trusted')
    } catch (e: any) {
      toast(e.message, true)
    }
  }

  return (
    <button className="btn warn" onClick={trust} disabled={!k?.seenFingerprint} title={k?.seenFingerprint ? `seen ${k.seenFingerprint}` : 'loading…'}>
      Host key changed — review
    </button>
  )
}
