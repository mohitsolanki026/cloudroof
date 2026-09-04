import { useEffect, useState, useCallback } from 'react'
import { api, hasHost, type Machine } from '../api'
import { StatusDots, ProviderChip } from '../components/Status'
import { ago } from '../lib/format'
import { useToast } from '../App'

const POLL_MS = 15_000

export function Fleet() {
  const [machines, setMachines] = useState<Machine[] | null>(null)
  const [filter, setFilter] = useState('')
  const [probing, setProbing] = useState<Set<number>>(new Set())
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

  const q = filter.trim().toLowerCase()
  const shown = (machines ?? []).filter(
    (m) =>
      !q ||
      m.name.toLowerCase().includes(q) ||
      m.provider.includes(q) ||
      m.publicIp.includes(q) ||
      m.sshHost.includes(q) ||
      m.tags.some((t) => t.toLowerCase().includes(q)) ||
      (m.facts?.osName ?? '').toLowerCase().includes(q),
  )

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
          <input
            className="input"
            placeholder="Filter by name, IP, tag, OS…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            style={{ width: 260 }}
          />
          <a className="btn primary" href="#/settings">
            Add machine
          </a>
        </div>
      </div>

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
                  <th style={{ width: 60 }}>Status</th>
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
                      {m.region && <span className="muted" style={{ marginLeft: 6, fontSize: 12 }}>{m.region}</span>}
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
    </>
  )
}
