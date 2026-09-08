// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package google adapts Google Compute Engine to the provider interface.
//
// Credentials are a service-account key JSON. Instances live in zones within
// projects, so the instance ID is "project/zone/name" and the interface stays
// project- and zone-agnostic. AggregatedList sweeps every zone in one call.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"cloudroof/internal/provider"
	"cloudroof/internal/store"
)

const Name = "gcp"

func init() {
	provider.Register(provider.Spec{
		Name:  Name,
		Label: "Google Cloud",
		Fields: []provider.Field{
			{Name: "serviceAccountJson", Label: "Service account key (JSON)", Kind: provider.FieldTextarea},
			{Name: "projects", Label: "Projects", Kind: provider.FieldList, Optional: true,
				Hint: "Comma-separated project IDs. Leave empty to use the project the key belongs to."},
		},
		Notes: "IAM → Service Accounts → Keys → add JSON key. Grant the Compute Viewer role for inventory, and Compute Instance Admin (v1) for start/stop.",
	}, func(c provider.Credentials) (provider.Provider, error) {
		key := strings.TrimSpace(c.ServiceAccountJSON)
		if !json.Valid([]byte(key)) {
			return nil, fmt.Errorf("gcp: service account key is not valid JSON")
		}
		projects := trimAll(c.Projects)
		if len(projects) == 0 {
			// Fall back to the project the key was issued for.
			var meta struct {
				ProjectID string `json:"project_id"`
			}
			_ = json.Unmarshal([]byte(key), &meta)
			if meta.ProjectID == "" {
				return nil, fmt.Errorf("gcp: no projects given and the key has no project_id")
			}
			projects = []string{meta.ProjectID}
		}
		return &GCE{key: []byte(key), projects: projects}, nil
	})
}

type GCE struct {
	key      []byte
	projects []string
}

func (g *GCE) Name() string { return Name }

func (g *GCE) service(ctx context.Context) (*compute.Service, error) {
	svc, err := compute.NewService(ctx, option.WithCredentialsJSON(g.key))
	if err != nil {
		return nil, fmt.Errorf("gcp: build client: %w", err)
	}
	return svc, nil
}

func (g *GCE) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	svc, err := g.service(ctx)
	if err != nil {
		return nil, err
	}
	out := []store.CloudInstance{}
	for _, project := range g.projects {
		err := svc.Instances.AggregatedList(project).Pages(ctx, func(page *compute.InstanceAggregatedList) error {
			for _, scoped := range page.Items {
				for _, in := range scoped.Instances {
					out = append(out, toInstance(project, in))
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("gcp: list instances in %s: %w", project, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (g *GCE) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	project, zone, name, err := split(instanceID)
	if err != nil {
		return store.PowerUnknown, err
	}
	svc, err := g.service(ctx)
	if err != nil {
		return store.PowerUnknown, err
	}
	in, err := svc.Instances.Get(project, zone, name).Context(ctx).Do()
	if err != nil {
		return store.PowerUnknown, fmt.Errorf("gcp: get %s: %w", instanceID, err)
	}
	return mapStatus(in.Status), nil
}

func (g *GCE) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	project, zone, name, err := split(instanceID)
	if err != nil {
		return err
	}
	svc, err := g.service(ctx)
	if err != nil {
		return err
	}
	// GCE has start, stop (ACPI), and reset (hard). There is no soft reboot,
	// so both reboot variants map to reset; both stop variants to stop.
	switch action {
	case provider.PowerStart:
		_, err = svc.Instances.Start(project, zone, name).Context(ctx).Do()
	case provider.PowerStop, provider.PowerForceStop:
		_, err = svc.Instances.Stop(project, zone, name).Context(ctx).Do()
	case provider.PowerReboot, provider.PowerForceReboot:
		_, err = svc.Instances.Reset(project, zone, name).Context(ctx).Do()
	default:
		return provider.ErrUnsupportedAction
	}
	if err != nil {
		return fmt.Errorf("gcp: %s %s: %w", action, instanceID, err)
	}
	return nil
}

func toInstance(project string, in *compute.Instance) store.CloudInstance {
	ci := store.CloudInstance{
		InstanceID:   project + "/" + lastSeg(in.Zone) + "/" + in.Name,
		Name:         in.Name,
		Region:       lastSeg(in.Zone),
		InstanceType: lastSeg(in.MachineType),
		PowerState:   mapStatus(in.Status),
	}
	for _, ni := range in.NetworkInterfaces {
		if ci.PrivateIP == "" {
			ci.PrivateIP = ni.NetworkIP
		}
		for _, ac := range ni.AccessConfigs {
			if ac.NatIP != "" && ci.PublicIP == "" {
				ci.PublicIP = ac.NatIP
			}
		}
	}
	for k, v := range in.Labels {
		if v == "" {
			ci.Labels = append(ci.Labels, k)
		} else {
			ci.Labels = append(ci.Labels, k+":"+v)
		}
	}
	sort.Strings(ci.Labels)
	return ci
}

// mapStatus: GCE reports PROVISIONING, STAGING, RUNNING, STOPPING, STOPPED,
// TERMINATED, SUSPENDING, SUSPENDED, REPAIRING.
func mapStatus(s string) store.PowerState {
	switch s {
	case "RUNNING":
		return store.PowerRunning
	case "TERMINATED", "STOPPED", "SUSPENDED":
		return store.PowerStopped
	case "PROVISIONING", "STAGING", "STOPPING", "SUSPENDING", "REPAIRING":
		return store.PowerPending
	default:
		return store.PowerUnknown
	}
}

// split takes "project/zone/name" apart. Names may not contain "/", and
// project/zone never do, so two splits are unambiguous.
func split(instanceID string) (project, zone, name string, err error) {
	parts := strings.SplitN(instanceID, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("gcp: invalid instance id %q (want project/zone/name)", instanceID)
	}
	return parts[0], parts[1], parts[2], nil
}

// lastSeg returns the final path segment of a GCE self-link or type URL, e.g.
// ".../zones/us-central1-a" -> "us-central1-a".
func lastSeg(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
