import { useEffect, useRef } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'

// Bytes in, bytes out. Binary frames are terminal data; text frames are JSON
// control (resize from us, error from the server).

export function Terminal({ machineId }: { machineId: number }) {
  const host = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = host.current
    if (!el) return

    const term = new XTerm({
      cursorBlink: true,
      fontFamily: "'IBM Plex Mono', ui-monospace, Menlo, monospace",
      fontSize: 13,
      lineHeight: 1.2,
      scrollback: 5000,
      theme: {
        background: '#0e1016',
        foreground: '#e9ebf1',
        cursor: '#8098ff',
        selectionBackground: '#2f4fd855',
        black: '#1c202b',
        brightBlack: '#5c6273',
        red: '#f27668',
        green: '#5fc98d',
        yellow: '#dfa945',
        blue: '#8098ff',
        magenta: '#c792ea',
        cyan: '#7fd1d8',
        white: '#e9ebf1',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(el)
    fit.fit()

    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(
      `${proto}://${location.host}/api/machines/${machineId}/terminal?cols=${term.cols}&rows=${term.rows}`,
    )
    ws.binaryType = 'arraybuffer'

    const enc = new TextEncoder()
    let open = false

    ws.onopen = () => {
      open = true
      term.focus()
    }
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(ev.data))
      } else if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data)
          if (msg.type === 'error') term.writeln(`\r\n\x1b[31m${msg.message}\x1b[0m`)
        } catch {
          term.write(ev.data)
        }
      }
    }
    ws.onclose = (ev) => {
      open = false
      term.writeln(`\r\n\x1b[90m[connection closed${ev.reason ? ': ' + ev.reason : ''}]\x1b[0m`)
    }
    ws.onerror = () => term.writeln('\r\n\x1b[31m[websocket error]\x1b[0m')

    const onData = term.onData((d) => {
      if (open) ws.send(enc.encode(d))
    })
    const onResize = term.onResize(({ cols, rows }) => {
      if (open) ws.send(JSON.stringify({ type: 'resize', cols, rows }))
    })

    const ro = new ResizeObserver(() => fit.fit())
    ro.observe(el)

    return () => {
      ro.disconnect()
      onData.dispose()
      onResize.dispose()
      ws.close()
      term.dispose()
    }
  }, [machineId])

  return (
    <div className="term-wrap">
      <div ref={host} style={{ height: '100%' }} />
    </div>
  )
}
