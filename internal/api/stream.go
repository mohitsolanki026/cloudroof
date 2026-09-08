// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"cloudroof/internal/actions"
	"cloudroof/internal/store"
)

// streamAction runs a follow action (journalctl -f, docker logs -f,
// supervisorctl tail -f) and pipes its output to the browser over a
// websocket, one text frame per line. It is read-only by construction: only
// actions.Action.Stream actions reach here, and those are all Tier 0.
//
// Params arrive as query values (it is a GET upgrade), the same names the
// buffered actions take as JSON.
func (s *Server) streamAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	actionID := r.PathValue("action")
	act, ok := actions.Lookup(actionID)
	if !ok {
		badRequest(w, "unknown action")
		return
	}
	if !act.Stream {
		badRequest(w, "action is not streamable")
		return
	}

	params := map[string]string{}
	for _, p := range act.Params {
		params[p.Name] = r.URL.Query().Get(p.Name)
	}

	prepared, err := s.engine.Prepare(actions.Request{
		MachineID: id,
		ActionID:  actionID,
		Params:    params,
		Actor:     actor,
	})
	if err != nil {
		// No websocket yet — a normal HTTP error is the honest response.
		fail(w, err)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// A follow is a Run too: record that a live view was opened, with the
	// command, so the audit log shows it happened. Output is unbounded and
	// not stored.
	if _, err := s.store.InsertRun(store.Run{
		MachineID:   &prepared.Machine.ID,
		MachineName: prepared.Machine.Name,
		ActionID:    act.ID,
		Command:     prepared.Command + "  (streamed)",
		Danger:      act.Danger,
		Actor:       actor,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		writeWSError(ws, "audit log write failed: "+err.Error())
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := s.broker.OpenStream(ctx, prepared.Target, prepared.Shell, strings.NewReader(prepared.Script))
	if err != nil {
		_ = s.store.SetReach(prepared.Machine.ID, classifyReach(err), err.Error())
		writeWSError(ws, err.Error())
		return
	}
	defer stream.Close()
	_ = s.store.SetReach(prepared.Machine.ID, store.ReachOK, "")

	// A client control frame (or a close) cancels the context, which closes
	// the stream. We do not read anything meaningful from the browser.
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Line-oriented: a follow is text, and one frame per line lets the client
	// cap scrollback trivially. Long lines are still delivered whole.
	sc := bufio.NewScanner(stream.Output)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			goto done
		default:
		}
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteMessage(websocket.TextMessage, sc.Bytes()); err != nil {
			goto done
		}
	}
done:
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stream ended"),
		time.Now().Add(time.Second))
}
