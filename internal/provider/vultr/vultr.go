// Package vultr adapts Vultr to the provider interface.
//
// Vultr's v2 API is a simple bearer-token REST surface, so this talks to it
// directly with net/http rather than pull in an SDK — which keeps it in the
// slim build tier alongside Hetzner and DigitalOcean.
package vultr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"bosun/internal/provider"
	"bosun/internal/store"
)

const Name = "vultr"

const base = "https://api.vultr.com/v2"

func init() {
	provider.Register(provider.Spec{
		Name:  Name,
		Label: "Vultr",
		Fields: []provider.Field{
			{Name: "token", Label: "API key", Kind: provider.FieldSecret},
		},
		Notes: "Account → API → enable the API and copy the personal access token. Read access lists instances; the token needs write for power control. Restrict the API to your IP if you can.",
	}, func(c provider.Credentials) (provider.Provider, error) {
		if c.Token == "" {
			return nil, fmt.Errorf("vultr: empty API key")
		}
		return &Vultr{token: c.Token, http: &http.Client{Timeout: 45 * time.Second}}, nil
	})
}

type Vultr struct {
	token string
	http  *http.Client
}

func (v *Vultr) Name() string { return Name }

// instance is the subset of Vultr's instance object we use.
type instance struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	MainIP      string   `json:"main_ip"`
	InternalIP  string   `json:"internal_ip"`
	Region      string   `json:"region"`
	Plan        string   `json:"plan"`
	Status      string   `json:"status"`       // active | pending | …
	PowerStatus string   `json:"power_status"` // running | stopped
	Tags        []string `json:"tags"`
}

func (v *Vultr) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+v.token)
	resp, err := v.http.Do(req)
	if err != nil {
		return fmt.Errorf("vultr: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body bytes.Buffer
		body.ReadFrom(resp.Body)
		return fmt.Errorf("vultr: %s %s: %s: %s", method, path, resp.Status, body.String())
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (v *Vultr) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	out := []store.CloudInstance{}
	cursor := ""
	for {
		var page struct {
			Instances []instance `json:"instances"`
			Meta      struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		path := "/instances?per_page=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := v.do(ctx, http.MethodGet, path, &page); err != nil {
			return nil, err
		}
		for _, in := range page.Instances {
			out = append(out, toInstance(in))
		}
		if page.Meta.Links.Next == "" {
			break
		}
		cursor = page.Meta.Links.Next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (v *Vultr) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	var resp struct {
		Instance instance `json:"instance"`
	}
	if err := v.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(instanceID), &resp); err != nil {
		return store.PowerUnknown, err
	}
	return mapStatus(resp.Instance), nil
}

func (v *Vultr) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	id := url.PathEscape(instanceID)
	var path string
	switch action {
	case provider.PowerStart:
		path = "/instances/" + id + "/start"
	case provider.PowerStop, provider.PowerForceStop:
		// Vultr has one power-off (halt); there is no separate hard stop.
		path = "/instances/" + id + "/halt"
	case provider.PowerReboot, provider.PowerForceReboot:
		path = "/instances/" + id + "/reboot"
	default:
		return provider.ErrUnsupportedAction
	}
	return v.do(ctx, http.MethodPost, path, nil)
}

func toInstance(in instance) store.CloudInstance {
	ci := store.CloudInstance{
		InstanceID:   in.ID,
		Name:         in.Label,
		Region:       in.Region,
		InstanceType: in.Plan,
		PrivateIP:    in.InternalIP,
		PowerState:   mapStatus(in),
		Labels:       append([]string{}, in.Tags...),
	}
	if in.Label == "" {
		ci.Name = in.ID
	}
	// Vultr reports 0.0.0.0 before an IP is assigned.
	if in.MainIP != "" && in.MainIP != "0.0.0.0" {
		ci.PublicIP = in.MainIP
	}
	sort.Strings(ci.Labels)
	return ci
}

// mapStatus prefers power_status (running|stopped); a still-provisioning
// instance (status != active) is pending.
func mapStatus(in instance) store.PowerState {
	if in.Status != "" && in.Status != "active" {
		return store.PowerPending
	}
	switch in.PowerStatus {
	case "running":
		return store.PowerRunning
	case "stopped":
		return store.PowerStopped
	default:
		return store.PowerUnknown
	}
}
