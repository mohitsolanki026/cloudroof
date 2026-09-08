import { useEffect, useState, createContext, useContext, useCallback } from 'react'
import { Fleet } from './pages/Fleet'
import { MachinePage } from './pages/Machine'
import { Activity } from './pages/Activity'
import { Settings } from './pages/Settings'

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

// --- shell --------------------------------------------------------------------

export function App() {
  const hash = useHash()
  const path = hash.replace(/^#/, '').split('?')[0]
  const query = new URLSearchParams(hash.split('?')[1] ?? '')

  let page: React.ReactNode
  const m = path.match(/^\/machines\/(\d+)$/)
  if (m) page = <MachinePage id={Number(m[1])} tab={query.get('tab') ?? 'overview'} />
  else if (path === '/activity') page = <Activity />
  else if (path === '/settings') page = <Settings />
  else page = <Fleet />

  const active = (p: string) => (path === p || (p !== '/' && path.startsWith(p)) ? 'active' : '')

  return (
    <ToastHost>
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
            <a href="#/activity" className={active('/activity')}>
              Activity
            </a>
            <a href="#/settings" className={active('/settings')}>
              Settings
            </a>
          </nav>
        </header>
        <main className="main">{page}</main>
      </div>
    </ToastHost>
  )
}
