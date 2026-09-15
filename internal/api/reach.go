// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package api

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mohitsolanki026/cloudroof/internal/sshx"
	"github.com/mohitsolanki026/cloudroof/internal/store"
)

// sshExit0 is the cheapest possible liveness command: read the script, exit 0.
// The timeout is chosen per call (tight for a warm pooled connection, generous
// for a cold dial) and the output cap is tiny so a slow host fails fast.
func sshExit0(timeout time.Duration) sshx.ExecOpts {
	return sshx.ExecOpts{
		Stdin:     strings.NewReader("exit 0\n"),
		Timeout:   timeout,
		MaxOutput: 256,
	}
}

// The reachability poller keeps the second status dot honest without the user
// clicking Probe. It is deliberately cheap: a trivial `exit 0` over the pooled
// SSH connection, not a full fingerprint. Healthy hosts are re-checked at a
// steady interval; failing ones back off so a dead box is not hammered.
//
// A fixed pool of workers pulls due machines off a queue, so one slow or
// unreachable host occupies a single worker for the length of its timeout
// rather than stalling the whole sweep — a dispatcher only ever enqueues work,
// it never blocks on an SSH call. That, plus a probe timeout matched to whether
// the connection is already warm, is what lets the fleet scale past a couple of
// dozen machines.
const (
	reachHealthy     = 60 * time.Second // steady re-check of a healthy host
	reachBackoff     = 15 * time.Minute // cap for a persistently-down host
	reachTick        = 5 * time.Second  // how often the dispatcher enqueues what's due
	reachFanout      = 4                // worker goroutines draining the queue
	reachQueue       = 256              // buffered due-machine handoff
	reachWarmTimeout = 5 * time.Second  // a live pooled connection should answer at once
	reachColdTimeout = 15 * time.Second // a cold check must cover dial + handshake
)

// settingReachEnabled is the key under which the auto-probe toggle persists.
const settingReachEnabled = "reach.enabled"

type reachSched struct {
	next  time.Time
	fails int
}

// reachPoller owns the background reachability loop: its per-machine schedule,
// the in-flight set that keeps a machine from being enqueued twice, and the
// enabled flag the dashboard toggles.
type reachPoller struct {
	srv     *Server
	enabled atomic.Bool
	queue   chan store.Machine

	mu       sync.Mutex
	sched    map[int64]*reachSched
	inflight map[int64]bool
}

// Enabled reports whether background probing is currently running.
func (p *reachPoller) Enabled() bool { return p.enabled.Load() }

// SetEnabled turns background probing on or off live. Turning it off lets any
// in-flight checks finish and leaves the last known reach state untouched; the
// user's manual Probe button still works either way.
func (p *reachPoller) SetEnabled(v bool) { p.enabled.Store(v) }

// StartReachPoller starts the worker pool and dispatcher, running until ctx is
// cancelled. The enabled state is restored from settings (default on).
func (s *Server) StartReachPoller(ctx context.Context) {
	p := &reachPoller{
		srv:      s,
		queue:    make(chan store.Machine, reachQueue),
		sched:    map[int64]*reachSched{},
		inflight: map[int64]bool{},
	}
	p.enabled.Store(true)
	if v, err := s.store.GetSetting(settingReachEnabled); err == nil {
		p.enabled.Store(v != "0" && v != "false")
	}
	s.reach = p

	for i := 0; i < reachFanout; i++ {
		go p.worker(ctx)
	}
	go p.dispatch(ctx)
}

// worker pulls one due machine at a time, probes it, and records the result
// and the next check time. Because workers are independent, a probe that sits
// on its timeout only holds this one worker; the others keep draining healthy
// machines.
func (p *reachPoller) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-p.queue:
			ok := p.srv.reachProbe(ctx, m)
			p.mu.Lock()
			if sc := p.sched[m.ID]; sc != nil {
				scheduleNext(sc, ok)
			}
			delete(p.inflight, m.ID)
			p.mu.Unlock()
		}
	}
}

// dispatch wakes on a short tick and hands every due machine to the workers. It
// never blocks on an SSH call, so a slow fleet cannot delay scheduling.
func (p *reachPoller) dispatch(ctx context.Context) {
	t := time.NewTicker(reachTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.enqueueDue()
		}
	}
}

func (p *reachPoller) enqueueDue() {
	if !p.enabled.Load() {
		return
	}
	machines, err := p.srv.store.ListMachines()
	if err != nil {
		return
	}
	now := time.Now()

	p.mu.Lock()
	seen := map[int64]bool{}
	var due []store.Machine
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
		sc := p.sched[m.ID]
		if sc == nil {
			sc = &reachSched{next: now} // check newly-seen hosts promptly
			p.sched[m.ID] = sc
		}
		if p.inflight[m.ID] || now.Before(sc.next) {
			continue
		}
		p.inflight[m.ID] = true // reserve before releasing the lock
		due = append(due, m)
	}
	// Forget machines that no longer exist so the maps can't grow without bound.
	for id := range p.sched {
		if !seen[id] {
			delete(p.sched, id)
			delete(p.inflight, id)
		}
	}
	p.mu.Unlock()

	for _, m := range due {
		select {
		case p.queue <- m:
		default:
			// Workers are saturated; drop the reservation so a later tick
			// retries this machine rather than skipping it forever.
			p.mu.Lock()
			delete(p.inflight, m.ID)
			p.mu.Unlock()
		}
	}
}

// reachProbe runs one liveness check and records the outcome, returning whether
// the host was reachable. The timeout is tight when the connection is already
// pooled and generous when a cold dial + handshake is needed.
func (s *Server) reachProbe(ctx context.Context, m store.Machine) bool {
	target, err := s.resolveTarget(m)
	if err != nil {
		return false
	}
	timeout := reachColdTimeout
	if s.broker.IsConnected(m.ID) {
		timeout = reachWarmTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := s.broker.Exec(callCtx, target, "sh -s", sshExit0(timeout)); err != nil {
		_ = s.store.SetReach(m.ID, classifyReach(err), err.Error())
		s.broker.Drop(m.ID)
		return false
	}
	_ = s.store.SetReach(m.ID, store.ReachOK, "")
	return true
}

// scheduleNext sets the next check time: steady when healthy, exponential
// backoff (capped) while failing. Called under the poller mutex.
func scheduleNext(sc *reachSched, ok bool) {
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
