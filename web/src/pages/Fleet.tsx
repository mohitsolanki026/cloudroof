import { useEffect, useState, useCallback } from 'react'
import { api, hasHost, ApiError, type Machine, type Group, type Action, type CustomAction, type BulkResult } from '../api'
import { StatusDots, ProviderChip } from '../components/Status'
import { ago } from '../lib/format'
import { useToast } from '../App'

const POLL_MS = 15_000

// A bulk-eligible action: built-ins and custom actions share this shape.
type BulkAction = { id: string; label: string; category: string; danger: number; params: { name: string; label: string }[] }

export function Fleet() {
  const [machines, setMachines] = useState<Machine[] | null>(null)
  const [groups, setGroups] = useState<Group[]>([])
  const [bulkActions, setBulkActions] = useState<BulkAction[]>([])
  const [filter, setFilter] = useState('')
  const [groupId, setGroupId] = useState<number | ''>('')
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [probing, setProbing] = useState<Set<number>>(new Set())
  const [bulk, setBulk] = useState<BulkAction | null>(null)
  const toast = useToast()

  const load = useCallback(async () => {
    try {
      setMachines(await api.machines.list())
    } catch (e: any) {
      toast(e.message, true)
    }
  }, [toast])

  useEffect(() => {
    load()
    const t = setInterval(load, POLL_MS)
    return () => clearInterval(t)
  }, [load])

  // Groups (for the filter) and the bulk action menu (catalog + custom).
  useEffect(() => {
    api.groups.list().then(setGroups).catch(() => {})
    Promise.all([api.catalog(), api.customActions.list()])
      .then(([cat, custom]) => {
        const fromCatalog: BulkAction[] = cat.map((a: Action) => ({ id: a.id, label: a.label, category: a.category, danger: a.danger, params: a.params }))
        const fromCustom: BulkAction[] = custom.map((c: CustomAction) => ({ id: c.id, label: c.label, category: c.category, danger: c.danger, params: c.params }))
        setBulkActions([...fromCatalog, ...fromCustom].filter((a) => a.danger <= 2))
      })
      .catch(() => {})
  }, [])

  const group = groups.find((g) => g.id === groupId)
  const q = filter.trim().toLowerCase()
  const shown = (machines ?? []).filter((m) => {
    if (group && !group.tags.every((t) => m.tags.includes(t))) return false
    return (
      !q ||
      m.name.toLowerCase().includes(q) ||
      m.provider.includes(q) ||
      m.publicIp.includes(q) ||
      m.sshHost.includes(q) ||
      m.tags.some((t) => t.toLowerCase().includes(q)) ||
      (m.facts?.osName ?? '').toLowerCase().includes(q)
    )
  })

  // Selection is scoped to machines that can actually run actions (have SSH).
  const selectable = shown.filter(hasHost)
  const allSelected = selectable.length > 0 && selectable.every((m) => selected.has(m.id))
  const toggleAll = () =>
    setSelected((prev) => {
      const next = new Set(prev)
      if (allSelected) selectable.forEach((m) => next.delete(m.id))
      else selectable.forEach((m) => next.add(m.id))
      return next
    })
  const toggle = (id: number) =>
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(id) ? next.delete(id) : next.add(id)
      return next
    })
  const selectedList = (machines ?? []).filter((m) => selected.has(m.id))

  const probe = async (m: Machine) => {
    setProbing((s) => new Set(s).add(m.id))
    try {
      await api.machines.probe(m.id)
      toast(`${m.name}: reachable`)
    } catch (e: any) {
      toast(`${m.name}: ${e.message}`, true)
    } finally {
      setProbing((s) => {
        const n = new Set(s)
        n.delete(m.id)
        return n
      })
      load()
    }
  }

  const counts = machines
    ? {
        total: machines.length,
        running: machines.filter((m) => m.powerState === 'running').length,
        reachable: machines.filter((m) => m.reachState === 'ok').length,
        attention: machines.filter(
          (m) =>
            (m.powerState === 'running' && hasHost(m) && ['refused', 'timeout'].includes(m.reachState)) ||
            m.reachState === 'hostkey_changed' ||
            m.reachState === 'auth_failed',
        ).length,
      }
    : null

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Fleet</h1>
          {counts && (
            <div className="sub">
              {counts.total} machines · {counts.running} running · {counts.reachable} reachable
              {counts.attention > 0 && (
                <>
                  {' '}
                  · <span style={{ color: 'var(--crit)' }}>{counts.attention} need attention</span>
                </>
              )}
            </div>
          )}
        </div>
        <div className="toolbar">
          <select className="select" value={groupId} onChange={(e) => setGroupId(e.target.value ? Number(e.target.value) : '')}>
            <option value="">All machines</option>
            {groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name} ({g.members})
              </option>
            ))}
          </select>
          <input className="input" placeholder="Filter by name, IP, tag, OS…" value={filter} onChange={(e) => setFilter(e.target.value)} style={{ width: 240 }} />
          <a className="btn primary" href="#/settings">
            Add machine
          </a>
        </div>
      </div>

      {selected.size > 0 && (
        <div className="bulk-bar">
          <span className="bulk-count">{selected.size} selected</span>
          <select
            className="select"
            value={bulk?.id ?? ''}
            onChange={(e) => setBulk(bulkActions.find((a) => a.id === e.target.value) ?? null)}
          >
            <option value="">Choose a bulk action…</option>
            {bulkActions.map((a) => (
              <option key={a.id} value={a.id}>
                {a.category} · {a.label} {a.danger >= 2 ? '(disruptive)' : a.danger === 1 ? '(write)' : ''}
              </option>
            ))}
          </select>
          <span className="grow" />
          <button className="btn ghost sm" onClick={() => setSelected(new Set())}>
            Clear selection
          </button>
        </div>
      )}

      <div className="card">
        {machines === null ? (
          <div className="empty">Loading…</div>
        ) : machines.length === 0 ? (
          <div className="empty">
            <strong>No machines yet</strong>
            Add an SSH host or connect a cloud account in <a href="#/settings">Settings</a>.
          </div>
        ) : (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th style={{ width: 28 }}>
                    <input type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="Select all" disabled={selectable.length === 0} />
                  </th>
                  <th style={{ width: 56 }}>Status</th>
                  <th>Name</th>
                  <th>Provider</th>
                  <th>Address</th>
                  <th>OS</th>
                  <th>Tags</th>
                  <th>Checked</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shown.map((m) => (
                  <tr key={m.id} className="clickable" onClick={() => (location.hash = `#/machines/${m.id}`)}>
                    <td onClick={(e) => e.stopPropagation()}>
                      <input type="checkbox" checked={selected.has(m.id)} onChange={() => toggle(m.id)} disabled={!hasHost(m)} aria-label={`Select ${m.name}`} />
                    </td>
                    <td>
                      <StatusDots m={m} />
                    </td>
                    <td>
                      <strong>{m.name}</strong>
                      {m.missing && (
                        <span className="chip warn" style={{ marginLeft: 8 }}>
                          missing
                        </span>
                      )}
                      {m.reachState === 'hostkey_changed' && (
                        <span className="chip warn" style={{ marginLeft: 8 }}>
                          host key changed
                        </span>
                      )}
                    </td>
                    <td>
                      <ProviderChip provider={m.provider} />
                      {m.region && (
                        <span className="muted" style={{ marginLeft: 6, fontSize: 12 }}>
                          {m.region}
                        </span>
                      )}
                    </td>
                    <td className="mono">{m.publicIp || m.sshHost || <span className="muted">—</span>}</td>
                    <td>
                      {m.facts ? (
                        <>
                          {m.facts.osName} {m.facts.osVersion}
                          <span className="muted" style={{ fontSize: 12, marginLeft: 6 }}>
                            {m.facts.initSystem}
                          </span>
                        </>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td>
                      {m.tags.slice(0, 4).map((t) => (
                        <span className="tag" key={t}>
                          {t}
                        </span>
                      ))}
                    </td>
                    <td className="muted" style={{ fontSize: 12.5 }}>
                      {ago(m.reachCheckedAt)}
                    </td>
                    <td className="actions" onClick={(e) => e.stopPropagation()}>
                      {hasHost(m) && (
                        <button className="btn sm" disabled={probing.has(m.id)} onClick={() => probe(m)}>
                          {probing.has(m.id) ? '…' : 'Probe'}
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

      {bulk && (
        <BulkDialog
          action={bulk}
          machines={selectedList}
          onClose={() => setBulk(null)}
          onDone={() => {
            setBulk(null)
            load()
          }}
        />
      )}
    </>
  )
}

// BulkDialog: fill params → preview blast radius (server returns it as a 428)
// → confirm (type the host count for writes) → run → per-machine results.
function BulkDialog({ action, machines, onClose, onDone }: { action: BulkAction; machines: Machine[]; onClose: () => void; onDone: () => void }) {
  const toast = useToast()
  const [step, setStep] = useState<'params' | 'confirm' | 'running' | 'results'>(action.params.length ? 'params' : 'confirm')
  const [params, setParams] = useState<Record<string, string>>({})
  const [preview, setPreview] = useState<{ count: number; machines: string[] } | null>(null)
  const [typed, setTyped] = useState('')
  const [results, setResults] = useState<BulkResult[] | null>(null)

  const ids = machines.map((m) => m.id)
  const isWrite = action.danger >= 1

  // Ask the server for the blast radius. It replies 428 with count + names.
  const fetchPreview = useCallback(async () => {
    try {
      await api.bulk(action.id, { machineIds: ids, params })
      // No 428 means nothing to confirm; treat as an immediate go.
      setPreview({ count: ids.length, machines: machines.map((m) => m.name) })
    } catch (e: any) {
      if (e instanceof ApiError && (e.code === 'needs_count' || e.code === 'needs_confirm')) {
        const d = (e.body as any)?.details ?? {}
        setPreview({ count: d.count ?? ids.length, machines: d.machines ?? machines.map((m) => m.name) })
      } else {
        toast(e.message, true)
        onClose()
      }
    }
  }, [action.id]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (step === 'confirm' && !preview) fetchPreview()
  }, [step, preview, fetchPreview])

  const run = async () => {
    if (!preview) return
    setStep('running')
    try {
      const res = await api.bulk(action.id, {
        machineIds: ids,
        params,
        confirm: true,
        confirmText: String(preview.count),
      })
      setResults(res.results)
      setStep('results')
      const failed = res.results.filter((r) => !r.ok).length
      toast(failed ? `${action.label}: ${res.count - failed}/${res.count} ok, ${failed} failed` : `${action.label}: all ${res.count} ok`, failed > 0)
    } catch (e: any) {
      toast(e.message, true)
      setStep('confirm')
    }
  }

  const canRun = preview && (!isWrite || typed === String(preview.count))

  return (
    <div className="modal-bg" onClick={step === 'running' ? undefined : onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="modal-head">
          <span className={'dot ' + (action.danger >= 2 ? 'crit' : action.danger === 1 ? 'warn' : 'ok')} />
          <h3>
            Bulk: {action.label} <span className="mono muted" style={{ fontSize: 12 }}>{action.id}</span>
          </h3>
        </div>

        <div className="modal-body">
          {step === 'params' && (
            <>
              <p>Set the parameters — they apply to every selected machine.</p>
              {action.params.map((p) => (
                <div className="field" key={p.name}>
                  <label>{p.label || p.name}</label>
                  <input className="input mono" value={params[p.name] ?? ''} onChange={(e) => setParams({ ...params, [p.name]: e.target.value })} spellCheck={false} autoFocus />
                </div>
              ))}
            </>
          )}

          {step === 'confirm' &&
            (!preview ? (
              <p className="muted">Resolving targets…</p>
            ) : (
              <>
                <p>
                  This runs <strong>{action.label}</strong> on <strong>{preview.count}</strong> machine{preview.count === 1 ? '' : 's'}:
                </p>
                <div className="bulk-hosts">
                  {preview.machines.map((n) => (
                    <span className="tag" key={n}>
                      {n}
                    </span>
                  ))}
                </div>
                {isWrite && (
                  <div className="field">
                    <label>Type {preview.count} to confirm</label>
                    <input className="input mono" autoFocus value={typed} onChange={(e) => setTyped(e.target.value)} placeholder={String(preview.count)} />
                    <span className="hint">This affects {preview.count} hosts and causes changes on each.</span>
                  </div>
                )}
              </>
            ))}

          {step === 'running' && <p className="muted">Running on {preview?.count} machines…</p>}

          {step === 'results' && results && (
            <div className="tw" style={{ maxHeight: 360 }}>
              <table>
                <thead>
                  <tr>
                    <th>Machine</th>
                    <th>Result</th>
                  </tr>
                </thead>
                <tbody>
                  {results.map((r) => (
                    <tr key={r.machineId}>
                      <td>{r.machineName}</td>
                      <td>
                        {r.error ? (
                          <span className="chip crit" title={r.error}>
                            error
                          </span>
                        ) : r.ok ? (
                          <span className="chip ok">exit 0</span>
                        ) : (
                          <span className="chip warn">exit {r.exitCode}</span>
                        )}
                        {r.error && <span className="muted" style={{ marginLeft: 8, fontSize: 12 }}>{r.error}</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div className="modal-foot">
          {step === 'params' && (
            <>
              <button className="btn" onClick={onClose}>
                Cancel
              </button>
              <button className="btn primary" onClick={() => setStep('confirm')} disabled={action.params.some((p) => !(params[p.name] ?? '').trim())}>
                Continue
              </button>
            </>
          )}
          {step === 'confirm' && (
            <>
              <button className="btn" onClick={onClose}>
                Cancel
              </button>
              <button className={'btn ' + (action.danger >= 2 ? 'danger' : 'primary')} onClick={run} disabled={!canRun}>
                Run on {preview?.count ?? ids.length}
              </button>
            </>
          )}
          {step === 'running' && (
            <button className="btn" disabled>
              Running…
            </button>
          )}
          {step === 'results' && (
            <button className="btn primary" onClick={onDone}>
              Done
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
