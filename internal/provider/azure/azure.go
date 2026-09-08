// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package azure adapts Azure Virtual Machines to the provider interface.
//
// Auth is a service principal (tenant + client id/secret) scoped to one
// subscription. The instance ID is the full ARM resource ID, from which the
// resource group and VM name are parsed for power operations.
//
// The Compute API returns power state (via StatusOnly) but not IP addresses,
// so IPs are enriched best-effort from the Network API: with only Compute
// permissions the fleet still lists and powers, just without a pre-filled
// ssh_host. Add the Reader role on network resources to get IPs.
package azure

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/mohitsolanki026/cloudroof/internal/provider"
	"github.com/mohitsolanki026/cloudroof/internal/store"
)

const Name = "azure"

func init() {
	provider.Register(provider.Spec{
		Name:  Name,
		Label: "Azure",
		Fields: []provider.Field{
			{Name: "subscriptionId", Label: "Subscription ID", Kind: provider.FieldText},
			{Name: "tenantId", Label: "Tenant ID", Kind: provider.FieldText},
			{Name: "clientId", Label: "Client ID", Kind: provider.FieldText},
			{Name: "clientSecret", Label: "Client secret", Kind: provider.FieldSecret},
		},
		Notes: "Create an app registration (service principal) and grant it a role on the subscription: Reader for inventory + IPs, Virtual Machine Contributor for start/stop.",
	}, func(c provider.Credentials) (provider.Provider, error) {
		if c.SubscriptionID == "" {
			return nil, fmt.Errorf("azure: subscription ID is required")
		}
		cred, err := azidentity.NewClientSecretCredential(c.TenantID, c.ClientID, c.ClientSecret, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: credential: %w", err)
		}
		vmClient, err := armcompute.NewVirtualMachinesClient(c.SubscriptionID, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: compute client: %w", err)
		}
		nicClient, err := armnetwork.NewInterfacesClient(c.SubscriptionID, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: network client: %w", err)
		}
		pipClient, err := armnetwork.NewPublicIPAddressesClient(c.SubscriptionID, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: public-ip client: %w", err)
		}
		return &Azure{vm: vmClient, nic: nicClient, pip: pipClient}, nil
	})
}

type Azure struct {
	vm  *armcompute.VirtualMachinesClient
	nic *armnetwork.InterfacesClient
	pip *armnetwork.PublicIPAddressesClient
}

func (a *Azure) Name() string { return Name }

func (a *Azure) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	// StatusOnly makes ListAll return each VM's instance view (power state) in
	// the same sweep rather than one Get per VM.
	pager := a.vm.NewListAllPager(&armcompute.VirtualMachinesClientListAllOptions{
		StatusOnly: to.Ptr("true"),
	})
	var out []store.CloudInstance
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: list VMs: %w", err)
		}
		for _, vm := range page.Value {
			out = append(out, toInstance(vm))
		}
	}

	// Enrich with IPs best-effort: a failure here (e.g. no network Reader)
	// leaves ssh_host empty rather than failing the whole inventory.
	a.enrichIPs(ctx, out)

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []store.CloudInstance{}
	}
	return out, nil
}

func (a *Azure) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	rg, name, err := parseVMID(instanceID)
	if err != nil {
		return store.PowerUnknown, err
	}
	res, err := a.vm.Get(ctx, rg, name, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return store.PowerUnknown, fmt.Errorf("azure: get %s: %w", name, err)
	}
	if res.Properties != nil && res.Properties.InstanceView != nil {
		return powerFromStatuses(res.Properties.InstanceView.Statuses), nil
	}
	return store.PowerUnknown, nil
}

func (a *Azure) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	rg, name, err := parseVMID(instanceID)
	if err != nil {
		return err
	}
	// Begin* submits the request and returns a poller; the request is accepted
	// by the time it returns, so — like the other adapters — we do not wait
	// for the VM to reach its target state.
	switch action {
	case provider.PowerStart:
		_, err = a.vm.BeginStart(ctx, rg, name, nil)
	case provider.PowerStop:
		// Graceful guest shutdown; the VM stays allocated (and billed).
		_, err = a.vm.BeginPowerOff(ctx, rg, name, nil)
	case provider.PowerForceStop:
		// Deallocate: release the compute, stop billing. The Azure portal's
		// "Stop" does this; it is the closer match to "pull the plug".
		_, err = a.vm.BeginDeallocate(ctx, rg, name, nil)
	case provider.PowerReboot, provider.PowerForceReboot:
		_, err = a.vm.BeginRestart(ctx, rg, name, nil)
	default:
		return provider.ErrUnsupportedAction
	}
	if err != nil {
		return fmt.Errorf("azure: %s %s: %w", action, name, err)
	}
	return nil
}

func toInstance(vm *armcompute.VirtualMachine) store.CloudInstance {
	ci := store.CloudInstance{
		InstanceID: str(vm.ID),
		Name:       str(vm.Name),
		Region:     str(vm.Location),
		PowerState: store.PowerUnknown,
	}
	if vm.Properties != nil {
		if vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
			ci.InstanceType = string(*vm.Properties.HardwareProfile.VMSize)
		}
		if vm.Properties.InstanceView != nil {
			ci.PowerState = powerFromStatuses(vm.Properties.InstanceView.Statuses)
		}
	}
	for k, v := range vm.Tags {
		if val := str(v); val == "" {
			ci.Labels = append(ci.Labels, k)
		} else {
			ci.Labels = append(ci.Labels, k+":"+val)
		}
	}
	sort.Strings(ci.Labels)
	return ci
}

// enrichIPs resolves each VM's private and public IP by sweeping all NICs and
// public IPs in the subscription once and joining in memory. Best-effort: on
// any error it simply leaves IPs blank.
func (a *Azure) enrichIPs(ctx context.Context, machines []store.CloudInstance) {
	// publicIP resource id (lower) -> address
	pips := map[string]string{}
	pp := a.pip.NewListAllPager(nil)
	for pp.More() {
		page, err := pp.NextPage(ctx)
		if err != nil {
			return
		}
		for _, ip := range page.Value {
			if ip.ID != nil && ip.Properties != nil && ip.Properties.IPAddress != nil {
				pips[strings.ToLower(*ip.ID)] = *ip.Properties.IPAddress
			}
		}
	}

	// NIC resource id (lower) -> {private, public}
	type nicIPs struct{ private, public string }
	nics := map[string]nicIPs{}
	np := a.nic.NewListAllPager(nil)
	for np.More() {
		page, err := np.NextPage(ctx)
		if err != nil {
			return
		}
		for _, n := range page.Value {
			if n.ID == nil || n.Properties == nil {
				continue
			}
			var v nicIPs
			for _, cfg := range n.Properties.IPConfigurations {
				if cfg.Properties == nil {
					continue
				}
				if v.private == "" && cfg.Properties.PrivateIPAddress != nil {
					v.private = *cfg.Properties.PrivateIPAddress
				}
				if cfg.Properties.PublicIPAddress != nil && cfg.Properties.PublicIPAddress.ID != nil {
					if addr := pips[strings.ToLower(*cfg.Properties.PublicIPAddress.ID)]; addr != "" && v.public == "" {
						v.public = addr
					}
				}
			}
			nics[strings.ToLower(*n.ID)] = v
		}
	}

	// Join: a VM references its NICs by id. We re-Get each VM's NIC refs from
	// the model we already have — but ListAll(StatusOnly) omits the network
	// profile, so resolve via the NIC list keyed by the VM's own id prefix is
	// not possible; instead match NICs whose id lives under the VM's resource
	// group and whose VM link points back. Azure NICs carry that back-link.
	byVMID := map[string]nicIPs{}
	np2 := a.nic.NewListAllPager(nil)
	for np2.More() {
		page, err := np2.NextPage(ctx)
		if err != nil {
			break
		}
		for _, n := range page.Value {
			if n.ID == nil || n.Properties == nil || n.Properties.VirtualMachine == nil || n.Properties.VirtualMachine.ID == nil {
				continue
			}
			byVMID[strings.ToLower(*n.Properties.VirtualMachine.ID)] = nics[strings.ToLower(*n.ID)]
		}
	}

	for i := range machines {
		if v, ok := byVMID[strings.ToLower(machines[i].InstanceID)]; ok {
			machines[i].PrivateIP = v.private
			machines[i].PublicIP = v.public
		}
	}
}

// powerFromStatuses reads the "PowerState/<x>" code from an instance view.
func powerFromStatuses(statuses []*armcompute.InstanceViewStatus) store.PowerState {
	for _, s := range statuses {
		if s == nil || s.Code == nil {
			continue
		}
		code := *s.Code
		if !strings.HasPrefix(code, "PowerState/") {
			continue
		}
		switch strings.TrimPrefix(code, "PowerState/") {
		case "running":
			return store.PowerRunning
		case "stopped", "deallocated":
			return store.PowerStopped
		case "starting", "stopping", "deallocating":
			return store.PowerPending
		}
	}
	return store.PowerUnknown
}

// parseVMID pulls the resource group and VM name out of a full ARM id:
// /subscriptions/<s>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<name>
func parseVMID(id string) (rg, name string, err error) {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		switch strings.ToLower(parts[i]) {
		case "resourcegroups":
			rg = parts[i+1]
		case "virtualmachines":
			name = parts[i+1]
		}
	}
	if rg == "" || name == "" {
		return "", "", fmt.Errorf("azure: cannot parse resource group / name from %q", id)
	}
	return rg, name, nil
}

// str safely dereferences an Azure *string.
func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
