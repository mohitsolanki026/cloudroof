package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrNotFound = errors.New("store: not found")

// --- credentials ------------------------------------------------------------

func (s *Store) CreateCredential(c Credential) (Credential, error) {
	c.CreatedAt = time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO credentials (name, kind, sealed, fingerprint, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.Name, string(c.Kind), c.Sealed, c.Fingerprint, ms(c.CreatedAt),
	)
	if err != nil {
		return c, fmt.Errorf("store: create credential: %w", err)
	}
	c.ID, _ = res.LastInsertId()
	return c, nil
}

func (s *Store) ListCredentials() ([]Credential, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, sealed, fingerprint, created_at FROM credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()

	out := []Credential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCredential(id int64) (Credential, error) {
	row := s.db.QueryRow(`SELECT id, name, kind, sealed, fingerprint, created_at FROM credentials WHERE id = ?`, id)
	c, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) DeleteCredential(id int64) error {
	_, err := s.db.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanCredential(r scanner) (Credential, error) {
	var c Credential
	var kind string
	var created int64
	if err := r.Scan(&c.ID, &c.Name, &kind, &c.Sealed, &c.Fingerprint, &created); err != nil {
		return c, err
	}
	c.Kind = CredKind(kind)
	c.CreatedAt = fromMs(created)
	return c, nil
}

// --- cloud accounts ---------------------------------------------------------

func (s *Store) CreateCloudAccount(a CloudAccount) (CloudAccount, error) {
	a.CreatedAt = time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO cloud_accounts (name, provider, sealed_token, last_sync_error, created_at) VALUES (?, ?, ?, '', ?)`,
		a.Name, a.Provider, a.SealedToken, ms(a.CreatedAt),
	)
	if err != nil {
		return a, fmt.Errorf("store: create cloud account: %w", err)
	}
	a.ID, _ = res.LastInsertId()
	return a, nil
}

const cloudAccountCols = `id, name, provider, sealed_token, last_sync_at, last_sync_error, created_at`

func (s *Store) ListCloudAccounts() ([]CloudAccount, error) {
	rows, err := s.db.Query(`SELECT ` + cloudAccountCols + ` FROM cloud_accounts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list cloud accounts: %w", err)
	}
	defer rows.Close()

	out := []CloudAccount{}
	for rows.Next() {
		a, err := scanCloudAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetCloudAccount(id int64) (CloudAccount, error) {
	row := s.db.QueryRow(`SELECT `+cloudAccountCols+` FROM cloud_accounts WHERE id = ?`, id)
	a, err := scanCloudAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// DeleteCloudAccount removes the account and strips the cloud half from every
// machine it owned. The FK alone would only null cloud_account_id and leave a
// stale provider/instance/power_state the UI would present as live — and a
// re-added account would then collide with those orphaned instance IDs.
func (s *Store) DeleteCloudAccount(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE machines SET
			cloud_account_id = NULL, provider = '', instance_id = '', region = '',
			instance_type = '', power_state = 'unknown', cloud_synced_at = NULL,
			missing = 0, updated_at = ?
		WHERE cloud_account_id = ?`, ms(time.Now()), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cloud_accounts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSyncResult records the outcome of a sync attempt. syncErr is stored rather
// than discarded so the UI can explain why a fleet looks stale.
func (s *Store) SetSyncResult(id int64, at time.Time, syncErr string) error {
	_, err := s.db.Exec(
		`UPDATE cloud_accounts SET last_sync_at = ?, last_sync_error = ? WHERE id = ?`,
		ms(at), syncErr, id,
	)
	return err
}

func scanCloudAccount(r scanner) (CloudAccount, error) {
	var a CloudAccount
	var lastSync sql.NullInt64
	var created int64
	if err := r.Scan(&a.ID, &a.Name, &a.Provider, &a.SealedToken, &lastSync, &a.LastSyncError, &created); err != nil {
		return a, err
	}
	a.LastSyncAt = fromMsPtr(lastSync)
	a.CreatedAt = fromMs(created)
	return a, nil
}

// --- machines ---------------------------------------------------------------

const machineCols = `
	id, name, tags,
	cloud_account_id, provider, instance_id, region, instance_type,
	power_state, public_ip, private_ip, cloud_synced_at, missing,
	ssh_host, ssh_port, ssh_user, credential_id,
	reach_state, reach_error, reach_checked_at,
	created_at, updated_at`

func (s *Store) CreateMachine(m Machine) (Machine, error) {
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if m.SSHPort == 0 {
		m.SSHPort = 22
	}
	if m.PowerState == "" {
		m.PowerState = PowerUnknown
	}
	if m.ReachState == "" {
		m.ReachState = ReachUnknown
	}

	res, err := s.db.Exec(`
		INSERT INTO machines (
			name, tags, cloud_account_id, provider, instance_id, region, instance_type,
			power_state, public_ip, private_ip, cloud_synced_at, missing,
			ssh_host, ssh_port, ssh_user, credential_id,
			reach_state, reach_error, reach_checked_at, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Name, joinTags(m.Tags), nullInt64(m.CloudAccountID), m.Provider, m.InstanceID,
		m.Region, m.InstanceType, string(m.PowerState), m.PublicIP, m.PrivateIP,
		nullInt64(msPtr(m.CloudSyncedAt)), boolInt(m.Missing),
		m.SSHHost, m.SSHPort, m.SSHUser, nullInt64(m.CredentialID),
		string(m.ReachState), m.ReachError, nullInt64(msPtr(m.ReachCheckedAt)),
		ms(m.CreatedAt), ms(m.UpdatedAt),
	)
	if err != nil {
		return m, fmt.Errorf("store: create machine: %w", err)
	}
	m.ID, _ = res.LastInsertId()
	return m, nil
}

func (s *Store) ListMachines() ([]Machine, error) {
	rows, err := s.db.Query(`SELECT ` + machineCols + ` FROM machines ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("store: list machines: %w", err)
	}
	defer rows.Close()

	out := []Machine{}
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMachine(id int64) (Machine, error) {
	row := s.db.QueryRow(`SELECT `+machineCols+` FROM machines WHERE id = ?`, id)
	m, err := scanMachine(row)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// UpdateMachine writes the user-editable fields. Cloud-derived and
// reachability fields have their own narrow setters so a stale form POST can
// never clobber live state.
func (s *Store) UpdateMachine(m Machine) error {
	if m.SSHPort == 0 {
		m.SSHPort = 22
	}
	_, err := s.db.Exec(`
		UPDATE machines SET
			name = ?, tags = ?, ssh_host = ?, ssh_port = ?, ssh_user = ?,
			credential_id = ?, cloud_account_id = ?, instance_id = ?, provider = ?,
			updated_at = ?
		WHERE id = ?`,
		m.Name, joinTags(m.Tags), m.SSHHost, m.SSHPort, m.SSHUser,
		nullInt64(m.CredentialID), nullInt64(m.CloudAccountID), m.InstanceID, m.Provider,
		ms(time.Now()), m.ID,
	)
	return err
}

func (s *Store) DeleteMachine(id int64) error {
	_, err := s.db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	return err
}

// MachineIDsByCredential lists machines that authenticate with a credential,
// so a deletion can drop exactly their pooled connections.
func (s *Store) MachineIDsByCredential(credID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM machines WHERE credential_id = ?`, credID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResetHostIdentity forgets everything learned from a machine's host — the
// pinned key and the fingerprint — for when it is re-pointed at a different
// box. The next probe pins and fingerprints fresh.
func (s *Store) ResetHostIdentity(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM host_keys WHERE machine_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM host_facts WHERE machine_id = ?`, id)
	return err
}

// SetReach records the result of a reachability probe.
func (s *Store) SetReach(id int64, state ReachState, reachErr string) error {
	_, err := s.db.Exec(
		`UPDATE machines SET reach_state = ?, reach_error = ?, reach_checked_at = ?, updated_at = ? WHERE id = ?`,
		string(state), reachErr, ms(time.Now()), ms(time.Now()), id,
	)
	return err
}

// SetPowerState records what the provider reports, without touching anything
// the user owns.
func (s *Store) SetPowerState(id int64, state PowerState) error {
	_, err := s.db.Exec(
		`UPDATE machines SET power_state = ?, updated_at = ? WHERE id = ?`,
		string(state), ms(time.Now()), id,
	)
	return err
}

// CloudInstance is the provider-derived half of a machine, as returned by an
// adapter and handed to UpsertFromCloud.
type CloudInstance struct {
	InstanceID   string
	Name         string
	Region       string
	InstanceType string
	PowerState   PowerState
	PublicIP     string
	PrivateIP    string
	Labels       []string
}

// UpsertFromCloud reconciles one synced instance into the machines table.
//
// It is deliberately additive: on an existing row it refreshes only
// cloud-derived columns and never touches ssh_*, credential_id, or the
// user-chosen name. A sync must never undo the user's configuration.
func (s *Store) UpsertFromCloud(accountID int64, provider string, in CloudInstance) (int64, bool, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM machines WHERE cloud_account_id = ? AND instance_id = ?`,
		accountID, in.InstanceID,
	).Scan(&id)

	now := ms(time.Now())

	switch {
	case errors.Is(err, sql.ErrNoRows):
		m := Machine{
			Name:           in.Name,
			Tags:           in.Labels,
			CloudAccountID: &accountID,
			Provider:       provider,
			InstanceID:     in.InstanceID,
			Region:         in.Region,
			InstanceType:   in.InstanceType,
			PowerState:     in.PowerState,
			PublicIP:       in.PublicIP,
			PrivateIP:      in.PrivateIP,
			// Pre-fill the SSH address from the public IP so linking is one
			// field (a username) rather than three.
			SSHHost: in.PublicIP,
			SSHPort: 22,
		}
		t := time.Now().UTC()
		m.CloudSyncedAt = &t
		created, cerr := s.CreateMachine(m)
		if cerr != nil {
			return 0, false, cerr
		}
		return created.ID, true, nil

	case err != nil:
		return 0, false, fmt.Errorf("store: lookup instance %s: %w", in.InstanceID, err)

	default:
		_, uerr := s.db.Exec(`
			UPDATE machines SET
				region = ?, instance_type = ?, power_state = ?,
				public_ip = ?, private_ip = ?, cloud_synced_at = ?,
				missing = 0, updated_at = ?
			WHERE id = ?`,
			in.Region, in.InstanceType, string(in.PowerState),
			in.PublicIP, in.PrivateIP, now, now, id,
		)
		if uerr != nil {
			return 0, false, fmt.Errorf("store: refresh instance %s: %w", in.InstanceID, uerr)
		}
		return id, false, nil
	}
}

// MarkMissing flags machines from an account that the latest sync did not
// return. They are flagged rather than deleted: the user may have annotated
// them, and a provider API blip should never silently erase the fleet.
func (s *Store) MarkMissing(accountID int64, seen []string) error {
	if len(seen) == 0 {
		_, err := s.db.Exec(
			`UPDATE machines SET missing = 1, updated_at = ? WHERE cloud_account_id = ? AND instance_id != ''`,
			ms(time.Now()), accountID,
		)
		return err
	}
	q := `UPDATE machines SET missing = 1, updated_at = ?
	      WHERE cloud_account_id = ? AND instance_id != ''
	        AND instance_id NOT IN (?` + strings.Repeat(`,?`, len(seen)-1) + `)`
	args := []any{ms(time.Now()), accountID}
	for _, s := range seen {
		args = append(args, s)
	}
	_, err := s.db.Exec(q, args...)
	return err
}

func scanMachine(r scanner) (Machine, error) {
	var m Machine
	var tags, power, reach string
	var cloudAcct, credID, syncedAt, reachAt sql.NullInt64
	var missing int
	var created, updated int64

	err := r.Scan(
		&m.ID, &m.Name, &tags,
		&cloudAcct, &m.Provider, &m.InstanceID, &m.Region, &m.InstanceType,
		&power, &m.PublicIP, &m.PrivateIP, &syncedAt, &missing,
		&m.SSHHost, &m.SSHPort, &m.SSHUser, &credID,
		&reach, &m.ReachError, &reachAt,
		&created, &updated,
	)
	if err != nil {
		return m, err
	}

	m.Tags = splitTags(tags)
	m.CloudAccountID = ptrInt64(cloudAcct)
	m.CredentialID = ptrInt64(credID)
	m.PowerState = PowerState(power)
	m.ReachState = ReachState(reach)
	m.CloudSyncedAt = fromMsPtr(syncedAt)
	m.ReachCheckedAt = fromMsPtr(reachAt)
	m.Missing = missing != 0
	m.CreatedAt = fromMs(created)
	m.UpdatedAt = fromMs(updated)
	return m, nil
}

// --- host facts -------------------------------------------------------------

func (s *Store) UpsertFacts(f HostFacts) error {
	caps, err := json.Marshal(f.Capabilities)
	if err != nil {
		return fmt.Errorf("store: encode capabilities: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO host_facts (machine_id, hostname, os_name, os_version, kernel, arch,
		                        init_system, sudo_mode, capabilities, fetched_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(machine_id) DO UPDATE SET
			hostname=excluded.hostname, os_name=excluded.os_name, os_version=excluded.os_version,
			kernel=excluded.kernel, arch=excluded.arch, init_system=excluded.init_system,
			sudo_mode=excluded.sudo_mode, capabilities=excluded.capabilities,
			fetched_at=excluded.fetched_at`,
		f.MachineID, f.Hostname, f.OSName, f.OSVersion, f.Kernel, f.Arch,
		f.InitSystem, f.SudoMode, string(caps), ms(time.Now()),
	)
	return err
}

func (s *Store) GetFacts(machineID int64) (HostFacts, error) {
	var f HostFacts
	var caps string
	var fetched int64
	err := s.db.QueryRow(`
		SELECT machine_id, hostname, os_name, os_version, kernel, arch,
		       init_system, sudo_mode, capabilities, fetched_at
		FROM host_facts WHERE machine_id = ?`, machineID,
	).Scan(&f.MachineID, &f.Hostname, &f.OSName, &f.OSVersion, &f.Kernel, &f.Arch,
		&f.InitSystem, &f.SudoMode, &caps, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal([]byte(caps), &f.Capabilities); err != nil {
		f.Capabilities = nil
	}
	f.FetchedAt = fromMs(fetched)
	return f, nil
}

// --- host keys --------------------------------------------------------------

func (s *Store) GetHostKey(machineID int64) (HostKey, error) {
	var k HostKey
	var seen int64
	err := s.db.QueryRow(
		`SELECT machine_id, algorithm, fingerprint, first_seen, seen_algorithm, seen_fingerprint
		 FROM host_keys WHERE machine_id = ?`,
		machineID,
	).Scan(&k.MachineID, &k.Algorithm, &k.Fingerprint, &seen, &k.SeenAlgorithm, &k.SeenFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, err
	}
	k.FirstSeen = fromMs(seen)
	return k, nil
}

// PinHostKey records a host key on first sight. It does not overwrite: a
// changed key must be resolved explicitly via TrustHostKey.
func (s *Store) PinHostKey(machineID int64, algorithm, fingerprint string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO host_keys (machine_id, algorithm, fingerprint, first_seen) VALUES (?,?,?,?)`,
		machineID, algorithm, fingerprint, ms(time.Now()),
	)
	return err
}

// RecordSeenHostKey stores a key that failed to match the pin, so the UI can
// show the user exactly what changed before they decide.
func (s *Store) RecordSeenHostKey(machineID int64, algorithm, fingerprint string) error {
	_, err := s.db.Exec(
		`UPDATE host_keys SET seen_algorithm = ?, seen_fingerprint = ? WHERE machine_id = ?`,
		algorithm, fingerprint, machineID,
	)
	return err
}

// ClearSeenHostKey resets the mismatch record after a successful match.
func (s *Store) ClearSeenHostKey(machineID int64) error {
	_, err := s.db.Exec(
		`UPDATE host_keys SET seen_algorithm = '', seen_fingerprint = '' WHERE machine_id = ? AND seen_fingerprint != ''`,
		machineID,
	)
	return err
}

// TrustHostKey replaces a pinned key after the user has accepted the change,
// and clears the mismatch record.
func (s *Store) TrustHostKey(machineID int64, algorithm, fingerprint string) error {
	_, err := s.db.Exec(`
		INSERT INTO host_keys (machine_id, algorithm, fingerprint, first_seen, seen_algorithm, seen_fingerprint)
		VALUES (?,?,?,?,'','')
		ON CONFLICT(machine_id) DO UPDATE SET
			algorithm=excluded.algorithm, fingerprint=excluded.fingerprint, first_seen=excluded.first_seen,
			seen_algorithm='', seen_fingerprint=''`,
		machineID, algorithm, fingerprint, ms(time.Now()),
	)
	return err
}

// --- runs (audit log) -------------------------------------------------------

const maxStoredOutput = 64 * 1024 // keep the audit log from swallowing log tails

func (s *Store) InsertRun(r Run) (Run, error) {
	res, err := s.db.Exec(`
		INSERT INTO runs (machine_id, machine_name, action_id, command, danger, actor,
		                  exit_code, stdout, stderr, error, duration_ms, started_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullInt64(r.MachineID), r.MachineName, r.ActionID, r.Command, r.Danger, r.Actor,
		nullIntVal(r.ExitCode), truncate(r.Stdout, maxStoredOutput), truncate(r.Stderr, maxStoredOutput),
		r.Error, r.DurationMS, ms(r.StartedAt),
	)
	if err != nil {
		return r, fmt.Errorf("store: insert run: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	return r, nil
}

const runCols = `id, machine_id, machine_name, action_id, command, danger, actor,
                 exit_code, stdout, stderr, error, duration_ms, started_at`

// ListRuns returns the audit log newest first, optionally scoped to a machine.
func (s *Store) ListRuns(machineID *int64, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if machineID != nil {
		rows, err = s.db.Query(`SELECT `+runCols+` FROM runs WHERE machine_id = ? ORDER BY started_at DESC LIMIT ?`, *machineID, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+runCols+` FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	out := []Run{}
	for rows.Next() {
		var r Run
		var machID, exit sql.NullInt64
		var started int64
		if err := rows.Scan(&r.ID, &machID, &r.MachineName, &r.ActionID, &r.Command, &r.Danger,
			&r.Actor, &exit, &r.Stdout, &r.Stderr, &r.Error, &r.DurationMS, &started); err != nil {
			return nil, err
		}
		r.MachineID = ptrInt64(machID)
		r.ExitCode = ptrInt(exit)
		r.StartedAt = fromMs(started)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- small helpers ----------------------------------------------------------

func joinTags(tags []string) string { return strings.Join(tags, ",") }

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIntVal(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back off to a rune boundary so the stored text stays valid UTF-8.
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "\n… truncated"
}
