// Typed client for the Bosun API. Types mirror internal/store/models.go and
// the handler DTOs in internal/api/handlers.go — keep them in sync by hand;
// the surface is small enough that codegen would cost more than it saves.

export type PowerState = 'running' | 'stopped' | 'pending' | 'unknown'
export type ReachState = 'ok' | 'refused' | 'timeout' | 'auth_failed' | 'hostkey_changed' | 'unknown'
export type CredKind = 'ssh_key' | 'password'
export type PowerAction = 'start' | 'stop' | 'reboot' | 'force_stop' | 'force_reboot'

export interface Facts {
  machineId: number
  hostname: string
  osName: string
  osVersion: string
  kernel: string
  arch: string
  initSystem: string
  sudoMode: string
  capabilities: string[]
  fetchedAt: string
}

export interface Machine {
  id: number
  name: string
  tags: string[]
  cloudAccountId: number | null
  provider: string
  instanceId: string
  region: string
  instanceType: string
  powerState: PowerState
  publicIp: string
  privateIp: string
  cloudSyncedAt: string | null
  missing: boolean
  sshHost: string
  sshPort: number
  sshUser: string
  credentialId: number | null
  reachState: ReachState
  reachError: string
  reachCheckedAt: string | null
  createdAt: string
  updatedAt: string
  facts: Facts | null
}

export interface MachineInput {
  name: string
  tags: string[]
  sshHost: string
  sshPort: number
  sshUser: string
  credentialId: number | null
  cloudAccountId: number | null
  instanceId: string
  provider: string
}

export interface Credential {
  id: number
  name: string
  kind: CredKind
  fingerprint: string
  createdAt: string
}

export interface CloudAccount {
  id: number
  name: string
  provider: string
  lastSyncAt: string | null
  lastSyncError: string
  createdAt: string
}

export interface ProviderField {
  name: string
  label: string
  kind: 'text' | 'secret' | 'list' | 'textarea'
  optional?: boolean
  hint?: string
}

// A provider declares the credential fields it needs; the Settings form
// renders from this, so a new adapter needs no UI work.
export interface ProviderSpec {
  name: string
  label: string
  fields: ProviderField[]
  notes?: string
}

// Mirrors provider.Credentials on the server.
export interface ProviderCredentials {
  token?: string
  accessKeyId?: string
  secretAccessKey?: string
  regions?: string[]
}

export interface Run {
  id: number
  machineId: number | null
  machineName: string
  actionId: string
  command: string
  danger: number
  actor: string
  exitCode: number | null
  stdout: string
  stderr: string
  error: string
  durationMs: number
  startedAt: string
}

export interface ActionParam {
  name: string
  label: string
  source?: string
}

// sudo mirrors actions.Sudo: '' never, 'preferred' when passwordless,
// 'required' or refuse.
export type SudoWant = '' | 'preferred' | 'required'

export interface Action {
  id: string
  label: string
  category: string
  requires: string[]
  command: string
  params: ActionParam[]
  danger: number
  sudo: SudoWant
  parse: string
  stream?: boolean
}

export interface ActionResult<T = unknown> {
  run: Run
  data: T
}

export interface HostKey {
  machineId: number
  algorithm: string
  fingerprint: string
  firstSeen: string
  seenAlgorithm: string
  seenFingerprint: string
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public body?: unknown,
  ) {
    super(message)
  }
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (res.status === 204) return undefined as T
  const text = await res.text()
  let json: any = null
  try {
    json = text ? JSON.parse(text) : null
  } catch {
    /* non-JSON error body */
  }
  if (!res.ok) {
    throw new ApiError(res.status, json?.code ?? 'internal', json?.error ?? text ?? res.statusText, json)
  }
  return json as T
}

// Go marshals a nil slice as null. The server now sends [] for these, but the
// client should not depend on it.
function normalizeActions(as: Action[]): Action[] {
  return (as ?? []).map((a) => ({ ...a, params: a.params ?? [], requires: a.requires ?? [] }))
}

export const api = {
  machines: {
    list: () => req<Machine[]>('GET', '/api/machines'),
    get: (id: number) => req<Machine>('GET', `/api/machines/${id}`),
    create: (m: MachineInput) => req<Machine>('POST', '/api/machines', m),
    update: (id: number, m: MachineInput) => req<Machine>('PUT', `/api/machines/${id}`, m),
    remove: (id: number) => req<void>('DELETE', `/api/machines/${id}`),
    probe: (id: number) => req<Machine>('POST', `/api/machines/${id}/probe`),
    power: (id: number, action: PowerAction, confirm: boolean, confirmName: string) =>
      req<Machine>('POST', `/api/machines/${id}/power`, { action, confirm, confirmName }),
    actions: (id: number) => req<Action[]>('GET', `/api/machines/${id}/actions`).then(normalizeActions),
    run: <T = unknown>(
      id: number,
      actionId: string,
      params: Record<string, string> = {},
      confirm = false,
      confirmName = '',
    ) => req<ActionResult<T>>('POST', `/api/machines/${id}/actions/${actionId}`, { params, confirm, confirmName }),
    // Live follow (journalctl -f, docker logs -f): a websocket, not a fetch.
    // Params ride in the query string, same names the buffered actions take.
    streamURL: (id: number, actionId: string, params: Record<string, string> = {}) => {
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      const qs = new URLSearchParams(params).toString()
      return `${proto}://${location.host}/api/machines/${id}/actions/${actionId}/stream${qs ? '?' + qs : ''}`
    },
    hostKey: (id: number) => req<HostKey>('GET', `/api/machines/${id}/hostkey`),
    trustHostKey: (id: number, algorithm: string, fingerprint: string) =>
      req<void>('POST', `/api/machines/${id}/hostkey/trust`, { algorithm, fingerprint }),
  },
  credentials: {
    list: () => req<Credential[]>('GET', '/api/credentials'),
    create: (c: { name: string; kind: CredKind; privateKey: string; passphrase: string; password: string }) =>
      req<Credential>('POST', '/api/credentials', c),
    remove: (id: number) => req<void>('DELETE', `/api/credentials/${id}`),
  },
  accounts: {
    list: () => req<CloudAccount[]>('GET', '/api/accounts'),
    create: (a: { name: string; provider: string; credentials: ProviderCredentials }) => req<CloudAccount>('POST', '/api/accounts', a),
    remove: (id: number) => req<void>('DELETE', `/api/accounts/${id}`),
    sync: (id: number) => req<{ total: number; created: number; updated: number }>('POST', `/api/accounts/${id}/sync`),
  },
  runs: {
    list: (machineId?: number, limit = 100) =>
      req<Run[]>('GET', `/api/runs?limit=${limit}${machineId ? `&machine=${machineId}` : ''}`),
  },
  catalog: () => req<Action[]>('GET', '/api/catalog').then(normalizeActions),
  providers: () => req<ProviderSpec[]>('GET', '/api/providers'),
}

// hasCloud / hasHost mirror the Go methods, so the UI gates the same way the
// server does and never offers a button the server would refuse.
export const hasCloud = (m: Machine) => m.instanceId !== '' && m.cloudAccountId !== null
export const hasHost = (m: Machine) => m.sshHost !== '' && m.sshUser !== '' && m.credentialId !== null

// renderCommand previews what a button will run, client-side, from the
// template. It matches the server's render() closely enough for display;
// the server is still the only thing that executes.
export function renderCommand(a: Action, params: Record<string, string>, facts: Facts | null): string {
  if (a.parse === 'overview') return 'sh -s  # batched overview probe'
  let cmd = a.command
  for (const p of a.params ?? []) {
    const v = params[p.name] ?? `{{${p.name}}}`
    cmd = cmd.replaceAll(`{{${p.name}}}`, `'${v}'`)
  }
  if (facts && facts.sudoMode !== 'root') {
    if (a.sudo === 'required' || (a.sudo === 'preferred' && facts.sudoMode === 'nopasswd')) cmd = 'sudo -n ' + cmd
  }
  return cmd
}

// canRun mirrors applySudo on the server: false only when the action requires
// root and the host cannot grant it without a prompt.
export function canRun(a: Action, facts: Facts | null): boolean {
  if (a.sudo !== 'required') return true
  if (!facts) return false
  return facts.sudoMode === 'root' || facts.sudoMode === 'nopasswd'
}
