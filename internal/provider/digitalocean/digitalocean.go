// Package digitalocean adapts DigitalOcean Droplets to the provider interface.
//
// Same shape as Hetzner — one bearer token, flat instance list — which is
// exactly why it is second: it confirms the abstraction holds for a second
// token-style API before AWS bends it.
package digitalocean

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/oauth2"

	"bosun/internal/provider"
	"bosun/internal/store"
)

const Name = "digitalocean"

func init() {
	provider.Register(provider.Spec{
		Name:  Name,
		Label: "DigitalOcean",
		Fields: []provider.Field{
			{Name: "token", Label: "Personal access token", Kind: provider.FieldSecret},
		},
		Notes: "API → Tokens → Generate New Token. Scopes: droplet:read for inventory; droplet:update for power control (actions).",
	}, func(c provider.Credentials) (provider.Provider, error) {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Token})
		hc := oauth2.NewClient(context.Background(), ts)
		hc.Timeout = 45 * time.Second
		client := godo.NewClient(hc)
		client.UserAgent = "bosun/1.0"
		return &DO{client: client}, nil
	})
}

type DO struct {
	client *godo.Client
}

func (d *DO) Name() string { return Name }

func (d *DO) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	out := []store.CloudInstance{}
	for {
		droplets, resp, err := d.client.Droplets.List(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("digitalocean: list droplets: %w", err)
		}
		for _, dr := range droplets {
			out = append(out, toInstance(dr))
		}
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			break
		}
		opt.Page = page + 1
	}
	return out, nil
}

func (d *DO) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	id, err := parseID(instanceID)
	if err != nil {
		return store.PowerUnknown, err
	}
	dr, _, err := d.client.Droplets.Get(ctx, id)
	if err != nil {
		return store.PowerUnknown, fmt.Errorf("digitalocean: get droplet %d: %w", id, err)
	}
	if dr == nil {
		return store.PowerUnknown, fmt.Errorf("digitalocean: droplet %d not found", id)
	}
	return mapStatus(dr.Status), nil
}

func (d *DO) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	id, err := parseID(instanceID)
	if err != nil {
		return err
	}
	switch action {
	case provider.PowerStart:
		_, _, err = d.client.DropletActions.PowerOn(ctx, id)
	case provider.PowerStop:
		_, _, err = d.client.DropletActions.Shutdown(ctx, id)
	case provider.PowerReboot:
		_, _, err = d.client.DropletActions.Reboot(ctx, id)
	case provider.PowerForceStop:
		_, _, err = d.client.DropletActions.PowerOff(ctx, id)
	case provider.PowerForceReboot:
		_, _, err = d.client.DropletActions.PowerCycle(ctx, id)
	default:
		return provider.ErrUnsupportedAction
	}
	if err != nil {
		return fmt.Errorf("digitalocean: %s droplet %d: %w", action, id, err)
	}
	// A 201 with an "in-progress" action object is DigitalOcean's
	// acknowledgement; the droplet's status catches up on the next sync.
	return nil
}

func toInstance(dr godo.Droplet) store.CloudInstance {
	in := store.CloudInstance{
		InstanceID:   strconv.Itoa(dr.ID),
		Name:         dr.Name,
		InstanceType: dr.SizeSlug,
		PowerState:   mapStatus(dr.Status),
		Labels:       append([]string{}, dr.Tags...),
	}
	if dr.Region != nil {
		in.Region = dr.Region.Slug
	}
	if dr.Networks != nil {
		for _, v4 := range dr.Networks.V4 {
			switch v4.Type {
			case "public":
				if in.PublicIP == "" {
					in.PublicIP = v4.IPAddress
				}
			case "private":
				if in.PrivateIP == "" {
					in.PrivateIP = v4.IPAddress
				}
			}
		}
	}
	sort.Strings(in.Labels)
	return in
}

// mapStatus: DigitalOcean reports active | off | new | archive.
func mapStatus(s string) store.PowerState {
	switch s {
	case "active":
		return store.PowerRunning
	case "off":
		return store.PowerStopped
	case "new":
		return store.PowerPending
	default:
		return store.PowerUnknown
	}
}

func parseID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("digitalocean: invalid droplet id %q", s)
	}
	return id, nil
}
