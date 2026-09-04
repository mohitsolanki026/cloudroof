package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"bosun/internal/actions"
	"bosun/internal/facts"
	"bosun/internal/provider"
	"bosun/internal/sshx"
	"bosun/internal/store"
)

const actor = "admin" // single-user in v0.1; multi-user lands in v2.5

func pathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func ptrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- machines ---------------------------------------------------------------

// machineView is what the fleet list renders. Facts ride along so the list can
// show OS and init system without a second round-trip per row.
type machineView struct {
	store.Machine
	Facts *store.HostFacts `json:"facts"`
}

func (s *Server) listMachines(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListMachines()
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]machineView, 0, len(ms))
	for _, m := range ms {
		v := machineView{Machine: m}
		if f, err := s.store.GetFacts(m.ID); err == nil {
			v.Facts = &f
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *Server) getMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		fail(w, err)
		return
	}
	v := machineView{Machine: m}
	if f, err := s.store.GetFacts(m.ID); err == nil {
		v.Facts = &f
	}
	writeJSON(w, 200, v)
}

type machineInput struct {
	Name           string   `json:"name"`
	Tags           []string `json:"tags"`
	SSHHost        string   `json:"sshHost"`
	SSHPort        int      `json:"sshPort"`
	SSHUser        string   `json:"sshUser"`
	CredentialID   *int64   `json:"credentialId"`
	CloudAccountID *int64   `json:"cloudAccountId"`
	InstanceID     string   `json:"instanceId"`
	Provider       string   `json:"provider"`
}

// normalize trims what users paste. IPv6 literals lose their brackets here
// and get them back in resolveTarget via net.JoinHostPort.
func (in *machineInput) normalize() {
	in.Name = strings.TrimSpace(in.Name)
	in.SSHHost = strings.Trim(strings.TrimSpace(in.SSHHost), "[]")
	in.SSHUser = strings.TrimSpace(in.SSHUser)
	if in.SSHPort == 0 {
		in.SSHPort = 22
	}
}

func (in machineInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if in.SSHPort < 0 || in.SSHPort > 65535 {
		return errors.New("sshPort out of range")
	}
	if (in.InstanceID != "") != (in.CloudAccountID != nil) {
		return errors.New("instanceId and cloudAccountId must be set together")
	}
	return nil
}

func (s *Server) createMachine(w http.ResponseWriter, r *http.Request) {
	var in machineInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.normalize()
	if err := in.validate(); err != nil {
		badRequest(w, err.Error())
		return
	}
	m, err := s.store.CreateMachine(store.Machine{
		Name: in.Name, Tags: in.Tags,
		SSHHost: in.SSHHost, SSHPort: in.SSHPort, SSHUser: in.SSHUser,
		CredentialID: in.CredentialID, CloudAccountID: in.CloudAccountID,
		InstanceID: in.InstanceID, Provider: in.Provider,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, machineView{Machine: m})
}

func (s *Server) updateMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		fail(w, err)
		return
	}
	var in machineInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.normalize()
	if err := in.validate(); err != nil {
		badRequest(w, err.Error())
		return
	}

	// A different host or port is a different box: forget its pinned key and
	// fingerprint so the next probe starts clean. A different user or
	// credential is the same box but a different session: just drop the
	// pooled connection so nothing keeps running under the old identity.
	boxChanged := m.SSHHost != in.SSHHost || m.SSHPort != in.SSHPort
	sessionChanged := boxChanged || m.SSHUser != in.SSHUser || !ptrEq(m.CredentialID, in.CredentialID)

	m.Name, m.Tags = in.Name, in.Tags
	m.SSHHost, m.SSHPort, m.SSHUser = in.SSHHost, in.SSHPort, in.SSHUser
	m.CredentialID, m.CloudAccountID = in.CredentialID, in.CloudAccountID
	m.InstanceID, m.Provider = in.InstanceID, in.Provider

	if err := s.store.UpdateMachine(m); err != nil {
		fail(w, err)
		return
	}
	if sessionChanged {
		s.broker.Drop(m.ID)
		_ = s.store.SetReach(m.ID, store.ReachUnknown, "")
	}
	if boxChanged {
		_ = s.store.ResetHostIdentity(m.ID)
	}
	m, _ = s.store.GetMachine(id)
	writeJSON(w, 200, machineView{Machine: m})
}

func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	s.broker.Drop(id)
	if err := s.store.DeleteMachine(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

// probeMachine checks reachability and, on success, refreshes host facts. It
// is the "connect" step: the UI calls it after adding a machine and on demand.
func (s *Server) probeMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		fail(w, err)
		return
	}
	if !m.HasHost() {
		fail(w, actions.ErrNoHost)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// A probe means "verify I can connect right now". Reusing a pooled
	// connection would answer from the past: it never re-checks the host
	// key, auth, or that sshd is still accepting. Force a fresh handshake.
	s.broker.Drop(m.ID)

	f, err := s.probe(ctx, m)
	m, _ = s.store.GetMachine(id)
	if err != nil {
		// The reach state has been recorded; return it with the error so the
		// UI can show both.
		writeJSON(w, 502, struct {
			apiError
			Machine machineView `json:"machine"`
		}{apiError{Error: err.Error(), Code: reachCode(err)}, machineView{Machine: m}})
		return
	}
	writeJSON(w, 200, machineView{Machine: m, Facts: &f})
}

// probe does the work: dial, classify, record, fingerprint.
func (s *Server) probe(ctx context.Context, m store.Machine) (store.HostFacts, error) {
	target, err := s.resolveTarget(m)
	if err != nil {
		_ = s.store.SetReach(m.ID, store.ReachUnknown, err.Error())
		return store.HostFacts{}, err
	}

	f, err := facts.Fingerprint(ctx, s.broker, target)
	if err != nil {
		s.broker.Drop(m.ID)
		_ = s.store.SetReach(m.ID, classifyReach(err), err.Error())
		return store.HostFacts{}, err
	}
	if err := s.store.UpsertFacts(f); err != nil {
		return f, err
	}
	_ = s.store.SetReach(m.ID, store.ReachOK, "")
	return f, nil
}

func classifyReach(err error) store.ReachState {
	switch {
	case errors.Is(err, sshx.ErrAuth):
		return store.ReachAuthFailed
	case errors.Is(err, sshx.ErrHostKeyMismatch):
		return store.ReachHostKeyChanged
	case errors.Is(err, sshx.ErrTimeout), errors.Is(err, sshx.ErrNoRoute):
		return store.ReachTimeout
	case errors.Is(err, sshx.ErrRefused):
		return store.ReachRefused
	default:
		return store.ReachUnknown
	}
}

func reachCode(err error) string {
	switch classifyReach(err) {
	case store.ReachAuthFailed:
		return "auth_failed"
	case store.ReachHostKeyChanged:
		return "hostkey_changed"
	case store.ReachTimeout, store.ReachRefused:
		return "unreachable"
	}
	return "internal"
}

// --- power ------------------------------------------------------------------

type powerInput struct {
	Action      provider.PowerAction `json:"action"`
	Confirm     bool                 `json:"confirm"`
	ConfirmName string               `json:"confirmName"`
}

func (s *Server) powerMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	var in powerInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !in.Action.Valid() {
		badRequest(w, "unknown power action")
		return
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		fail(w, err)
		return
	}
	if !m.HasCloud() {
		writeJSON(w, 409, apiError{Error: "machine is not linked to a cloud instance", Code: "not_available"})
		return
	}

	// Same tiers as the catalog: start is reversible, everything else is
	// disruptive and needs the name typed.
	switch in.Action.Danger() {
	case actions.DangerReversible:
		if !in.Confirm {
			fail(w, actions.ErrNeedsConfirm)
			return
		}
	default:
		if in.ConfirmName != m.Name {
			fail(w, actions.ErrNeedsName)
			return
		}
	}

	p, err := s.providerFor(*m.CloudAccountID)
	if err != nil {
		fail(w, err)
		return
	}

	run := store.Run{
		MachineID:   &m.ID,
		MachineName: m.Name,
		ActionID:    "power." + string(in.Action),
		Command:     fmt.Sprintf("%s %s %s", p.Name(), in.Action, m.InstanceID),
		Danger:      in.Action.Danger(),
		Actor:       actor,
		StartedAt:   time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	start := time.Now()
	perr := p.Power(ctx, m.InstanceID, in.Action)
	run.DurationMS = time.Since(start).Milliseconds()
	if perr != nil {
		run.Error = perr.Error()
	} else {
		zero := 0
		run.ExitCode = &zero
	}
	if _, err := s.store.InsertRun(run); err != nil {
		fail(w, err)
		return
	}
	if perr != nil {
		writeJSON(w, 502, apiError{Error: perr.Error(), Code: "provider_error"})
		return
	}

	// The instance is now in transition; drop the SSH connection so the next
	// action sees the real state instead of a dead pipe.
	s.broker.Drop(m.ID)
	if st, err := p.GetPowerState(ctx, m.InstanceID); err == nil {
		_ = s.store.SetPowerState(m.ID, st)
	}
	m, _ = s.store.GetMachine(id)
	writeJSON(w, 200, machineView{Machine: m})
}

// --- actions ----------------------------------------------------------------

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	f, err := s.store.GetFacts(id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 200, actions.Available(nil))
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, actions.Available(f.Capabilities))
}

type actionInput struct {
	Params      map[string]string `json:"params"`
	Confirm     bool              `json:"confirm"`
	ConfirmName string            `json:"confirmName"`
}

func (s *Server) runAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	// ContentLength is -1 for chunked bodies, so test for a body rather
	// than for a positive length; an empty body is a valid Tier-0 request.
	var in actionInput
	if r.Body != nil && r.Body != http.NoBody {
		if err := readJSON(w, r, &in); err != nil && !errors.Is(err, io.EOF) {
			badRequest(w, err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res, err := s.engine.Run(ctx, actions.Request{
		MachineID:   id,
		ActionID:    r.PathValue("action"),
		Params:      in.Params,
		Confirm:     in.Confirm,
		ConfirmName: in.ConfirmName,
		Actor:       actor,
	})
	if err != nil {
		// A transport failure updates reachability; the next fleet poll
		// reflects it.
		if rs := classifyReach(err); rs != store.ReachUnknown {
			_ = s.store.SetReach(id, rs, err.Error())
			s.broker.Drop(id)
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// --- host keys --------------------------------------------------------------

func (s *Server) getHostKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	k, err := s.store.GetHostKey(id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, k)
}

type trustInput struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
}

// trustHostKey replaces the pinned key with the one most recently observed.
// The client must echo back exactly that fingerprint, so a stale dialog
// cannot approve a key it never saw, and nothing can be trusted that the
// host did not actually present.
func (s *Server) trustHostKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	var in trustInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	k, err := s.store.GetHostKey(id)
	if err != nil {
		fail(w, err)
		return
	}
	if k.SeenFingerprint == "" {
		writeJSON(w, 409, apiError{Error: "no host key change is pending for this machine", Code: "stale"})
		return
	}
	if in.Fingerprint != k.SeenFingerprint || in.Algorithm != k.SeenAlgorithm {
		writeJSON(w, 409, apiError{
			Error:   "fingerprint does not match the key last seen from this host — re-probe and review again",
			Code:    "stale",
			Details: map[string]string{"algorithm": k.SeenAlgorithm, "fingerprint": k.SeenFingerprint},
		})
		return
	}
	if err := s.store.TrustHostKey(id, k.SeenAlgorithm, k.SeenFingerprint); err != nil {
		fail(w, err)
		return
	}
	s.broker.Drop(id)
	_ = s.store.SetReach(id, store.ReachUnknown, "")
	w.WriteHeader(204)
}

// --- credentials ------------------------------------------------------------

type credentialInput struct {
	Name       string         `json:"name"`
	Kind       store.CredKind `json:"kind"`
	PrivateKey string         `json:"privateKey"`
	Passphrase string         `json:"passphrase"`
	Password   string         `json:"password"`
}

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store.ListCredentials()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, cs)
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request) {
	var in credentialInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badRequest(w, "name is required")
		return
	}

	var secret sealedSecret
	var fingerprint string
	switch in.Kind {
	case store.CredSSHKey:
		if in.PrivateKey == "" {
			badRequest(w, "privateKey is required")
			return
		}
		// Parse now so a bad paste fails at save time, not at first connect.
		var signer ssh.Signer
		var err error
		if in.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(in.PrivateKey), []byte(in.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(in.PrivateKey))
		}
		if err != nil {
			var pe *ssh.PassphraseMissingError
			if errors.As(err, &pe) {
				badRequest(w, "this key is encrypted — provide its passphrase")
				return
			}
			badRequest(w, "could not parse private key: "+err.Error())
			return
		}
		fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
		secret = sealedSecret{PrivateKey: in.PrivateKey, Passphrase: in.Passphrase}
	case store.CredPassword:
		if in.Password == "" {
			badRequest(w, "password is required")
			return
		}
		secret = sealedSecret{Password: in.Password}
	default:
		badRequest(w, "kind must be ssh_key or password")
		return
	}

	plain, _ := json.Marshal(secret)
	sealed, err := s.keys.Seal(plain)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.store.CreateCredential(store.Credential{
		Name: in.Name, Kind: in.Kind, Sealed: sealed, Fingerprint: fingerprint,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	// Machines that used it lose their credential (FK SET NULL) and their
	// pooled connection — only theirs; other machines' terminals stay up.
	ids, _ := s.store.MachineIDsByCredential(id)
	if err := s.store.DeleteCredential(id); err != nil {
		fail(w, err)
		return
	}
	for _, mid := range ids {
		s.broker.Drop(mid)
	}
	w.WriteHeader(204)
}

// --- cloud accounts ---------------------------------------------------------

type accountInput struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	as, err := s.store.ListCloudAccounts()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, as)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" || in.Token == "" {
		badRequest(w, "name and token are required")
		return
	}
	// Validate the token by constructing the provider and making one cheap
	// call, so a typo is caught here rather than on the first sync.
	p, err := provider.New(in.Provider, in.Token)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if _, err := p.ListInstances(ctx); err != nil {
		writeJSON(w, 502, apiError{Error: "provider rejected the token: " + err.Error(), Code: "provider_error"})
		return
	}

	sealed, err := s.keys.Seal([]byte(in.Token))
	if err != nil {
		fail(w, err)
		return
	}
	a, err := s.store.CreateCloudAccount(store.CloudAccount{
		Name: in.Name, Provider: in.Provider, SealedToken: sealed,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	if err := s.store.DeleteCloudAccount(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) syncAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := s.sync(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// --- audit & metadata -------------------------------------------------------

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	var machineID *int64
	if v := r.URL.Query().Get("machine"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			badRequest(w, "bad machine id")
			return
		}
		machineID = &id
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.ListRuns(machineID, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, runs)
}

func (s *Server) catalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, actions.Catalog)
}

func (s *Server) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, provider.Names())
}
