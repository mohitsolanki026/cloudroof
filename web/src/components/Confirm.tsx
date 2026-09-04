import { useEffect, useState } from 'react'

// One dialog, two gates. Tier 1 is a click. Tier 2 is typing the machine's
// name. There is no Tier 3 dialog because there are no Tier 3 buttons.

export interface ConfirmProps {
  title: string
  command: string
  danger: 1 | 2
  machineName: string
  busy?: boolean
  onConfirm: () => void
  onCancel: () => void
}

export function Confirm({ title, command, danger, machineName, busy, onConfirm, onCancel }: ConfirmProps) {
  const [typed, setTyped] = useState('')
  const ok = danger === 1 || typed === machineName

  // Escape is global. Enter is deliberately not: the Run button is focused
  // for Tier 1 so the browser's own activation handles it, and for Tier 2 the
  // name input handles it below. A window-level Enter would fire Run while
  // Cancel had focus.
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onCancel()
    }
    window.addEventListener('keydown', on)
    return () => window.removeEventListener('keydown', on)
  }, [busy, onCancel])

  return (
    <div className="modal-bg" onClick={busy ? undefined : onCancel}>
      <div className="modal" onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true">
        <div className="modal-head">
          <span className={'dot ' + (danger === 2 ? 'crit' : 'warn')} />
          <h3>{title}</h3>
        </div>
        <div className="modal-body">
          <p>
            This will run on <strong>{machineName}</strong>:
          </p>
          <div className="cmd-preview">{command}</div>
          {danger === 2 && (
            <div className="field">
              <label>Type the machine name to confirm</label>
              <input
                className="input mono"
                autoFocus
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && ok && !busy) onConfirm()
                }}
                placeholder={machineName}
                spellCheck={false}
                autoComplete="off"
              />
              <span className="hint">This action causes downtime or loses in-flight work.</span>
            </div>
          )}
        </div>
        <div className="modal-foot">
          <button className="btn" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button
            className={'btn ' + (danger === 2 ? 'danger' : 'primary')}
            onClick={onConfirm}
            disabled={!ok || busy}
            autoFocus={danger === 1}
          >
            {busy ? 'Running…' : 'Run'}
          </button>
        </div>
      </div>
    </div>
  )
}
