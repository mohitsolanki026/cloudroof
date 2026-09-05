package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// The seen_* columns must exist end to end after a normal Open.
func TestMigrationRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	m, err := s.CreateMachine(Machine{Name: "x", SSHHost: "h", SSHUser: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PinHostKey(m.ID, "ssh-ed25519", "SHA256:aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSeenHostKey(m.ID, "ssh-ed25519", "SHA256:bbb"); err != nil {
		t.Fatal(err)
	}
	k, err := s.GetHostKey(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k.Fingerprint != "SHA256:aaa" || k.SeenFingerprint != "SHA256:bbb" {
		t.Fatalf("host key round-trip wrong: %+v", k)
	}
}

// Reproduces the reported bug: a database created by the original v0.1
// migration 001 (host_keys without seen_*), opened by the current binary,
// must gain the columns via migration 002 rather than fail with
// "no such column: seen_algorithm".
func TestUpgradeFromV01Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db := openRaw(t, path)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	// host_keys as it shipped in v0.1 — no seen_* columns.
	mustExec(t, db, `CREATE TABLE host_keys (
		machine_id  INTEGER PRIMARY KEY,
		algorithm   TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		first_seen  INTEGER NOT NULL
	)`)
	mustExec(t, db, `INSERT INTO host_keys VALUES (1, 'ssh-ed25519', 'SHA256:old', 0)`)
	mustExec(t, db, `INSERT INTO schema_migrations VALUES (1, 0)`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v0.1 database must not fail: %v", err)
	}
	defer s.Close()

	k, err := s.GetHostKey(1)
	if err != nil {
		t.Fatalf("GetHostKey after upgrade: %v", err)
	}
	if k.Fingerprint != "SHA256:old" || k.SeenFingerprint != "" {
		t.Fatalf("existing pin not preserved: %+v", k)
	}
	// The seen columns are now writable.
	if err := s.RecordSeenHostKey(1, "ssh-ed25519", "SHA256:new"); err != nil {
		t.Fatalf("seen columns missing after upgrade: %v", err)
	}
}

// A database from the brief window where the columns lived inline in 001 must
// still upgrade cleanly: migration 002's ADD COLUMN is tolerated as a no-op.
func TestUpgradeToleratesExistingColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inline.db")
	db := openRaw(t, path)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	mustExec(t, db, `CREATE TABLE host_keys (
		machine_id       INTEGER PRIMARY KEY,
		algorithm        TEXT NOT NULL,
		fingerprint      TEXT NOT NULL,
		first_seen       INTEGER NOT NULL,
		seen_algorithm   TEXT NOT NULL DEFAULT '',
		seen_fingerprint TEXT NOT NULL DEFAULT ''
	)`)
	mustExec(t, db, `INSERT INTO schema_migrations VALUES (1, 0)`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("duplicate-column ADD must be tolerated: %v", err)
	}
	s.Close()
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("setup exec failed: %v\n%s", err, q)
	}
}

// A cloud instance whose public IP changes across a stop/start must have
// ssh_host follow the new IP when it was tracking the old one — but a
// user-pinned ssh_host (a DNS name, bastion, or Elastic IP) must be left alone.
func TestUpsertFollowsPublicIP(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	acct, err := s.CreateCloudAccount(CloudAccount{Name: "aws", Provider: "aws", SealedToken: "x"})
	if err != nil {
		t.Fatal(err)
	}

	// First sync: instance at 1.1.1.1 → ssh_host auto-filled to 1.1.1.1.
	id, created, hostChanged, err := s.UpsertFromCloud(acct.ID, "aws", CloudInstance{
		InstanceID: "us-east-1:i-abc", Name: "web", PublicIP: "1.1.1.1", PowerState: PowerRunning,
	})
	if err != nil || !created || hostChanged {
		t.Fatalf("create: created=%v hostChanged=%v err=%v", created, hostChanged, err)
	}
	m, _ := s.GetMachine(id)
	if m.SSHHost != "1.1.1.1" {
		t.Fatalf("ssh_host not pre-filled: %q", m.SSHHost)
	}

	// Stop/start: same instance, new IP. ssh_host was tracking, so it follows.
	_, _, hostChanged, err = s.UpsertFromCloud(acct.ID, "aws", CloudInstance{
		InstanceID: "us-east-1:i-abc", Name: "web", PublicIP: "2.2.2.2", PowerState: PowerRunning,
	})
	if err != nil || !hostChanged {
		t.Fatalf("IP change should report hostChanged: %v %v", hostChanged, err)
	}
	m, _ = s.GetMachine(id)
	if m.SSHHost != "2.2.2.2" || m.PublicIP != "2.2.2.2" {
		t.Fatalf("ssh_host did not follow IP: ssh=%q ip=%q", m.SSHHost, m.PublicIP)
	}

	// Now the user pins a real name. A later IP change must NOT clobber it.
	m.SSHHost = "web.example.com"
	if err := s.UpdateMachine(m); err != nil {
		t.Fatal(err)
	}
	_, _, hostChanged, err = s.UpsertFromCloud(acct.ID, "aws", CloudInstance{
		InstanceID: "us-east-1:i-abc", Name: "web", PublicIP: "3.3.3.3", PowerState: PowerRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hostChanged {
		t.Fatal("a user-pinned ssh_host must not be reported as changed")
	}
	m, _ = s.GetMachine(id)
	if m.SSHHost != "web.example.com" {
		t.Fatalf("user-pinned ssh_host was clobbered: %q", m.SSHHost)
	}
	if m.PublicIP != "3.3.3.3" {
		t.Fatalf("public_ip should still refresh: %q", m.PublicIP)
	}
}
