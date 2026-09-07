package store

// migrations are applied in order and recorded in schema_migrations. Never edit
// a migration that has shipped; append a new one instead.
var migrations = []string{
	// 001 — credentials, cloud accounts, and the machine join.
	`
	CREATE TABLE credentials (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT    NOT NULL,
		kind         TEXT    NOT NULL,              -- ssh_key | password
		sealed       TEXT    NOT NULL,              -- keyring blob, never plaintext
		fingerprint  TEXT    NOT NULL DEFAULT '',   -- SHA256:... for display only
		created_at   INTEGER NOT NULL
	);

	CREATE TABLE cloud_accounts (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		name            TEXT    NOT NULL,
		provider        TEXT    NOT NULL,           -- hetzner | digitalocean | aws
		sealed_token    TEXT    NOT NULL,
		last_sync_at    INTEGER,
		last_sync_error TEXT    NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL
	);

	-- A machine has two identities and either half may be absent:
	--   cloud identity  -> provider, instance_id, power_state
	--   host identity   -> ssh_host, ssh_user, credential_id
	-- Both present is the linked case the product is built around.
	CREATE TABLE machines (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		name             TEXT    NOT NULL,
		tags             TEXT    NOT NULL DEFAULT '',

		cloud_account_id INTEGER REFERENCES cloud_accounts(id) ON DELETE SET NULL,
		provider         TEXT    NOT NULL DEFAULT '',
		instance_id      TEXT    NOT NULL DEFAULT '',
		region           TEXT    NOT NULL DEFAULT '',
		instance_type    TEXT    NOT NULL DEFAULT '',
		power_state      TEXT    NOT NULL DEFAULT 'unknown',
		public_ip        TEXT    NOT NULL DEFAULT '',
		private_ip       TEXT    NOT NULL DEFAULT '',
		cloud_synced_at  INTEGER,
		missing          INTEGER NOT NULL DEFAULT 0,  -- sync no longer sees it

		ssh_host         TEXT    NOT NULL DEFAULT '',
		ssh_port         INTEGER NOT NULL DEFAULT 22,
		ssh_user         TEXT    NOT NULL DEFAULT '',
		credential_id    INTEGER REFERENCES credentials(id) ON DELETE SET NULL,

		-- Reachability is deliberately independent of power_state. The pair
		-- (running, unreachable) is the most useful signal in the product.
		reach_state      TEXT    NOT NULL DEFAULT 'unknown',
		reach_error      TEXT    NOT NULL DEFAULT '',
		reach_checked_at INTEGER,

		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	);

	CREATE UNIQUE INDEX idx_machines_instance
		ON machines(cloud_account_id, instance_id)
		WHERE instance_id != '';

	CREATE TABLE host_facts (
		machine_id   INTEGER PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
		hostname     TEXT    NOT NULL DEFAULT '',
		os_name      TEXT    NOT NULL DEFAULT '',
		os_version   TEXT    NOT NULL DEFAULT '',
		kernel       TEXT    NOT NULL DEFAULT '',
		arch         TEXT    NOT NULL DEFAULT '',
		init_system  TEXT    NOT NULL DEFAULT 'unknown',
		sudo_mode    TEXT    NOT NULL DEFAULT 'unknown',
		capabilities TEXT    NOT NULL DEFAULT '[]',  -- JSON array
		fetched_at   INTEGER NOT NULL
	);

	CREATE TABLE host_keys (
		machine_id  INTEGER PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
		algorithm   TEXT    NOT NULL,
		fingerprint TEXT    NOT NULL,
		first_seen  INTEGER NOT NULL
	);

	-- Audit log. machine_id is SET NULL rather than CASCADE and the name is
	-- denormalized: deleting a machine must never erase the record of what was
	-- done to it.
	CREATE TABLE runs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		machine_id   INTEGER REFERENCES machines(id) ON DELETE SET NULL,
		machine_name TEXT    NOT NULL,
		action_id    TEXT    NOT NULL,
		command      TEXT    NOT NULL,
		danger       INTEGER NOT NULL DEFAULT 0,
		actor        TEXT    NOT NULL DEFAULT 'admin',
		exit_code    INTEGER,
		stdout       TEXT    NOT NULL DEFAULT '',
		stderr       TEXT    NOT NULL DEFAULT '',
		error        TEXT    NOT NULL DEFAULT '',
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		started_at   INTEGER NOT NULL
	);

	CREATE INDEX idx_runs_started ON runs(started_at DESC);
	CREATE INDEX idx_runs_machine ON runs(machine_id, started_at DESC);
	`,

	// 002 — record the last host key observed that did NOT match the pin, so
	// the UI can show pinned vs. seen and the trust endpoint can require the
	// user to echo the seen fingerprint back. (These columns were originally
	// added by editing 001 in place, which never reached databases created
	// before the edit; this migration is the correct, append-only fix.)
	`
	ALTER TABLE host_keys ADD COLUMN seen_algorithm   TEXT NOT NULL DEFAULT '';
	ALTER TABLE host_keys ADD COLUMN seen_fingerprint TEXT NOT NULL DEFAULT '';
	`,

	// 003 — v1.5 "Fleet": tag-based groups that target bulk actions, and
	// admin-authored saved custom actions.
	`
	-- A group is a set of tags. A machine belongs when it carries ALL of
	-- them. Membership is resolved live from machines.tags, never stored, so
	-- it stays correct as the fleet changes.
	CREATE TABLE groups (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT    NOT NULL,
		tags       TEXT    NOT NULL DEFAULT '',   -- comma-separated, ALL must match
		created_at INTEGER NOT NULL
	);

	-- A custom action is a curated command the admin saved. It carries the
	-- same shape as a built-in: a danger tier the gate enforces identically,
	-- optional capability requirements, and templated params. danger is
	-- constrained to 0..2 — a Tier-3 button never exists, built-in or custom.
	CREATE TABLE custom_actions (
		id         TEXT    PRIMARY KEY,           -- "custom.<slug>"
		label      TEXT    NOT NULL,
		category   TEXT    NOT NULL DEFAULT 'custom',
		command    TEXT    NOT NULL,
		params     TEXT    NOT NULL DEFAULT '[]', -- JSON [{name,label}]
		requires   TEXT    NOT NULL DEFAULT '[]', -- JSON [capability]
		sudo       TEXT    NOT NULL DEFAULT '',   -- '' | preferred | required
		danger     INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	`,
}
