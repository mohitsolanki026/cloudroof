// Package hetzner adapts Hetzner Cloud to the provider interface.
//
// Hetzner is the first adapter because its API is the simplest of the
// supported set (one bearer token, no regions to enumerate) — it proves the
// interface before AWS stress-tests it.
package hetzner

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"bosun/internal/provider"
	"bosun/internal/store"
)

const Name = "hetzner"

func init() {
	provider.Register(Name, func(token string) (provider.Provider, error) {
		if token == "" {
			return nil, fmt.Errorf("hetzner: empty API token")
		}
		return &Hetzner{
			client: hcloud.NewClient(
				hcloud.WithToken(token),
				hcloud.WithApplication("bosun", "0.1"),
				// Callers pass contexts, but a client-level ceiling guards
				// the one path (hcloud's own retries) that outlives them.
				hcloud.WithHTTPClient(&http.Client{Timeout: 45 * time.Second}),
			),
		}, nil
	})
}

type Hetzner struct {
	client *hcloud.Client
}

func (h *Hetzner) Name() string { return Name }

func (h *Hetzner) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	servers, err := h.client.Server.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("hetzner: list servers: %w", err)
	}

	out := make([]store.CloudInstance, 0, len(servers))
	for _, s := range servers {
		out = append(out, toInstance(s))
	}
	return out, nil
}

func (h *Hetzner) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	id, err := parseID(instanceID)
	if err != nil {
		return store.PowerUnknown, err
	}
	s, _, err := h.client.Server.GetByID(ctx, id)
	if err != nil {
		return store.PowerUnknown, fmt.Errorf("hetzner: get server %d: %w", id, err)
	}
	if s == nil {
		return store.PowerUnknown, fmt.Errorf("hetzner: server %d not found", id)
	}
	return mapStatus(s.Status), nil
}

func (h *Hetzner) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	id, err := parseID(instanceID)
	if err != nil {
		return err
	}
	srv := &hcloud.Server{ID: id}

	var act *hcloud.Action
	switch action {
	case provider.PowerStart:
		act, _, err = h.client.Server.Poweron(ctx, srv)
	case provider.PowerStop:
		act, _, err = h.client.Server.Shutdown(ctx, srv)
	case provider.PowerReboot:
		act, _, err = h.client.Server.Reboot(ctx, srv)
	case provider.PowerForceStop:
		act, _, err = h.client.Server.Poweroff(ctx, srv)
	case provider.PowerForceReboot:
		act, _, err = h.client.Server.Reset(ctx, srv)
	default:
		return provider.ErrUnsupportedAction
	}
	if err != nil {
		return fmt.Errorf("hetzner: %s server %d: %w", action, id, err)
	}
	// Wait for Hetzner to acknowledge the action itself (not for the server
	// to reach its target state). Acknowledgement is fast and catches
	// "server is locked" style refusals synchronously.
	if act != nil {
		if err := h.client.Action.WaitFor(ctx, act); err != nil {
			return fmt.Errorf("hetzner: %s server %d: %w", action, id, err)
		}
	}
	return nil
}

func toInstance(s *hcloud.Server) store.CloudInstance {
	in := store.CloudInstance{
		InstanceID: strconv.FormatInt(s.ID, 10),
		Name:       s.Name,
		PowerState: mapStatus(s.Status),
	}
	if s.ServerType != nil {
		in.InstanceType = s.ServerType.Name
	}
	if s.Location != nil {
		in.Region = s.Location.Name
	}
	if s.PublicNet.IPv4.IP != nil {
		in.PublicIP = s.PublicNet.IPv4.IP.String()
	}
	if len(s.PrivateNet) > 0 && s.PrivateNet[0].IP != nil {
		in.PrivateIP = s.PrivateNet[0].IP.String()
	}
	// Hetzner labels are key=value; flatten to "key:value" tags, or just the
	// key when the value is empty (Hetzner permits that).
	for k, v := range s.Labels {
		if v == "" {
			in.Labels = append(in.Labels, k)
		} else {
			in.Labels = append(in.Labels, k+":"+v)
		}
	}
	sort.Strings(in.Labels)
	return in
}

func mapStatus(s hcloud.ServerStatus) store.PowerState {
	switch s {
	case hcloud.ServerStatusRunning:
		return store.PowerRunning
	case hcloud.ServerStatusOff:
		return store.PowerStopped
	case hcloud.ServerStatusStarting, hcloud.ServerStatusStopping,
		hcloud.ServerStatusInitializing, hcloud.ServerStatusRebuilding,
		hcloud.ServerStatusMigrating:
		return store.PowerPending
	default:
		return store.PowerUnknown
	}
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("hetzner: invalid instance id %q", s)
	}
	return id, nil
}
