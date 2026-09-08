// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package provider defines the narrow adapter interface every cloud must
// implement, and a registry to construct one by name.
//
// The interface is deliberately small: list, describe, power. Provisioning,
// billing, and networking are explicitly out of scope — the differences
// between providers there are large enough that a shared abstraction would be
// a lie.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloudroof/internal/store"
)

// PowerAction is the normalized set of power operations. Every supported
// provider maps all five:
//
//	Hetzner: poweron / shutdown / reboot / poweroff / reset
//	DO:      power_on / shutdown / reboot / power_off / power_cycle
//	AWS:     Start / Stop / Reboot / Stop(Force) / Reboot (EC2 escalates to a
//	         hard reboot itself when the guest ignores the ACPI request)
type PowerAction string

const (
	PowerStart       PowerAction = "start"
	PowerStop        PowerAction = "stop"         // graceful ACPI shutdown
	PowerReboot      PowerAction = "reboot"       // graceful ACPI reboot
	PowerForceStop   PowerAction = "force_stop"   // pull the plug
	PowerForceReboot PowerAction = "force_reboot" // power cycle; for hung machines
)

// Danger tier of each power action, mirroring the action catalog. Power
// actions are always at least Tier 2 because they cause downtime.
func (a PowerAction) Danger() int {
	switch a {
	case PowerStart:
		return 1
	default:
		return 2
	}
}

func (a PowerAction) Valid() bool {
	switch a {
	case PowerStart, PowerStop, PowerReboot, PowerForceStop, PowerForceReboot:
		return true
	}
	return false
}

var ErrUnsupportedAction = errors.New("provider: action not supported")
var ErrUnknownProvider = errors.New("provider: unknown provider")

// Provider is one cloud account's worth of access.
type Provider interface {
	// Name is the registry key, e.g. "hetzner".
	Name() string
	// ListInstances returns every instance visible to the credential. It
	// must return an error rather than a partial list: the caller marks
	// anything absent from the list as missing.
	ListInstances(ctx context.Context) ([]store.CloudInstance, error)
	// GetPowerState fetches the current state of one instance.
	GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error)
	// Power performs an action and returns once the provider has accepted it.
	// It does not wait for the instance to reach the target state; callers
	// poll GetPowerState for that.
	Power(ctx context.Context, instanceID string, action PowerAction) error
}

// Credentials is the plaintext secret for one account, sealed as JSON at
// rest. Which fields a provider reads is declared by its Spec, so the UI can
// render the right form and the handler can validate before sealing.
type Credentials struct {
	// Token-style (Hetzner, DigitalOcean).
	Token string `json:"token,omitempty"`
	// Key pair (AWS).
	AccessKeyID     string   `json:"accessKeyId,omitempty"`
	SecretAccessKey string   `json:"secretAccessKey,omitempty"`
	Regions         []string `json:"regions,omitempty"`
	// Service principal (Azure).
	SubscriptionID string `json:"subscriptionId,omitempty"`
	TenantID       string `json:"tenantId,omitempty"`
	ClientID       string `json:"clientId,omitempty"`
	ClientSecret   string `json:"clientSecret,omitempty"`
	// Service account (GCP): the key JSON, and the projects to scan (empty
	// falls back to the project_id inside the key).
	ServiceAccountJSON string   `json:"serviceAccountJson,omitempty"`
	Projects           []string `json:"projects,omitempty"`
}

// Get returns a field by its JSON name, for spec-driven validation.
func (c Credentials) Get(name string) string {
	switch name {
	case "token":
		return c.Token
	case "accessKeyId":
		return c.AccessKeyID
	case "secretAccessKey":
		return c.SecretAccessKey
	case "regions":
		return strings.Join(c.Regions, ",")
	case "subscriptionId":
		return c.SubscriptionID
	case "tenantId":
		return c.TenantID
	case "clientId":
		return c.ClientID
	case "clientSecret":
		return c.ClientSecret
	case "serviceAccountJson":
		return c.ServiceAccountJSON
	case "projects":
		return strings.Join(c.Projects, ",")
	}
	return ""
}

// Empty reports whether no field is set.
func (c Credentials) Empty() bool {
	return c.Token == "" && c.AccessKeyID == "" && c.SecretAccessKey == "" && len(c.Regions) == 0 &&
		c.SubscriptionID == "" && c.TenantID == "" && c.ClientID == "" && c.ClientSecret == "" &&
		c.ServiceAccountJSON == "" && len(c.Projects) == 0
}

type FieldKind string

const (
	FieldText     FieldKind = "text"
	FieldSecret   FieldKind = "secret"
	FieldList     FieldKind = "list"     // comma-separated in the UI, []string in Credentials
	FieldTextarea FieldKind = "textarea" // multi-line, e.g. a pasted JSON key
)

// Field describes one credential input the UI renders.
type Field struct {
	Name     string    `json:"name"` // JSON key in Credentials
	Label    string    `json:"label"`
	Kind     FieldKind `json:"kind"`
	Optional bool      `json:"optional,omitempty"`
	Hint     string    `json:"hint,omitempty"`
}

// Spec is what the UI needs to render an account form for a provider.
type Spec struct {
	Name   string  `json:"name"`
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
	// Notes is shown under the form: where to get the credential and the
	// minimum permissions it needs.
	Notes string `json:"notes,omitempty"`
}

// Validate checks that every required field is present.
func (s Spec) Validate(c Credentials) error {
	for _, f := range s.Fields {
		if !f.Optional && strings.TrimSpace(c.Get(f.Name)) == "" {
			return fmt.Errorf("%s is required", f.Label)
		}
	}
	return nil
}

// Factory builds a Provider from validated plaintext credentials.
type Factory func(c Credentials) (Provider, error)

type entry struct {
	spec    Spec
	factory Factory
}

var registry = map[string]entry{}

// Register adds a provider to the registry. Adapters call this from init().
func Register(spec Spec, f Factory) { registry[spec.Name] = entry{spec: spec, factory: f} }

// New validates the credentials against the provider's spec and constructs it.
func New(name string, c Credentials) (Provider, error) {
	e, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	if err := e.spec.Validate(c); err != nil {
		return nil, fmt.Errorf("%s: %w", e.spec.Label, err)
	}
	return e.factory(c)
}

// Lookup returns a provider's spec.
func Lookup(name string) (Spec, bool) {
	e, ok := registry[name]
	return e.spec, ok
}

// Specs lists every registered provider, sorted by name, for the UI.
func Specs() []Spec {
	out := make([]Spec, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names lists registered provider names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
