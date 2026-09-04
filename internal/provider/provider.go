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

	"bosun/internal/store"
)

// PowerAction is the normalized set of power operations. Every supported
// provider maps all five cleanly:
//
//	Hetzner: poweron / shutdown / reboot / poweroff / reset
//	DO:      power_on / shutdown / reboot / power_off / power_cycle
//	AWS:     Start / Stop / Reboot / Stop(Force) / — (Reboot is already hard)
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
	// ListInstances returns every instance visible to the credential.
	ListInstances(ctx context.Context) ([]store.CloudInstance, error)
	// GetPowerState fetches the current state of one instance.
	GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error)
	// Power performs an action and returns once the provider has accepted it.
	// It does not wait for the instance to reach the target state; callers
	// poll GetPowerState for that.
	Power(ctx context.Context, instanceID string, action PowerAction) error
}

// Factory builds a Provider from a plaintext credential.
type Factory func(token string) (Provider, error)

var registry = map[string]Factory{}

// Register adds a provider to the registry. Adapters call this from init().
func Register(name string, f Factory) { registry[name] = f }

// New constructs the named provider.
func New(name, token string) (Provider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	return f(token)
}

// Names lists registered providers, for the UI's account-type picker.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}
