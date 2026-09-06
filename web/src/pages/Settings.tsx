import { useEffect, useState, type FormEvent } from 'react'
import { api, type Credential, type CloudAccount, type CredKind, type Machine, type ProviderSpec, type ProviderCredentials } from '../api'
import { ago } from '../lib/format'
import { useToast } from '../App'

export function Settings() {
  const [creds, setCreds] = useState<Credential[]>([])
  const [accounts, setAccounts] = useState<CloudAccount[]>([])
  const [providers, setProviders] = useState<ProviderSpec[]>([])
  const [machines, setMachines] = useState<Machine[]>([])
  const toast = useToast()

  const reload = async () => {
    const [c, a, p, m] = await Promise.all([api.credentials.list(), api.accounts.list(), api.providers(), api.machines.list()])
    setCreds(c)
    setAccounts(a)
    setProviders(p)
    setMachines(m)
  }
  useEffect(() => {
    reload().catch((e) => toast(e.message, true))
  }, [])

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Settings</h1>
          <div className="sub">Credentials are sealed with the master key in your data directory and never leave this host.</div>
        </div>
      </div>

      <AddMachine creds={creds} accounts={accounts} onDone={reload} />
      <Credentials creds={creds} onDone={reload} />
      <Accounts accounts={accounts} providers={providers} onDone={reload} />
      <LinkMachines machines={machines} creds={creds} onDone={reload} />
    </>
  )
}

// --- add machine --------------------------------------------------------------

function AddMachine({ creds, accounts, onDone }: { creds: Credential[]; accounts: CloudAccount[]; onDone: () => Promise<void> }) {
  const [f, setF] = useState({ name: '', sshHost: '', sshPort: 22, sshUser: 'root', credentialId: '' as string, tags: '' })
  const [busy, setBusy] = useState(false)
  const toast = useToast()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      const m = await api.machines.create({
        name: f.name,
        tags: f.tags.split(',').map((t) => t.trim()).filter(Boolean),
        sshHost: f.sshHost,
        sshPort: Number(f.sshPort),
        sshUser: f.sshUser,
        credentialId: f.credentialId ? Number(f.credentialId) : null,
        cloudAccountId: null,
        instanceId: '',
        provider: '',
      })
      toast(`Added ${m.name}`)
      setF({ name: '', sshHost: '', sshPort: 22, sshUser: 'root', credentialId: '', tags: '' })
      await onDone()
      // Probe immediately so the first thing the user sees is real state.
      if (m.sshHost && f.credentialId) {
        api.machines.probe(m.id).catch((err) => toast(`${m.name}: ${err.message}`, true))
      }
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="section">
      <h2>Add a machine by SSH</h2>
      <div className="card">
        <form className="card-body form cols-2" onSubmit={submit}>
          <div className="field">
            <label>Name</label>
            <input className="input" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} placeholder="db-primary" />
          </div>
          <div className="field">
            <label>Tags (comma separated)</label>
            <input className="input" value={f.tags} onChange={(e) => setF({ ...f, tags: e.target.value })} placeholder="prod, eu" />
          </div>
          <div className="field">
            <label>Host or IP</label>
            <input className="input mono" required value={f.sshHost} onChange={(e) => setF({ ...f, sshHost: e.target.value })} placeholder="203.0.113.10" />
          </div>
          <div className="row" style={{ gap: 14 }}>
            <div className="field" style={{ width: 90 }}>
              <label>Port</label>
              <input className="input mono" type="number" min={1} max={65535} value={f.sshPort} onChange={(e) => setF({ ...f, sshPort: Number(e.target.value) })} />
            </div>
            <div className="field grow">
              <label>User</label>
              <input className="input mono" required value={f.sshUser} onChange={(e) => setF({ ...f, sshUser: e.target.value })} />
            </div>
          </div>
          <div className="field span">
            <label>Credential</label>
            <select className="select" value={f.credentialId} onChange={(e) => setF({ ...f, credentialId: e.target.value })}>
              <option value="">— none yet (add below) —</option>
              {creds.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.kind === 'ssh_key' ? c.fingerprint.slice(0, 19) + '…' : 'password'})
                </option>
              ))}
            </select>
          </div>
          {accounts.length > 0 && (
            <span className="hint span">
              Cloud instances from synced accounts appear in the fleet automatically — give them a user and credential in “Link synced instances” below.
            </span>
          )}
          <div className="form-actions">
            <button className="btn primary" disabled={busy}>
              {busy ? 'Adding…' : 'Add machine'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// --- credentials --------------------------------------------------------------

function Credentials({ creds, onDone }: { creds: Credential[]; onDone: () => Promise<void> }) {
  const [kind, setKind] = useState<CredKind>('ssh_key')
  const [f, setF] = useState({ name: '', privateKey: '', passphrase: '', password: '' })
  const [busy, setBusy] = useState(false)
  const toast = useToast()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      const c = await api.credentials.create({ ...f, kind })
      toast(`Saved credential ${c.name}`)
      setF({ name: '', privateKey: '', passphrase: '', password: '' })
      await onDone()
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (c: Credential) => {
    if (!confirm(`Delete credential "${c.name}"? Machines using it will lose SSH access until reassigned.`)) return
    await api.credentials.remove(c.id)
    await onDone()
  }

  return (
    <div className="section">
      <h2>SSH credentials</h2>
      <div className="card">
        {creds.length > 0 && (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Fingerprint</th>
                  <th>Added</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {creds.map((c) => (
                  <tr key={c.id}>
                    <td>
                      <strong>{c.name}</strong>
                    </td>
                    <td>
                      <span className="chip">{c.kind === 'ssh_key' ? 'key' : 'password'}</span>
                    </td>
                    <td className="mono muted">{c.fingerprint || '—'}</td>
                    <td className="muted">{ago(c.createdAt)}</td>
                    <td className="actions">
                      <button className="btn sm ghost" onClick={() => remove(c)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <form className="card-body form" onSubmit={submit} style={{ borderTop: creds.length ? '1px solid var(--line)' : 0 }}>
          <div className="row" style={{ gap: 14 }}>
            <div className="field grow">
              <label>Name</label>
              <input className="input" required value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} placeholder="deploy key" />
            </div>
            <div className="field">
              <label>Kind</label>
              <select className="select" value={kind} onChange={(e) => setKind(e.target.value as CredKind)}>
                <option value="ssh_key">SSH private key</option>
                <option value="password">Password</option>
              </select>
            </div>
          </div>
          {kind === 'ssh_key' ? (
            <>
              <div className="field">
                <label>Private key (PEM / OpenSSH)</label>
                <textarea
                  className="textarea"
                  required
                  value={f.privateKey}
                  onChange={(e) => setF({ ...f, privateKey: e.target.value })}
                  placeholder={'-----BEGIN OPENSSH PRIVATE KEY-----\n…'}
                  spellCheck={false}
                />
              </div>
              <div className="field">
                <label>Passphrase (if encrypted)</label>
                <input className="input" type="password" value={f.passphrase} onChange={(e) => setF({ ...f, passphrase: e.target.value })} autoComplete="off" />
              </div>
            </>
          ) : (
            <div className="field">
              <label>Password</label>
              <input className="input" type="password" required value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} autoComplete="new-password" />
              <span className="hint">Keys are strongly preferred. Passwords are supported so a stray VPS isn't rejected at the door.</span>
            </div>
          )}
          <div className="form-actions">
            <button className="btn primary" disabled={busy}>
              {busy ? 'Saving…' : 'Save credential'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// --- cloud accounts -----------------------------------------------------------

function Accounts({ accounts, providers, onDone }: { accounts: CloudAccount[]; providers: ProviderSpec[]; onDone: () => Promise<void> }) {
  const [name, setName] = useState('')
  const [providerName, setProviderName] = useState(providers[0]?.name ?? 'hetzner')
  const [values, setValues] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [syncing, setSyncing] = useState<number | null>(null)
  const toast = useToast()

  useEffect(() => {
    if (providers.length && !providers.some((p) => p.name === providerName)) setProviderName(providers[0].name)
  }, [providers, providerName])

  const spec = providers.find((p) => p.name === providerName)

  // The form renders from the provider's spec. Lists are typed comma-separated
  // and sent as arrays; everything else goes through as-is.
  const credentials = (): ProviderCredentials => {
    const c: Record<string, unknown> = {}
    for (const f of spec?.fields ?? []) {
      const v = (values[f.name] ?? '').trim()
      if (!v) continue
      c[f.name] = f.kind === 'list' ? v.split(',').map((s) => s.trim()).filter(Boolean) : v
    }
    return c as ProviderCredentials
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      const a = await api.accounts.create({ name, provider: providerName, credentials: credentials() })
      toast(`Connected ${a.name}`)
      setName('')
      setValues({})
      await onDone()
      await sync(a)
    } catch (e: any) {
      toast(e.message, true)
    } finally {
      setBusy(false)
    }
  }

  const sync = async (a: CloudAccount) => {
    setSyncing(a.id)
    try {
      const r = await api.accounts.sync(a.id)
      toast(`${a.name}: ${r.total} instances (${r.created} new)`)
      await onDone()
    } catch (e: any) {
      toast(`${a.name}: ${e.message}`, true)
    } finally {
      setSyncing(null)
    }
  }

  const remove = async (a: CloudAccount) => {
    if (!confirm(`Disconnect "${a.name}"? Its machines stay in the fleet but lose their cloud identity and power control.`)) return
    await api.accounts.remove(a.id)
    await onDone()
  }

  return (
    <div className="section">
      <h2>Cloud accounts</h2>
      <div className="card">
        {accounts.length > 0 && (
          <div className="tw">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Provider</th>
                  <th>Last sync</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => (
                  <tr key={a.id}>
                    <td>
                      <strong>{a.name}</strong>
                    </td>
                    <td>
                      <span className="chip accent">{providers.find((p) => p.name === a.provider)?.label ?? a.provider}</span>
                    </td>
                    <td>
                      <span className="muted">{ago(a.lastSyncAt)}</span>
                      {a.lastSyncError && (
                        <span className="chip crit" style={{ marginLeft: 8 }} title={a.lastSyncError}>
                          error
                        </span>
                      )}
                    </td>
                    <td className="actions">
                      <button className="btn sm" disabled={syncing === a.id} onClick={() => sync(a)}>
                        {syncing === a.id ? 'Syncing…' : 'Sync now'}
                      </button>
                      <button className="btn sm ghost" onClick={() => remove(a)}>
                        Disconnect
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <form className="card-body form" onSubmit={submit} style={{ borderTop: accounts.length ? '1px solid var(--line)' : 0 }}>
          <div className="row" style={{ gap: 14 }}>
            <div className="field grow">
              <label>Name</label>
              <input className="input" required value={name} onChange={(e) => setName(e.target.value)} placeholder={`${spec?.label ?? 'cloud'} — personal`} />
            </div>
            <div className="field">
              <label>Provider</label>
              <select
                className="select"
                value={providerName}
                onChange={(e) => {
                  setProviderName(e.target.value)
                  setValues({})
                }}
              >
                {providers.map((p) => (
                  <option key={p.name} value={p.name}>
                    {p.label}
                  </option>
                ))}
              </select>
            </div>
          </div>
          {spec?.fields.map((f) => (
            <div className="field" key={f.name}>
              <label>
                {f.label}
                {f.optional ? ' (optional)' : ''}
              </label>
              {f.kind === 'textarea' ? (
                <textarea
                  className="textarea"
                  required={!f.optional}
                  value={values[f.name] ?? ''}
                  onChange={(e) => setValues({ ...values, [f.name]: e.target.value })}
                  spellCheck={false}
                  placeholder={'{\n  "type": "service_account",\n  ...\n}'}
                />
              ) : (
                <input
                  className="input mono"
                  type={f.kind === 'secret' ? 'password' : 'text'}
                  required={!f.optional}
                  value={values[f.name] ?? ''}
                  onChange={(e) => setValues({ ...values, [f.name]: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                />
              )}
              {f.hint && <span className="hint">{f.hint}</span>}
            </div>
          ))}
          {spec?.notes && <span className="hint">{spec.notes}</span>}
          <div className="form-actions">
            <button className="btn primary" disabled={busy || !spec}>
              {busy ? 'Checking credentials…' : 'Connect account'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// --- link synced instances ------------------------------------------------------

// Synced instances arrive with an IP but no SSH user or credential. This is the
// one-field step that turns a cloud identity into a linked machine.
function LinkMachines({ machines, creds, onDone }: { machines: Machine[]; creds: Credential[]; onDone: () => Promise<void> }) {
  const toast = useToast()
  const unlinked = machines.filter((m) => m.instanceId && (!m.sshUser || m.credentialId === null))
  const [edits, setEdits] = useState<Record<number, { sshUser: string; credentialId: string }>>({})

  if (unlinked.length === 0) return null

  const save = async (m: Machine) => {
    const e = edits[m.id] ?? { sshUser: 'root', credentialId: '' }
    if (!e.credentialId) {
      toast('Pick a credential first', true)
      return
    }
    try {
      await api.machines.update(m.id, {
        name: m.name,
        tags: m.tags,
        sshHost: m.sshHost || m.publicIp,
        sshPort: m.sshPort || 22,
        sshUser: e.sshUser || 'root',
        credentialId: Number(e.credentialId),
        cloudAccountId: m.cloudAccountId,
        instanceId: m.instanceId,
        provider: m.provider,
      })
      toast(`Linked ${m.name}`)
      await onDone()
      api.machines.probe(m.id).catch((err) => toast(`${m.name}: ${err.message}`, true))
    } catch (err: any) {
      toast(err.message, true)
    }
  }

  return (
    <div className="section">
      <h2>Link synced instances</h2>
      <div className="card">
        <div className="tw">
          <table>
            <thead>
              <tr>
                <th>Instance</th>
                <th>Address</th>
                <th>SSH user</th>
                <th>Credential</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {unlinked.map((m) => {
                const e = edits[m.id] ?? { sshUser: 'root', credentialId: '' }
                const set = (patch: Partial<typeof e>) => setEdits({ ...edits, [m.id]: { ...e, ...patch } })
                return (
                  <tr key={m.id}>
                    <td>
                      <strong>{m.name}</strong> <span className="chip accent">{m.provider}</span>
                    </td>
                    <td className="mono">{m.publicIp || m.sshHost}</td>
                    <td>
                      <input className="input mono" style={{ width: 120 }} value={e.sshUser} onChange={(ev) => set({ sshUser: ev.target.value })} />
                    </td>
                    <td>
                      <select className="select" value={e.credentialId} onChange={(ev) => set({ credentialId: ev.target.value })}>
                        <option value="">— choose —</option>
                        {creds.map((c) => (
                          <option key={c.id} value={c.id}>
                            {c.name}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td className="actions">
                      <button className="btn sm primary" onClick={() => save(m)}>
                        Link
                      </button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
