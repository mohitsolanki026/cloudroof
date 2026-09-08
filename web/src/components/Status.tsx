import type { Machine, PowerState, ReachState } from '../api'
import { hasCloud, hasHost } from '../api'

// Two dots, never one. Power is what the provider says; reach is what SSH saw.
// Their disagreement is the most useful signal CloudRoof produces.

function powerClass(p: PowerState, linked: boolean): string {
  if (!linked) return 'off'
  switch (p) {
    case 'running':
      return 'ok'
    case 'stopped':
      return 'off'
    case 'pending':
      return 'pending'
    default:
      return 'off'
  }
}

function reachClass(r: ReachState, configured: boolean): string {
  if (!configured) return 'off'
  switch (r) {
    case 'ok':
      return 'ok'
    case 'auth_failed':
    case 'hostkey_changed':
      return 'warn'
    case 'refused':
    case 'timeout':
      return 'crit'
    default:
      return 'off'
  }
}

export function StatusDots({ m }: { m: Machine }) {
  const cloud = hasCloud(m)
  const host = hasHost(m)
  const power = cloud ? m.powerState : 'not linked'
  const reach = host ? m.reachState.replace('_', ' ') : 'no ssh'
  return (
    <span className="dots" title={`power: ${power} · ssh: ${reach}`}>
      <span className={'dot ' + powerClass(m.powerState, cloud)} />
      <span className={'dot ' + reachClass(m.reachState, host)} />
    </span>
  )
}

export function PowerChip({ m }: { m: Machine }) {
  if (!hasCloud(m)) return <span className="chip">unlinked</span>
  const cls = m.powerState === 'running' ? 'ok' : m.powerState === 'pending' ? 'warn' : ''
  return <span className={'chip ' + cls}>{m.powerState}</span>
}

export function ReachChip({ m }: { m: Machine }) {
  if (!hasHost(m)) return <span className="chip">no ssh</span>
  const cls =
    m.reachState === 'ok'
      ? 'ok'
      : m.reachState === 'unknown'
        ? ''
        : m.reachState === 'auth_failed' || m.reachState === 'hostkey_changed'
          ? 'warn'
          : 'crit'
  return (
    <span className={'chip ' + cls} title={m.reachError || undefined}>
      ssh {m.reachState.replace('_', ' ')}
    </span>
  )
}

export function ProviderChip({ provider }: { provider: string }) {
  if (!provider) return <span className="chip">manual</span>
  return <span className="chip accent">{provider}</span>
}
