import { useEffect, useState } from 'react'
import { api, type Run } from '../api'
import { timestamp, ms } from '../lib/format'

// The trust surface: every run, newest first, expandable to full output.

function outcome(r: Run) {
  if (r.error) return <span className="chip crit">error</span>
  if (r.exitCode === null) return <span className="chip">—</span>
  if (r.exitCode === 0) return <span className="chip ok">exit 0</span>
  return <span className="chip warn">exit {r.exitCode}</span>
}

export function RunList({ machineId, limit = 100 }: { machineId?: number; limit?: number }) {
  const [runs, setRuns] = useState<Run[] | null>(null)
  const [open, setOpen] = useState<number | null>(null)

  useEffect(() => {
    api.runs.list(machineId, limit).then(setRuns)
  }, [machineId, limit])

  if (runs === null) return <div className="empty">Loading…</div>
  if (runs.length === 0)
    return (
      <div className="empty">
        <strong>Nothing yet</strong>
        Every action, power change, and terminal session will be recorded here.
      </div>
    )

  return (
    <div className="tw">
      <table>
        <thead>
          <tr>
            <th>When</th>
            {!machineId && <th>Machine</th>}
            <th>Action</th>
            <th>Command</th>
            <th>Tier</th>
            <th>Result</th>
            <th className="num">Took</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <RunRow
              key={r.id}
              r={r}
              showMachine={!machineId}
              open={open === r.id}
              onToggle={() => setOpen(open === r.id ? null : r.id)}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function RunRow({ r, showMachine, open, onToggle }: { r: Run; showMachine: boolean; open: boolean; onToggle: () => void }) {
  const cols = showMachine ? 7 : 6
  return (
    <>
      <tr className="clickable" onClick={onToggle}>
        <td className="mono muted" style={{ whiteSpace: 'nowrap' }}>
          {timestamp(r.startedAt)}
        </td>
        {showMachine && (
          <td>
            {r.machineId ? <a href={`#/machines/${r.machineId}`} onClick={(e) => e.stopPropagation()}>{r.machineName}</a> : r.machineName}
          </td>
        )}
        <td className="mono">{r.actionId}</td>
        <td className="mono" style={{ maxWidth: 360, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {r.command}
        </td>
        <td>
          <span className={'chip ' + (r.danger >= 2 ? 'crit' : r.danger === 1 ? 'warn' : '')}>T{r.danger}</span>
        </td>
        <td>{outcome(r)}</td>
        <td className="num muted">{ms(r.durationMs)}</td>
      </tr>
      {open && (
        <tr>
          <td colSpan={cols} style={{ background: 'var(--surface-2)' }}>
            <div style={{ display: 'grid', gap: 10 }}>
              <div className="cmd-preview">{r.command}</div>
              {r.error && <div className="error-box">{r.error}</div>}
              {r.stdout && <pre className="output">{r.stdout}</pre>}
              {r.stderr && <pre className="output err">{r.stderr}</pre>}
              {!r.stdout && !r.stderr && !r.error && <span className="hint">No output.</span>}
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

export function Activity() {
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Activity</h1>
          <div className="sub">Append-only. Every command Bosun has run, including the ones that failed.</div>
        </div>
      </div>
      <div className="card">
        <RunList limit={200} />
      </div>
    </>
  )
}
