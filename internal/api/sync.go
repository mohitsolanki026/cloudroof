package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloudroof/internal/provider"
	"cloudroof/internal/store"
)

type syncResult struct {
	Total   int `json:"total"`
	Created int `json:"created"`
	Updated int `json:"updated"`
}

// providerFor unseals an account's token and constructs its adapter.
func (s *Server) providerFor(accountID int64) (provider.Provider, error) {
	a, err := s.store.GetCloudAccount(accountID)
	if err != nil {
		return nil, err
	}
	plain, err := s.keys.OpenSealed(a.SealedToken)
	if err != nil {
		return nil, err
	}
	var c provider.Credentials
	if json.Unmarshal(plain, &c) != nil || c.Empty() {
		// v0.1 sealed the bare token string rather than a JSON object.
		c = provider.Credentials{Token: strings.TrimSpace(string(plain))}
	}
	return provider.New(a.Provider, c)
}

// sync pulls every instance from one account into the fleet. It is additive:
// new instances become machines, known ones get their cloud half refreshed,
// and ones that vanished are flagged missing rather than deleted.
func (s *Server) sync(ctx context.Context, accountID int64) (syncResult, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	a, err := s.store.GetCloudAccount(accountID)
	if err != nil {
		return syncResult{}, err
	}
	p, err := s.providerFor(accountID)
	if err != nil {
		return syncResult{}, err
	}

	instances, err := p.ListInstances(ctx)
	if err != nil {
		_ = s.store.SetSyncResult(accountID, time.Now(), err.Error())
		return syncResult{}, fmt.Errorf("sync %s: %w", a.Name, err)
	}

	var res syncResult
	seen := make([]string, 0, len(instances))
	for _, in := range instances {
		id, created, hostChanged, err := s.store.UpsertFromCloud(accountID, a.Provider, in)
		if err != nil {
			_ = s.store.SetSyncResult(accountID, time.Now(), err.Error())
			return res, err
		}
		if created {
			res.Created++
		} else {
			res.Updated++
		}
		if hostChanged {
			// The instance's public IP moved (a stop/start on an ephemeral
			// address) and ssh_host followed it. The pooled connection points
			// at the old IP; drop it so the next action redials, and reset
			// reachability so the fleet reflects "unknown until re-probed"
			// rather than a stale "ok".
			s.broker.Drop(id)
			_ = s.store.SetReach(id, store.ReachUnknown, "")
			s.log.Info("machine public IP changed; ssh_host followed", "machine", id, "ip", in.PublicIP)
		}
		seen = append(seen, in.InstanceID)
	}
	res.Total = len(instances)

	if err := s.store.MarkMissing(accountID, seen); err != nil {
		return res, err
	}
	_ = s.store.SetSyncResult(accountID, time.Now(), "")
	s.log.Info("synced cloud account", "account", a.Name, "provider", a.Provider,
		"total", res.Total, "created", res.Created)
	return res, nil
}

// refreshPowerStates re-reads the provider state for every linked machine.
// Cheap enough to run on a timer; it is what keeps the first status dot honest.
func (s *Server) refreshPowerStates(ctx context.Context) {
	accounts, err := s.store.ListCloudAccounts()
	if err != nil {
		return
	}
	for _, a := range accounts {
		// Bounded per account so one stalled provider cannot wedge the
		// ticker for everyone else.
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := s.sync(callCtx, a.ID)
		cancel()
		if err != nil {
			s.log.Warn("power refresh failed", "account", a.Name, "err", err)
		}
	}
}

// StartBackground launches the periodic power-state refresh. It stops when ctx
// is cancelled.
func (s *Server) StartBackground(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshPowerStates(ctx)
			}
		}
	}()
}
