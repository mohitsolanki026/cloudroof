// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"cloudroof/internal/actions"
	"cloudroof/internal/store"
)

// Wire protocol, kept deliberately dumb:
//   - binary frames carry terminal bytes in both directions
//   - text frames carry JSON control messages: {"type":"resize","cols":N,"rows":N}
// The client is xterm.js; it neither needs nor wants anything smarter.

// The upgrader lives on the Server so its origin check shares the Host
// allowlist with the REST guard; see server.go.

type controlMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		fail(w, err)
		return
	}
	if !m.HasHost() {
		fail(w, actions.ErrNoHost)
		return
	}
	target, err := s.resolveTarget(m)
	if err != nil {
		fail(w, err)
		return
	}

	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already wrote the response
	}
	defer ws.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	shell, err := s.broker.OpenShell(ctx, target, cols, rows)
	if err != nil {
		_ = s.store.SetReach(m.ID, classifyReach(err), err.Error())
		writeWSError(ws, err.Error())
		return
	}
	defer shell.Close()

	// A terminal session is a Run too: it is the one place the user can do
	// anything, so the audit log must at least record that it was opened.
	if _, err := s.store.InsertRun(store.Run{
		MachineID:   &m.ID,
		MachineName: m.Name,
		ActionID:    "terminal.open",
		Command:     "(interactive shell)",
		Danger:      actions.DangerDisruptive,
		Actor:       actor,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		// Same rule as engine.Run: an unrecorded session is worse than a
		// refused one.
		writeWSError(ws, "audit log write failed: "+err.Error())
		return
	}
	_ = s.store.SetReach(m.ID, store.ReachOK, "")

	// remote -> browser
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := shell.Stdout.Read(buf)
			if n > 0 {
				ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// browser -> remote
	go func() {
		defer cancel()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if _, err := shell.Stdin.Write(data); err != nil {
					return
				}
			case websocket.TextMessage:
				var c controlMsg
				if json.Unmarshal(data, &c) == nil && c.Type == "resize" {
					_ = shell.Resize(c.Cols, c.Rows)
				}
			}
		}
	}()

	// Hold until either side hangs up or the shell exits.
	done := make(chan struct{})
	go func() {
		_ = shell.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shell exited"),
		time.Now().Add(time.Second))
}

func writeWSError(ws *websocket.Conn, msg string) {
	// Send as a text frame the client renders in the terminal, then close.
	_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","message":`+strconv.Quote(msg)+`}`))
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, msg),
		time.Now().Add(time.Second))
}
