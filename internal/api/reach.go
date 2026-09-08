// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"cloudroof/internal/sshx"
	"cloudroof/internal/store"
)

// sshExit0 is the cheapest possible liveness command: read the script, exit 0.
// Short timeout and tiny output cap so a slow host fails fast.
func sshExit0() sshx.ExecOpts {
	return sshx.ExecOpts{
		Stdin:     strings.NewReader("exit 0\n"),
		Timeout:   12 * time.Second,
		MaxOutput: 256,
	}
}

// The reachability poller keeps the second status dot honest without the user
// clicking Probe. It is deliberately cheap: a trivial `exit 0` over the pooled
// SSH connection, not a full fingerprint. Healthy hosts are re-checked at a
// steady interval; failing ones back off so a dead box is not hammered.
const (
	reachHealthy = 60 * time.Second
	reachBackoff = 15 * time.Minute // cap for a persistently-down host
	reachTick    = 20 * time.Second // how often the loop wakes to see what is due
	reachFanout  = 4                // concurrent checks per tick
)

type reachSched struct {
	next  time.Time
	fails int
}

// StartReachPoller runs the reachability loop until ctx is cancelled.
func (s *Server) StartReachPoller(ctx context.Context) {
	go func() {
		sched := map[int64]*reachSched{}
		t := time.NewTicker(reachTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reachSweep(ctx, sched)
			}
		}
	}()
}

func (s *Server) reachSweep(ctx context.Context, sched map[int64]*reachSched) {
	machines, err := s.store.ListMachines()
	if err != nil {
		return
	}
	now := time.Now()
	seen := map[int64]bool{}
	sem := make(chan struct{}, reachFanout)
	var wg sync.WaitGroup

	for _, m := range machines {
		seen[m.ID] = true
		if !m.HasHost() {
			continue
		}
		// A stopped instance is expected to be unreachable; don't probe it or
		// wave a red dot. It flips back to unknown on power-on (broker.Drop in
		// powerMachine) and the next sweep re-checks.
		if m.HasCloud() && m.PowerState == store.PowerStopped {
			continue
		}
		sc := sched[m.ID]
		if sc == nil {
			sc = &reachSched{next: now} // check newly-seen hosts promptly
			sched[m.ID] = sc
		}
		if now.Before(sc.next) {
			continue
		}
		wg.Add(1)
		go func(m store.Machine, sc *reachSched) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.reachCheck(ctx, m, sc)
		}(m, sc)
	}
	wg.Wait()

	// Forget machines that no longer exist so the map can't grow without bound.
	for id := range sched {
		if !seen[id] {
			delete(sched, id)
		}
	}
}

func (s *Server) reachCheck(ctx context.Context, m store.Machine, sc *reachSched) {
	target, err := s.resolveTarget(m)
	if err != nil {
		s.scheduleNext(sc, false)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, err = s.broker.Exec(callCtx, target, "sh -s", sshExit0())
	if err != nil {
		_ = s.store.SetReach(m.ID, classifyReach(err), err.Error())
		s.broker.Drop(m.ID)
		s.scheduleNext(sc, false)
		return
	}
	_ = s.store.SetReach(m.ID, store.ReachOK, "")
	s.scheduleNext(sc, true)
}

// scheduleNext sets the next check time: steady when healthy, exponential
// backoff (capped) while failing.
func (s *Server) scheduleNext(sc *reachSched, ok bool) {
	if ok {
		sc.fails = 0
		sc.next = time.Now().Add(reachHealthy)
		return
	}
	sc.fails++
	d := reachHealthy << sc.fails // 120s, 240s, 480s, …
	if d > reachBackoff || d <= 0 {
		d = reachBackoff
	}
	sc.next = time.Now().Add(d)
}
