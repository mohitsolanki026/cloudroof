// Package amazon adapts AWS EC2 to the provider interface.
//
// EC2 is the adapter that stress-tests the abstraction: credentials are a key
// pair rather than a token, instances live in regions that must each be
// queried separately, and a power call has to know which region to address.
// The region rides inside the instance ID ("eu-central-1:i-0abc…") so the
// Provider interface stays region-agnostic.
package amazon

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"bosun/internal/provider"
	"bosun/internal/store"
)

const Name = "aws"

// homeRegion is where DescribeRegions is asked; the answer is global.
const homeRegion = "us-east-1"

func init() {
	provider.Register(provider.Spec{
		Name:  Name,
		Label: "AWS EC2",
		Fields: []provider.Field{
			{Name: "accessKeyId", Label: "Access key ID", Kind: provider.FieldText},
			{Name: "secretAccessKey", Label: "Secret access key", Kind: provider.FieldSecret},
			{Name: "regions", Label: "Regions", Kind: provider.FieldList, Optional: true,
				Hint: "Comma-separated, e.g. eu-central-1, us-east-1. Leave empty to scan every region enabled on the account."},
		},
		Notes: "Create an IAM user with only ec2:DescribeInstances, ec2:DescribeRegions, ec2:StartInstances, ec2:StopInstances, ec2:RebootInstances. Bosun never needs anything else.",
	}, func(c provider.Credentials) (provider.Provider, error) {
		regions := make([]string, 0, len(c.Regions))
		for _, r := range c.Regions {
			if r = strings.TrimSpace(r); r != "" {
				regions = append(regions, r)
			}
		}
		return &AWS{
			creds:   credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""),
			regions: regions,
			http:    &http.Client{Timeout: 45 * time.Second},
			clients: map[string]*ec2.Client{},
		}, nil
	})
}

type AWS struct {
	creds   aws.CredentialsProvider
	regions []string // configured; empty means discover
	http    *http.Client

	mu      sync.Mutex
	clients map[string]*ec2.Client
}

func (a *AWS) Name() string { return Name }

// client returns a per-region EC2 client, built on first use.
func (a *AWS) client(ctx context.Context, region string) (*ec2.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[region]; ok {
		return c, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(a.creds),
		config.WithHTTPClient(a.http),
	)
	if err != nil {
		return nil, fmt.Errorf("aws: config for %s: %w", region, err)
	}
	c := ec2.NewFromConfig(cfg)
	a.clients[region] = c
	return c, nil
}

// regionList is the configured regions, or every region enabled on the
// account when none were given.
func (a *AWS) regionList(ctx context.Context) ([]string, error) {
	if len(a.regions) > 0 {
		return a.regions, nil
	}
	c, err := a.client(ctx, homeRegion)
	if err != nil {
		return nil, err
	}
	out, err := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("aws: describe regions: %w", err)
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		if name := aws.ToString(r.RegionName); name != "" {
			regions = append(regions, name)
		}
	}
	sort.Strings(regions)
	return regions, nil
}

func (a *AWS) ListInstances(ctx context.Context) ([]store.CloudInstance, error) {
	regions, err := a.regionList(ctx)
	if err != nil {
		return nil, err
	}

	// Regions are independent; query them concurrently, a few at a time.
	// Any failure fails the whole list: a partial inventory would make the
	// caller mark every instance in the missing region as gone.
	var (
		mu       sync.Mutex
		out      []store.CloudInstance
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, 4)
	)
	for _, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ins, err := a.listRegion(ctx, region)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out = append(out, ins...)
		}(region)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []store.CloudInstance{}
	}
	return out, nil
}

func (a *AWS) listRegion(ctx context.Context, region string) ([]store.CloudInstance, error) {
	c, err := a.client(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []store.CloudInstance
	p := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: describe instances in %s: %w", region, err)
		}
		for _, res := range page.Reservations {
			for _, in := range res.Instances {
				// Terminated instances linger in the API for an hour; they
				// are not machines any more.
				if in.State != nil && in.State.Name == types.InstanceStateNameTerminated {
					continue
				}
				out = append(out, toInstance(region, in))
			}
		}
	}
	return out, nil
}

func toInstance(region string, in types.Instance) store.CloudInstance {
	id := aws.ToString(in.InstanceId)
	ci := store.CloudInstance{
		InstanceID:   region + ":" + id,
		Name:         id,
		Region:       region,
		InstanceType: string(in.InstanceType),
		PublicIP:     aws.ToString(in.PublicIpAddress),
		PrivateIP:    aws.ToString(in.PrivateIpAddress),
		PowerState:   store.PowerUnknown,
	}
	if in.State != nil {
		ci.PowerState = mapState(in.State.Name)
	}
	if in.Placement != nil && in.Placement.AvailabilityZone != nil {
		ci.Region = aws.ToString(in.Placement.AvailabilityZone)
	}
	for _, t := range in.Tags {
		k, v := aws.ToString(t.Key), aws.ToString(t.Value)
		switch {
		case k == "Name":
			if v != "" {
				ci.Name = v
			}
		case strings.HasPrefix(k, "aws:"):
			// Internal tags (autoscaling, cloudformation) are noise here.
		case v == "":
			ci.Labels = append(ci.Labels, k)
		default:
			ci.Labels = append(ci.Labels, k+":"+v)
		}
	}
	sort.Strings(ci.Labels)
	return ci
}

func mapState(s types.InstanceStateName) store.PowerState {
	switch s {
	case types.InstanceStateNameRunning:
		return store.PowerRunning
	case types.InstanceStateNameStopped:
		return store.PowerStopped
	case types.InstanceStateNamePending, types.InstanceStateNameStopping, types.InstanceStateNameShuttingDown:
		return store.PowerPending
	default:
		return store.PowerUnknown
	}
}

// split takes "region:i-…" apart.
func split(instanceID string) (region, id string, err error) {
	region, id, ok := strings.Cut(instanceID, ":")
	if !ok || region == "" || !strings.HasPrefix(id, "i-") {
		return "", "", fmt.Errorf("aws: invalid instance id %q (want region:i-…)", instanceID)
	}
	return region, id, nil
}

func (a *AWS) GetPowerState(ctx context.Context, instanceID string) (store.PowerState, error) {
	region, id, err := split(instanceID)
	if err != nil {
		return store.PowerUnknown, err
	}
	c, err := a.client(ctx, region)
	if err != nil {
		return store.PowerUnknown, err
	}
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return store.PowerUnknown, fmt.Errorf("aws: describe %s: %w", instanceID, err)
	}
	for _, res := range out.Reservations {
		for _, in := range res.Instances {
			if in.State != nil {
				return mapState(in.State.Name), nil
			}
		}
	}
	return store.PowerUnknown, fmt.Errorf("aws: instance %s not found", instanceID)
}

func (a *AWS) Power(ctx context.Context, instanceID string, action provider.PowerAction) error {
	region, id, err := split(instanceID)
	if err != nil {
		return err
	}
	c, err := a.client(ctx, region)
	if err != nil {
		return err
	}
	ids := []string{id}
	switch action {
	case provider.PowerStart:
		_, err = c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: ids})
	case provider.PowerStop:
		_, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids})
	case provider.PowerForceStop:
		_, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids, Force: aws.Bool(true)})
	case provider.PowerReboot, provider.PowerForceReboot:
		// EC2 has one reboot: it asks the guest nicely and performs a hard
		// reboot itself if the guest has not complied within four minutes.
		_, err = c.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: ids})
	default:
		return provider.ErrUnsupportedAction
	}
	if err != nil {
		return fmt.Errorf("aws: %s %s: %w", action, instanceID, err)
	}
	return nil
}
