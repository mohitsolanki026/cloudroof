// Package config resolves runtime configuration from flags, environment, and
// sensible defaults. Bosun is self-hosted and expected to run with zero
// configuration, so every value here has a working default.
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	// Addr is the listen address for the HTTP API and UI.
	Addr string
	// DataDir holds the SQLite database and the master key file.
	DataDir string
	// MasterKeyPath is the file containing the 32-byte secret-sealing key.
	// Generated on first run if absent.
	MasterKeyPath string
	// MasterKeyEnv, when set, overrides the key file. Intended for container
	// users who inject secrets as environment variables.
	MasterKeyEnv string
	// Dev serves the frontend from disk instead of the embedded bundle.
	Dev bool
	// Hosts is the Host-header allowlist: names the UI is reached on besides
	// loopback. Empty is fine for the loopback default; a network bind
	// without it accepts any Host (and says so in the log).
	Hosts []string
}

// BindsLoopback reports whether Addr listens only on a loopback address.
func (c Config) BindsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "bosun.db") }

// Load parses flags and environment into a Config, creating the data directory
// if it does not exist.
func Load(args []string) (Config, error) {
	fs := flag.NewFlagSet("bosun", flag.ContinueOnError)

	var c Config
	var hosts string
	// Loopback by default: the API has no authentication in v0.1, so binding
	// every interface would hand root-capable actions to the whole network.
	// Exposing it is an explicit opt-in (-addr 0.0.0.0:7070 or a LAN IP),
	// ideally behind a reverse proxy that adds auth and TLS.
	fs.StringVar(&c.Addr, "addr", envOr("BOSUN_ADDR", "127.0.0.1:7070"), "listen address (loopback by default)")
	fs.StringVar(&hosts, "hosts", envOr("BOSUN_HOSTS", ""), "comma-separated hostnames the UI is reached on, e.g. bosun.lan,10.0.0.5 (Host-header allowlist; loopback is always allowed)")
	fs.StringVar(&c.DataDir, "data", envOr("BOSUN_DATA", defaultDataDir()), "data directory")
	fs.BoolVar(&c.Dev, "dev", os.Getenv("BOSUN_DEV") != "", "serve frontend from ./web/dist instead of embedded bundle")

	if err := fs.Parse(args); err != nil {
		return c, err
	}

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return c, fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	c.MasterKeyPath = filepath.Join(c.DataDir, "master.key")
	c.MasterKeyEnv = os.Getenv("BOSUN_MASTER_KEY")
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.Hosts = append(c.Hosts, h)
		}
	}

	// 0700: the data directory holds sealed SSH keys and the master key beside
	// each other. Group and world have no business here.
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return c, fmt.Errorf("create data dir %s: %w", c.DataDir, err)
	}
	return c, nil
}

func defaultDataDir() string {
	// Prefer XDG, fall back to a dot-directory, fall back to the working
	// directory. The last case covers containers running without HOME set.
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "bosun")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".bosun")
	}
	return "./bosun-data"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
