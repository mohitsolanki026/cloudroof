import { useEffect, useState, createContext, useContext, useCallback } from 'react'
import { Fleet } from './pages/Fleet'
import { MachinePage } from './pages/Machine'
import { Activity } from './pages/Activity'
import { Settings } from './pages/Settings'
import { api, setUnauthorizedHandler, type AuthState } from './api'

// A hash router is ~20 lines and one fewer dependency. Routes:
//   #/                 fleet
//   #/machines/:id     machine detail (optional ?tab=)
//   #/activity
//   #/settings

function useHash() {
  const [hash, setHash] = useState(window.location.hash || '#/')
  useEffect(() => {
    const on = () => setHash(window.location.hash || '#/')
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return hash
}

export function navigate(to: string) {
  window.location.hash = to
}

// --- toasts -------------------------------------------------------------------

type Toast = { id: number; text: string; err?: boolean }
const ToastCtx = createContext<(text: string, err?: boolean) => void>(() => {})
export const useToast = () => useContext(ToastCtx)

function ToastHost({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const push = useCallback((text: string, err = false) => {
    const id = Date.now() + Math.random()
    setToasts((t) => [...t, { id, text, err }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), err ? 6000 : 3000)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts">
        {toasts.map((t) => (
          <div key={t.id} className={'toast' + (t.err ? ' err' : '')}>
            {t.text}
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

// --- auth gate ----------------------------------------------------------------

// AuthGate resolves who (if anyone) is logged in before the app renders. It
// shows first-run setup, the login screen, or the app, and a global 401 handler
// bounces an expired session straight back to login.
function AuthGate() {
  const [auth, setAuth] = useState<AuthState | null>(null)

  const refresh = useCallback(() => {
    api.auth
      .me()
      .then(setAuth)
      .catch(() => setAuth({ authDisabled: false, setupRequired: false, authenticated: false, user: null }))
  }, [])

  useEffect(() => {
    refresh()
    // A 401 from any call means the session lapsed: drop to logged-out so the
    // login screen shows without a manual reload.
    setUnauthorizedHandler(() => setAuth((a) => (a ? { ...a, authenticated: false, user: null } : a)))
    return () => setUnauthorizedHandler(null)
  }, [refresh])

  if (auth === null) return <div className="auth-loading">Loading…</div>
  if (!auth.authDisabled && !auth.authenticated) {
    return <AuthScreen mode={auth.setupRequired ? 'setup' : 'login'} onDone={refresh} />
  }
  return <Shell auth={auth} onLoggedOut={refresh} />
}

function AuthScreen({ mode, onDone }: { mode: 'login' | 'setup'; onDone: () => void }) {
  const toast = useToast()
  const [username, setUsername] = useState(mode === 'setup' ? 'admin' : '')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (mode === 'setup' && password !== confirm) {
      toast('Passwords do not match', true)
      return
    }
    setBusy(true)
    try {
      if (mode === 'setup') await api.auth.setup(username.trim(), password)
      else await api.auth.login(username.trim(), password)
      onDone()
    } catch (err: any) {
      toast(err.message, true)
      setBusy(false)
    }
  }

  return (
    <div className="auth-screen">
      <form className="auth-card" onSubmit={submit}>
        <div className="auth-brand">
          <span className="dot ok" /> CloudRoof
        </div>
        <h1>{mode === 'setup' ? 'Create your admin account' : 'Sign in'}</h1>
        {mode === 'setup' && (
          <p className="muted auth-hint">
            First-run setup. Choose the admin login for this CloudRoof instance — you can change it later in Settings.
          </p>
        )}
        <label>Username</label>
        <input
          className="input"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          autoFocus={mode === 'login'}
          autoComplete="username"
          spellCheck={false}
        />
        <label>Password</label>
        <input
          className="input"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete={mode === 'setup' ? 'new-password' : 'current-password'}
        />
        {mode === 'setup' && (
          <>
            <label>Confirm password</label>
            <input className="input" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" />
            <span className="hint">At least 10 characters.</span>
          </>
        )}
        <button className="btn primary auth-submit" type="submit" disabled={busy || !username.trim() || !password}>
          {busy ? '…' : mode === 'setup' ? 'Create account' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}

// --- shell --------------------------------------------------------------------

function Shell({ auth, onLoggedOut }: { auth: AuthState; onLoggedOut: () => void }) {
  const hash = useHash()
  const path = hash.replace(/^#/, '').split('?')[0]
  const query = new URLSearchParams(hash.split('?')[1] ?? '')

  let page: React.ReactNode
  const m = path.match(/^\/machines\/(\d+)$/)
  if (m) page = <MachinePage id={Number(m[1])} tab={query.get('tab') ?? 'overview'} />
  else if (path === '/activity') page = <Activity />
  else if (path === '/settings') page = <Settings />
  else page = <Fleet />

  const logout = async () => {
    try {
      await api.auth.logout()
    } finally {
      onLoggedOut()
    }
  }

  return (
    <div className="shell">
      <header className="topbar">
        <a className="brand" href="#/">
          <span className="dot ok" />
          CloudRoof
        </a>
        <nav className="nav">
          <a href="#/" className={path === '/' || path.startsWith('/machines') ? 'active' : ''}>
            Fleet
          </a>
          <a href="#/activity" className={path === '/activity' ? 'active' : ''}>
            Activity
          </a>
          <a href="#/settings" className={path === '/settings' ? 'active' : ''}>
            Settings
          </a>
        </nav>
        {!auth.authDisabled && auth.user && (
          <div className="userbox">
            <span className="muted" title={`Signed in as ${auth.user.username}`}>
              {auth.user.username}
            </span>
            <button className="btn ghost sm" onClick={logout}>
              Log out
            </button>
          </div>
        )}
      </header>
      <main className="main">{page}</main>
    </div>
  )
}

export function App() {
  return (
    <ToastHost>
      <AuthGate />
    </ToastHost>
  )
}
