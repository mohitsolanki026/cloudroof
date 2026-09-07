package store

import "time"

// PowerState is what the cloud provider believes about the instance.
type PowerState string

const (
	PowerRunning PowerState = "running"
	PowerStopped PowerState = "stopped"
	PowerPending PowerState = "pending"
	PowerUnknown PowerState = "unknown"
)

// ReachState is what SSH actually observed. It is tracked separately from
// PowerState on purpose — the two disagreeing is the most useful thing Bosun
// can tell you.
type ReachState string

const (
	ReachOK             ReachState = "ok"
	ReachRefused        ReachState = "refused"
	ReachTimeout        ReachState = "timeout"
	ReachAuthFailed     ReachState = "auth_failed"
	ReachHostKeyChanged ReachState = "hostkey_changed"
	ReachUnknown        ReachState = "unknown"
)

// CredKind distinguishes the two supported SSH auth methods. Keys are strongly
// preferred; passwords exist because a real share of the target audience still
// has password-auth VPSes and rejecting them at the add-machine screen is a
// harsh first impression.
type CredKind string

const (
	CredSSHKey   CredKind = "ssh_key"
	CredPassword CredKind = "password"
)

type Credential struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Kind        CredKind  `json:"kind"`
	Sealed      string    `json:"-"` // never leaves the process
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
}

type CloudAccount struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Provider      string     `json:"provider"`
	SealedToken   string     `json:"-"`
	LastSyncAt    *time.Time `json:"lastSyncAt"`
	LastSyncError string     `json:"lastSyncError"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// Machine is the join between a cloud instance and an SSH host. Either half may
// be empty: a bare-metal box has no cloud identity, and an instance you have
// not yet given credentials for has no host identity.
type Machine struct {
	ID   int64    `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`

	// Cloud identity.
	CloudAccountID *int64     `json:"cloudAccountId"`
	Provider       string     `json:"provider"`
	InstanceID     string     `json:"instanceId"`
	Region         string     `json:"region"`
	InstanceType   string     `json:"instanceType"`
	PowerState     PowerState `json:"powerState"`
	PublicIP       string     `json:"publicIp"`
	PrivateIP      string     `json:"privateIp"`
	CloudSyncedAt  *time.Time `json:"cloudSyncedAt"`
	Missing        bool       `json:"missing"`

	// Host identity.
	SSHHost      string `json:"sshHost"`
	SSHPort      int    `json:"sshPort"`
	SSHUser      string `json:"sshUser"`
	CredentialID *int64 `json:"credentialId"`

	// Reachability.
	ReachState     ReachState `json:"reachState"`
	ReachError     string     `json:"reachError"`
	ReachCheckedAt *time.Time `json:"reachCheckedAt"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// HasCloud reports whether power control is possible.
func (m Machine) HasCloud() bool { return m.InstanceID != "" && m.CloudAccountID != nil }

// HasHost reports whether SSH actions are possible.
func (m Machine) HasHost() bool { return m.SSHHost != "" && m.SSHUser != "" && m.CredentialID != nil }

// Address is the host:port SSH should dial.
func (m Machine) Address() string { return m.SSHHost }

type HostFacts struct {
	MachineID    int64     `json:"machineId"`
	Hostname     string    `json:"hostname"`
	OSName       string    `json:"osName"`
	OSVersion    string    `json:"osVersion"`
	Kernel       string    `json:"kernel"`
	Arch         string    `json:"arch"`
	InitSystem   string    `json:"initSystem"` // systemd | openrc | sysv | unknown
	SudoMode     string    `json:"sudoMode"`   // root | nopasswd | password | none | unknown
	Capabilities []string  `json:"capabilities"`
	FetchedAt    time.Time `json:"fetchedAt"`
}

// Has reports whether the host advertised a capability, e.g. "docker".
func (f HostFacts) Has(cap string) bool {
	for _, c := range f.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

type HostKey struct {
	MachineID   int64     `json:"machineId"`
	Algorithm   string    `json:"algorithm"`
	Fingerprint string    `json:"fingerprint"`
	FirstSeen   time.Time `json:"firstSeen"`
	// Seen* hold the most recent key that failed to match the pin. Empty
	// when the last connection matched.
	SeenAlgorithm   string `json:"seenAlgorithm"`
	SeenFingerprint string `json:"seenFingerprint"`
}

// Run is one audited command execution. Every action the engine performs writes
// one of these, including read-only ones and including failures.
type Run struct {
	ID          int64     `json:"id"`
	MachineID   *int64    `json:"machineId"`
	MachineName string    `json:"machineName"`
	ActionID    string    `json:"actionId"`
	Command     string    `json:"command"`
	Danger      int       `json:"danger"`
	Actor       string    `json:"actor"`
	ExitCode    *int      `json:"exitCode"`
	Stdout      string    `json:"stdout"`
	Stderr      string    `json:"stderr"`
	Error       string    `json:"error"`
	DurationMS  int64     `json:"durationMs"`
	StartedAt   time.Time `json:"startedAt"`
}

// Group is a set of tags; a machine belongs when it carries ALL of them.
// Membership is never stored — it is resolved live from each machine's tags,
// so it tracks the fleet as tags change.
type Group struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"createdAt"`
	// Members is filled by the API when listing; the number of machines that
	// currently match. Not a stored column.
	Members int `json:"members"`
}

// CustomActionParam is one templated input of a saved action. Unlike built-in
// params it has no author-supplied regex: values are shell-quoted and length-
// capped at render time, which is sufficient because the admin already has a
// full terminal on the box.
type CustomActionParam struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// CustomAction is an admin-authored command saved to run as a button. It maps
// to the same execution path as a built-in action; the danger tier it declares
// is enforced by the same gate. Stored raw here; the actions package converts
// it to an actions.Action.
type CustomAction struct {
	ID        string              `json:"id"`
	Label     string              `json:"label"`
	Category  string              `json:"category"`
	Command   string              `json:"command"`
	Params    []CustomActionParam `json:"params"`
	Requires  []string            `json:"requires"`
	Sudo      string              `json:"sudo"` // "" | preferred | required
	Danger    int                 `json:"danger"`
	CreatedAt time.Time           `json:"createdAt"`
}
