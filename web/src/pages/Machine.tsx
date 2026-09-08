import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, hasCloud, hasHost, renderCommand, canRun, ApiError, type Action, type Machine, type PowerAction, type HostKey, type Credential } from '../api'
import { StatusDots, PowerChip, ReachChip, ProviderChip } from '../components/Status'
import { Confirm } from '../components/Confirm'
import { Terminal } from '../components/Terminal'
import { RunList } from './Activity'
import { bytes, duration, ago } from '../lib/format'
import { useToast } from '../App'

// Tabs render from the actions the host actually supports. A box without
// systemctl never shows a Services tab; that is the catalog doing its job.

const TAB_ORDER = ['overview', 'services', 'programs', 'processes', 'containers', 'network', 'web', 'disk', 'terminal', 'activity'] as const
const TAB_LABEL: Record<string, string> = {
  overview: 'Overview',
  services: 'Services',
  programs: 'Programs',
  processes: 'Processes',
  containers: 'Containers',
  network: 'Network',
  web: 'Web',
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
  const [editing, setEditing] = useState(false)
  const toast = useToast()

  // runAction reads the machine through a ref so its identity does not change
  // every time load() refreshes state. Tab effects depend on runAction; if it
  // changed on every load, an error path that calls load() would re-run the
  // tab, fail again, load again — an unbounded loop.
  const mRef = useRef<Machine | null>(null)
  useEffect(() => {
    mRef.current = m
  }, [m])

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
  const gate = useCallback(
    (p: Omit<Pending, 'resolve'>): Promise<boolean> => new Promise((resolve) => setPending({ ...p, resolve })),
    [],
  )

  // runAction is the one path every button goes through.
  const runAction = useCallback(
    async <T,>(a: Action, params: Record<string, string> = {}): Promise<T | null> => {
      const cur = mRef.current
      if (!cur) return null
      let confirm = false
      let confirmName = ''
      const gated = a.danger === 1 || a.danger === 2
      if (gated) {
        const ok = await gate({
          kind: 'action',
          id: a.id,
          label: a.label,
          command: renderCommand(a, params, cur.facts),
          danger: a.danger as 1 | 2,
          params,
        })
        if (!ok) {
          setPending(null)
          return null
        }
        confirm = true
        confirmName = cur.name
      }
      // The dialog stays mounted while the request runs so its busy state
      // ("Running…", Cancel disabled) is actually visible.
      setBusy(true)
      try {
        const res = await api.machines.run<T>(id, a.id, params, confirm, confirmName)
        const code = res.run.exitCode
        const stderr = (res.run.stderr || '').split('\n').find((l) => l.trim()) ?? ''
        if (code !== 0 && (gated || stderr || looksEmpty(res.data))) {
          // A read action that failed on the host would otherwise render as a
          // plausible empty table. Say what the host said.
          toast(`${a.label}: exit ${code}${stderr ? ' — ' + stderr : ''}`, true)
        } else if (gated) {
          toast(`${a.label}: ok`)
        }
        return res.data
      } catch (e: any) {
        if (e instanceof ApiError && (e.code === 'unreachable' || e.code === 'auth_failed' || e.code === 'hostkey_changed')) load()
        toast(`${a.label}: ${e.message}`, true)
        return null
      } finally {
        setBusy(false)
        if (gated) setPending(null)
      }
    },
    [id, toast, load, gate],
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
    if (!ok) {
      setPending(null)
      return
    }
    setBusy(true)
    try {
      await api.machines.power(id, action, true, m.name)
      toast(`${action.replace('_', ' ')} requested`)
      load()
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setBusy(false)
      setPending(null)
    }
  }

  const categories = useMemo(() => new Set(actions.map((a) => a.category)), [actions])
  const tabs = TAB_ORDER.filter((t) => {
    if (t === 'overview' || t === 'activity') return true
    if (t === 'terminal') return m ? hasHost(m) : false
    // Every other tab runs actions the engine refuses until the host has
    // been fingerprinted; offering them earlier just produces a 409.
    if (!m?.facts) return false
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
          <button className="btn" onClick={() => setEditing(true)}>
            Edit connection
          </button>
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
        {hasHost(m) && hasCloud(m) && m.publicIp && m.publicIp !== m.sshHost && (
          <div className="warn-box">
            SSH is set to <span className="mono">{m.sshHost}</span> but the provider now reports{' '}
            <span className="mono">{m.publicIp}</span> — the instance's public IP likely changed on a stop/start.{' '}
            <button className="btn sm" onClick={() => setEditing(true)}>Update address</button>
            {' '}or attach an Elastic/static IP so it stops moving.
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
      {tab === 'programs' && <Programs m={m} id={id} run={runAction} actions={actions} />}
      {tab === 'processes' && <Processes run={runAction} actions={actions} />}
      {tab === 'containers' && <Containers id={id} run={runAction} actions={actions} />}
      {tab === 'network' && <Network run={runAction} actions={actions} />}
      {tab === 'web' && <Web m={m} run={runAction} actions={actions} />}
      {tab === 'disk' && <Disk m={m} run={runAction} actions={actions} />}
      {tab === 'terminal' && hasHost(m) && <Terminal machineId={id} />}
      {tab === 'activity' && (
        <div className="card">
          <RunList machineId={id} />
        </div>
      )}

      {editing && (
        <EditConnection
          m={m}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false)
            probe()
          }}
        />
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

// looksEmpty: a parsed payload whose only content is empty arrays — what a
// failed list command parses into.
function looksEmpty(d: unknown): boolean {
  if (!d || typeof d !== 'object') return false
  const arrays = Object.values(d as Record<string, unknown>).filter(Array.isArray)
  return arrays.length > 0 && arrays.every((a) => a.length === 0)
}

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
  const [follow, setFollow] = useState<string | null>(null)
  const followAction = actions.find((a) => a.id === 'services.journal_follow')
  const list = actions.find((a) => a.id === 'services.list')
  const byId = (id: string) => actions.find((a) => a.id === id)

  const [failed, setFailed] = useState(false)
  const refresh = useCallback(async () => {
    if (!list) return
    setFailed(false)
    const d = await run<{ units: Unit[] }>(list)
    if (d) setUnits(d.units)
    else setFailed(true)
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
      {follow && followAction ? (
        <div style={{ marginBottom: 12 }}>
          <LogStream machineId={m.id} action={followAction} params={{ unit: follow }} title={`journal · ${follow} (live)`} onClose={() => setFollow(null)} />
        </div>
      ) : (
        journal && (
          <div className="card" style={{ marginBottom: 12 }}>
            <div className="card-head">
              <h3>
                journal · <span className="mono">{journal.unit}</span>
              </h3>
              <div className="row" style={{ gap: 6 }}>
                {followAction && (
                  <button className="btn sm" onClick={() => setFollow(journal.unit)}>
                    Follow live
                  </button>
                )}
                <button className="btn sm ghost" onClick={() => setJournal(null)}>
                  Close
                </button>
              </div>
            </div>
            <pre className="output" style={{ border: 0, borderRadius: 0 }}>
              {journal.lines.join('\n') || '(empty)'}
            </pre>
          </div>
        )
      )}
      <div className="card">
        {units === null ? (
          <div className="empty">{failed ? 'Could not list units — see the toast for the reason.' : 'Listing units…'}</div>
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
  const [failed, setFailed] = useState(false)
  const top = actions.find((a) => a.id === 'processes.top')
  const term = actions.find((a) => a.id === 'processes.terminate')
  const kill = actions.find((a) => a.id === 'processes.kill')

  const refresh = useCallback(async () => {
    if (!top) return
    setFailed(false)
    const d = await run<{ processes: Proc[] }>(top)
    if (d) setProcs(d.processes)
    else setFailed(true)
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
          <div className="empty">{failed ? 'Could not read processes — see the toast for the reason.' : 'Reading…'}</div>
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
  peer: string
  port: number
  process: string
  pid: number
}

function Network({ run, actions }: { run: Runner; actions: Action[] }) {
  const [mode, setMode] = useState<'listening' | 'established'>('listening')
  const [ls, setLs] = useState<Listener[] | null>(null)
  const [failed, setFailed] = useState(false)
  const ports = actions.find((a) => a.id === 'network.ports')
  const established = actions.find((a) => a.id === 'network.established')
  const reach = actions.find((a) => a.id === 'network.reach')
  const action = mode === 'listening' ? ports : established

  const refresh = useCallback(async () => {
    if (!action) return
    setFailed(false)
    const d = await run<{ listeners: Listener[] }>(action)
    if (d) setLs(d.listeners)
    else setFailed(true)
  }, [action, run])
  useEffect(() => {
    setLs(null)
    refresh()
  }, [refresh])

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <div className="seg">
          <button className={'seg-btn' + (mode === 'listening' ? ' active' : '')} onClick={() => setMode('listening')}>
            Listening
          </button>
          {established && (
            <button className={'seg-btn' + (mode === 'established' ? ' active' : '')} onClick={() => setMode('established')}>
              Established
            </button>
          )}
        </div>
        <span className="hint">{mode === 'listening' ? 'Listening sockets, mapped to the owning process.' : 'Established connections.'}</span>
        <span className="grow" />
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      {reach && <ReachProbe run={run} action={reach} />}
      <div className="card">
        {ls === null ? (
          <div className="empty">{failed ? 'Could not read sockets — see the toast for the reason.' : 'Reading…'}</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Proto</th>
                  <th className="num">Port</th>
                  <th>{mode === 'listening' ? 'Bind' : 'Peer'}</th>
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
                      <td className="mono muted">{mode === 'established' ? l.peer || l.local : l.local}</td>
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

function ReachProbe({ run, action }: { run: Runner; action: Action }) {
  const [url, setUrl] = useState('https://')
  const [out, setOut] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const go = async () => {
    setBusy(true)
    const d = await run<{ text: string }>(action, { url })
    setBusy(false)
    if (d) setOut(d.text.trim())
  }
  return (
    <div className="card" style={{ marginBottom: 12 }}>
      <div className="card-body" style={{ display: 'grid', gap: 10 }}>
        <div className="row" style={{ gap: 8 }}>
          <input
            className="input mono grow"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && go()}
            placeholder="https://example.com/health"
            spellCheck={false}
          />
          <button className="btn" onClick={go} disabled={busy}>
            {busy ? 'Reaching…' : 'Reach from host'}
          </button>
        </div>
        {out && <pre className="output" style={{ maxHeight: 160 }}>{out}</pre>}
      </div>
    </div>
  )
}

// --- disk -----------------------------------------------------------------------

function Disk({ m, run, actions }: { m: Machine; run: Runner; actions: Action[] }) {
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
        {vacuum && canRun(vacuum, m.facts) && (
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

// --- live log stream ------------------------------------------------------------

// LogStream follows journalctl -f / docker logs -f / supervisorctl tail -f over
// a websocket. Lines arrive one frame each; scrollback is capped so a chatty
// service can't grow the DOM without bound. It auto-scrolls only while the
// viewer is already at the bottom, so scrolling up to read holds position.
function LogStream({
  machineId,
  action,
  params,
  title,
  onClose,
}: {
  machineId: number
  action: Action
  params: Record<string, string>
  title: string
  onClose: () => void
}) {
  const [lines, setLines] = useState<string[]>([])
  const [state, setState] = useState<'connecting' | 'live' | 'closed'>('connecting')
  const boxRef = useRef<HTMLPreElement>(null)
  const atBottom = useRef(true)
  const MAX = 2000

  useEffect(() => {
    let ws: WebSocket | null = null
    let cancelled = false
    const timer = setTimeout(() => {
      if (cancelled) return
      const sock = new WebSocket(api.machines.streamURL(machineId, action.id, params))
      ws = sock
      sock.onopen = () => setState('live')
      sock.onmessage = (ev) => {
        if (typeof ev.data !== 'string') return
        setLines((prev) => {
          const next = prev.concat(ev.data)
          return next.length > MAX ? next.slice(next.length - MAX) : next
        })
      }
      sock.onclose = () => setState('closed')
      sock.onerror = () => setState('closed')
    }, 0)
    return () => {
      cancelled = true
      clearTimeout(timer)
      ws?.close()
    }
  }, [machineId, action.id, JSON.stringify(params)])

  useEffect(() => {
    const el = boxRef.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [lines])

  const onScroll = () => {
    const el = boxRef.current
    if (!el) return
    atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>
          {title} <span className={'chip ' + (state === 'live' ? 'ok' : state === 'closed' ? 'crit' : '')}>{state}</span>
        </h3>
        <div className="row" style={{ gap: 6 }}>
          <button className="btn sm ghost" onClick={() => setLines([])}>
            Clear
          </button>
          <button className="btn sm" onClick={onClose}>
            Stop
          </button>
        </div>
      </div>
      <pre ref={boxRef} className="output" onScroll={onScroll} style={{ border: 0, borderRadius: 0, maxHeight: 480, minHeight: 240 }}>
        {lines.length ? lines.join('\n') : state === 'connecting' ? 'connecting…' : '(no output yet)'}
      </pre>
    </div>
  )
}

// --- programs (supervisor) ------------------------------------------------------

interface Program {
  name: string
  state: string
  info: string
}

function Programs({ m, id, run, actions }: { m: Machine; id: number; run: Runner; actions: Action[] }) {
  const [progs, setProgs] = useState<Program[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [tail, setTail] = useState<string | null>(null)
  const list = actions.find((a) => a.id === 'programs.list')
  const update = actions.find((a) => a.id === 'programs.update')
  const tailAction = actions.find((a) => a.id === 'programs.tail')
  const byId = (aid: string) => actions.find((a) => a.id === aid)

  const refresh = useCallback(async () => {
    if (!list) return
    setFailed(false)
    const d = await run<{ programs: Program[] }>(list)
    if (d) setProgs(d.programs)
    else setFailed(true)
  }, [list, run])
  useEffect(() => {
    refresh()
  }, [refresh])

  const act = async (aid: string, prog: string) => {
    const a = byId(aid)
    if (!a) return
    const r = await run<unknown>(a, { prog })
    if (r !== null) setTimeout(refresh, 400)
  }

  const stateClass = (st: string) => {
    if (st === 'RUNNING') return 'ok'
    if (st === 'STARTING' || st === 'STOPPING' || st === 'BACKOFF') return 'warn'
    if (st === 'FATAL' || st === 'EXITED') return 'crit'
    return ''
  }
  const canWrite = canRun(byId('programs.restart') ?? ({} as Action), m.facts)

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <span className="hint">supervisor programs</span>
        <span className="grow" />
        {update && (
          <button className="btn sm" onClick={() => act('programs.update', '')}>
            Reread &amp; update
          </button>
        )}
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      {tail && tailAction && (
        <div style={{ marginBottom: 12 }}>
          <LogStream machineId={id} action={tailAction} params={{ prog: tail }} title={`tail · ${tail}`} onClose={() => setTail(null)} />
        </div>
      )}
      <div className="card">
        {progs === null ? (
          <div className="empty">{failed ? 'Could not reach supervisor — see the toast.' : 'Reading…'}</div>
        ) : progs.length === 0 ? (
          <div className="empty">No programs configured.</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Program</th>
                  <th>State</th>
                  <th>Detail</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {progs.map((p) => (
                  <tr key={p.name}>
                    <td className="mono">{p.name}</td>
                    <td>
                      <span className={'chip ' + stateClass(p.state)}>{p.state}</span>
                    </td>
                    <td className="muted mono" style={{ fontSize: 12 }}>
                      {p.info}
                    </td>
                    <td className="actions">
                      {tailAction && (
                        <button className="btn sm ghost" onClick={() => setTail(p.name)}>
                          Tail
                        </button>
                      )}
                      {canWrite &&
                        (p.state === 'RUNNING' ? (
                          <>
                            <button className="btn sm" onClick={() => act('programs.restart', p.name)}>
                              Restart
                            </button>
                            <button className="btn sm warn" onClick={() => act('programs.stop', p.name)}>
                              Stop
                            </button>
                          </>
                        ) : (
                          <button className="btn sm primary" onClick={() => act('programs.start', p.name)}>
                            Start
                          </button>
                        ))}
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

// --- containers (docker) --------------------------------------------------------

interface ContainerRow {
  id: string
  name: string
  image: string
  state: string
  status: string
  ports: string
  runningFor: string
}

function Containers({ id, run, actions }: { id: number; run: Runner; actions: Action[] }) {
  const [rows, setRows] = useState<ContainerRow[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [logs, setLogs] = useState<{ id: string; name: string } | null>(null)
  const list = actions.find((a) => a.id === 'containers.list')
  const logsAction = actions.find((a) => a.id === 'containers.logs')
  const df = actions.find((a) => a.id === 'containers.df')
  const prune = actions.find((a) => a.id === 'containers.prune')
  const byId = (aid: string) => actions.find((a) => a.id === aid)
  const [dfOut, setDfOut] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    if (!list) return
    setFailed(false)
    const d = await run<{ containers: ContainerRow[] }>(list)
    if (d) setRows(d.containers)
    else setFailed(true)
  }, [list, run])
  useEffect(() => {
    refresh()
  }, [refresh])

  const act = async (aid: string, cid: string) => {
    const a = byId(aid)
    if (!a) return
    const r = await run<unknown>(a, { id: cid })
    if (r !== null) setTimeout(refresh, 500)
  }
  const showDf = async () => {
    if (!df) return
    const d = await run<{ text: string }>(df)
    if (d) setDfOut(d.text.trim())
  }

  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <span className="hint">docker containers</span>
        <span className="grow" />
        {df && (
          <button className="btn sm" onClick={showDf}>
            Disk usage
          </button>
        )}
        {prune && (
          <button className="btn sm warn" onClick={() => run(prune).then(() => setTimeout(refresh, 500))}>
            Prune unused
          </button>
        )}
        <button className="btn sm" onClick={refresh}>
          Refresh
        </button>
      </div>
      {dfOut && (
        <div className="card" style={{ marginBottom: 12 }}>
          <div className="card-head">
            <h3>docker system df</h3>
            <button className="btn sm ghost" onClick={() => setDfOut(null)}>
              Close
            </button>
          </div>
          <pre className="output" style={{ border: 0, borderRadius: 0 }}>
            {dfOut}
          </pre>
        </div>
      )}
      {logs && logsAction && (
        <div style={{ marginBottom: 12 }}>
          <LogStream machineId={id} action={logsAction} params={{ id: logs.id }} title={`logs · ${logs.name}`} onClose={() => setLogs(null)} />
        </div>
      )}
      <div className="card">
        {rows === null ? (
          <div className="empty">{failed ? 'Could not reach docker — see the toast.' : 'Reading…'}</div>
        ) : rows.length === 0 ? (
          <div className="empty">No containers.</div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Image</th>
                  <th>State</th>
                  <th>Status</th>
                  <th>Ports</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <tr key={c.id}>
                    <td className="mono">{c.name}</td>
                    <td className="mono muted" style={{ maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {c.image}
                    </td>
                    <td>
                      <span className={'chip ' + (c.state === 'running' ? 'ok' : c.state === 'paused' ? 'warn' : '')}>{c.state}</span>
                    </td>
                    <td className="muted" style={{ fontSize: 12 }}>
                      {c.status}
                    </td>
                    <td className="mono muted" style={{ fontSize: 11.5, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {c.ports}
                    </td>
                    <td className="actions">
                      {logsAction && (
                        <button className="btn sm ghost" onClick={() => setLogs({ id: c.id, name: c.name })}>
                          Logs
                        </button>
                      )}
                      {c.state === 'running' ? (
                        <>
                          <button className="btn sm" onClick={() => act('containers.restart', c.id)}>
                            Restart
                          </button>
                          <button className="btn sm warn" onClick={() => act('containers.stop', c.id)}>
                            Stop
                          </button>
                        </>
                      ) : (
                        <button className="btn sm primary" onClick={() => act('containers.start', c.id)}>
                          Start
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

// --- web (nginx / TLS) ----------------------------------------------------------

interface CertData {
  ok?: boolean
  subject?: string
  issuer?: string
  notBefore?: string
  notAfter?: string
}

function Web({ m, run, actions }: { m: Machine; run: Runner; actions: Action[] }) {
  const test = actions.find((a) => a.id === 'web.nginx_test')
  const reload = actions.find((a) => a.id === 'web.nginx_reload')
  const cert = actions.find((a) => a.id === 'web.tls_cert')
  const [testOut, setTestOut] = useState<string | null>(null)
  const [testOk, setTestOk] = useState(false)
  const [host, setHost] = useState('')
  const [port, setPort] = useState('443')
  const [certOut, setCertOut] = useState<CertData | null>(null)

  const runTest = async () => {
    if (!test) return
    const d = await run<{ text: string }>(test)
    if (d) {
      setTestOut(d.text.trim())
      // nginx -t prints "syntax is ok" / "test is successful" on success.
      setTestOk(/successful|syntax is ok/i.test(d.text))
    }
  }
  const runCert = async () => {
    if (!cert) return
    const d = await run<CertData>(cert, { host, port })
    if (d) setCertOut(d)
  }

  return (
    <div style={{ display: 'grid', gap: 16 }}>
      {(test || reload) && (
        <div className="section" style={{ margin: 0 }}>
          <h2>nginx</h2>
          <div className="card">
            <div className="card-body" style={{ display: 'grid', gap: 10 }}>
              <div className="row" style={{ gap: 8 }}>
                {test && (
                  <button className="btn" onClick={runTest}>
                    Test config
                  </button>
                )}
                {reload && canRun(reload, m.facts) && (
                  <button className="btn primary" disabled={!testOk} onClick={() => run(reload)} title={testOk ? '' : 'Run a passing config test first'}>
                    Reload
                  </button>
                )}
                {reload && !testOk && <span className="hint">Reload unlocks after the config test passes.</span>}
              </div>
              {testOut && <pre className={'output' + (testOk ? '' : ' err')}>{testOut}</pre>}
            </div>
          </div>
        </div>
      )}
      {cert && (
        <div className="section" style={{ margin: 0 }}>
          <h2>TLS certificate</h2>
          <div className="card">
            <div className="card-body" style={{ display: 'grid', gap: 12 }}>
              <div className="row" style={{ gap: 8 }}>
                <input className="input mono grow" value={host} onChange={(e) => setHost(e.target.value)} placeholder="example.com" spellCheck={false} />
                <input className="input mono" style={{ width: 90 }} value={port} onChange={(e) => setPort(e.target.value)} />
                <button className="btn" onClick={runCert} disabled={!host}>
                  Check
                </button>
              </div>
              {certOut &&
                (certOut.ok || certOut.notAfter ? (
                  <dl className="kv">
                    <dt>Subject</dt>
                    <dd>{certOut.subject}</dd>
                    <dt>Issuer</dt>
                    <dd>{certOut.issuer}</dd>
                    <dt>Valid from</dt>
                    <dd>{certOut.notBefore}</dd>
                    <dt>Expires</dt>
                    <dd>{certOut.notAfter}</dd>
                  </dl>
                ) : (
                  <div className="error-box">No certificate returned — the handshake to {host}:{port} failed.</div>
                ))}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

// --- edit connection ------------------------------------------------------------

// EditConnection changes a machine's SSH address / port / user / credential —
// the one thing "Link synced instances" can't do for an already-linked box.
// It exists chiefly for the case where a cloud instance's ephemeral public IP
// changed and ssh_host had already diverged from it before the auto-follow
// could catch it. Cloud identity, name and tags are preserved untouched.
function EditConnection({ m, onClose, onSaved }: { m: Machine; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [creds, setCreds] = useState<Credential[]>([])
  const [host, setHost] = useState(m.sshHost)
  const [port, setPort] = useState(m.sshPort || 22)
  const [user, setUser] = useState(m.sshUser)
  const [credentialId, setCredentialId] = useState<string>(m.credentialId ? String(m.credentialId) : '')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api.credentials.list().then(setCreds).catch(() => {})
  }, [])

  const save = async () => {
    setBusy(true)
    try {
      await api.machines.update(m.id, {
        name: m.name,
        tags: m.tags,
        sshHost: host.trim(),
        sshPort: Number(port) || 22,
        sshUser: user.trim(),
        credentialId: credentialId ? Number(credentialId) : null,
        cloudAccountId: m.cloudAccountId,
        instanceId: m.instanceId,
        provider: m.provider,
      })
      toast('Connection updated')
      onSaved()
    } catch (e: any) {
      toast(e.message, true)
      setBusy(false)
    }
  }

  const ipMismatch = m.publicIp && m.publicIp !== host
  return (
    <div className="modal-bg" onClick={busy ? undefined : onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="modal-head">
          <span className="dot" />
          <h3>Edit connection · {m.name}</h3>
        </div>
        <div className="modal-body">
          <div className="field">
            <label>Host or IP</label>
            <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} spellCheck={false} autoFocus />
            {ipMismatch && (
              <span className="hint">
                Provider reports <span className="mono">{m.publicIp}</span>.{' '}
                <button className="btn sm ghost" onClick={() => setHost(m.publicIp)}>
                  Use it
                </button>
              </span>
            )}
          </div>
          <div className="row" style={{ gap: 14 }}>
            <div className="field" style={{ width: 100 }}>
              <label>Port</label>
              <input className="input mono" type="number" min={1} max={65535} value={port} onChange={(e) => setPort(Number(e.target.value))} />
            </div>
            <div className="field grow">
              <label>SSH user</label>
              <input className="input mono" value={user} onChange={(e) => setUser(e.target.value)} spellCheck={false} placeholder="ec2-user, ubuntu, root…" />
            </div>
          </div>
          <div className="field">
            <label>Credential</label>
            <select className="select" value={credentialId} onChange={(e) => setCredentialId(e.target.value)}>
              <option value="">— none —</option>
              {creds.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.kind === 'ssh_key' ? 'key' : 'password'})
                </option>
              ))}
            </select>
          </div>
          <span className="hint">Changing the host or port re-pins the host key and re-fingerprints on the next probe.</span>
        </div>
        <div className="modal-foot">
          <button className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button className="btn primary" onClick={save} disabled={busy || !host.trim() || !user.trim()}>
            {busy ? 'Saving…' : 'Save & probe'}
          </button>
        </div>
      </div>
    </div>
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
      `The SSH host key for ${m.name} does not match the one CloudRoof pinned.\n\n` +
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
