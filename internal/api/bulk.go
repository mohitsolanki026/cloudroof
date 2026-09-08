package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"cloudroof/internal/actions"
	"cloudroof/internal/store"
)

// Bulk runs one action across many machines — a group, or an explicit set.
//
// Confirmation is escalated over the single-machine gate: a read needs a
// click; a write needs the operator to type the number of affected hosts,
// after a preview that lists them by name. Each per-machine execution goes
// through engine.Run, so it is gated, sudo-decided, and audited exactly like a
// single action — the bulk layer only adds the fan-out and the escalation.

type bulkInput struct {
	MachineIDs  []int64           `json:"machineIds"`
	GroupID     *int64            `json:"groupId"`
	Params      map[string]string `json:"params"`
	Confirm     bool              `json:"confirm"`     // satisfies a Tier-0 bulk
	ConfirmText string            `json:"confirmText"` // must equal the host count for a write
}

type bulkResult struct {
	MachineID   int64  `json:"machineId"`
	MachineName string `json:"machineName"`
	OK          bool   `json:"ok"`
	ExitCode    *int   `json:"exitCode"`
	Error       string `json:"error"`
}

const bulkConcurrency = 5

func (s *Server) runBulk(w http.ResponseWriter, r *http.Request) {
	actionID := r.PathValue("action")
	act, ok := s.engine.Resolve(actionID)
	if !ok {
		badRequest(w, "unknown action")
		return
	}
	var in bulkInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}

	// Resolve targets from a group or an explicit list, then keep only the
	// ones we can actually SSH to.
	var candidates []store.Machine
	if in.GroupID != nil {
		members, err := s.store.GroupMembers(*in.GroupID)
		if err != nil {
			fail(w, err)
			return
		}
		candidates = members
	} else {
		for _, id := range in.MachineIDs {
			if m, err := s.store.GetMachine(id); err == nil {
				candidates = append(candidates, m)
			}
		}
	}
	targets := make([]store.Machine, 0, len(candidates))
	for _, m := range candidates {
		if m.HasHost() {
			targets = append(targets, m)
		}
	}
	n := len(targets)
	if n == 0 {
		writeJSON(w, 400, apiError{Error: "no targets with SSH configured", Code: "bad_request"})
		return
	}

	names := make([]string, n)
	for i, m := range targets {
		names[i] = m.Name
	}

	// Escalated gate. Details carry the blast radius so the UI can preview it.
	if act.Danger == actions.DangerRead {
		if !in.Confirm {
			writeJSON(w, 428, apiError{Error: "confirm the bulk read", Code: "needs_confirm",
				Details: map[string]any{"count": n, "machines": names}})
			return
		}
	} else if in.ConfirmText != strconv.Itoa(n) {
		writeJSON(w, 428, apiError{
			Error:   "type the number of affected hosts to confirm",
			Code:    "needs_count",
			Details: map[string]any{"count": n, "machines": names},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	results := make([]bulkResult, n)
	sem := make(chan struct{}, bulkConcurrency)
	var wg sync.WaitGroup
	for i, m := range targets {
		wg.Add(1)
		go func(i int, m store.Machine) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Each machine's own name satisfies engine.Run's Tier-2 gate; the
			// bulk gate above is what authorized the fan-out.
			res, err := s.engine.Run(ctx, actions.Request{
				MachineID: m.ID, ActionID: actionID, Params: in.Params,
				Confirm: true, ConfirmName: m.Name, Actor: actor,
			})
			br := bulkResult{MachineID: m.ID, MachineName: m.Name}
			if err != nil {
				br.Error = err.Error()
				if rs := classifyReach(err); rs != store.ReachUnknown {
					_ = s.store.SetReach(m.ID, rs, err.Error())
					s.broker.Drop(m.ID)
				}
			} else {
				br.ExitCode = res.Run.ExitCode
				br.OK = res.Run.ExitCode != nil && *res.Run.ExitCode == 0
			}
			results[i] = br
		}(i, m)
	}
	wg.Wait()

	writeJSON(w, 200, map[string]any{"count": n, "results": results})
}
