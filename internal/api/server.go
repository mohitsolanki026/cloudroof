// Package api is the HTTP surface: REST for everything, one websocket for the
// terminal, and the embedded frontend.
//
// This is also where secrets get unsealed — the keyring and the store meet
// here, in resolveTarget, and nowhere else.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"bosun/internal/actions"
	"bosun/internal/keyring"
	"bosun/internal/sshx"
	"bosun/internal/store"
)

type Server struct {
	store   *store.Store
	keys    *keyring.Keyring
	broker  *sshx.Broker
	engine  *actions.Engine
	log     *slog.Logger
	static  fs.FS // nil in dev mode
	devDist string

	// hosts is the Host-header allowlist (lower-case, no port). anyHost
	// disables the check for deployments that opted into it.
	hosts    map[string]bool
	anyHost  bool
	upgrader websocket.Upgrader

	// syncMu serializes cloud syncs so the timer and a manual sync cannot
	// race each other into duplicate machines.
	syncMu sync.Mutex
}

type Options struct {
	Store   *store.Store
	Keyring *keyring.Keyring
	Broker  *sshx.Broker
	Log     *slog.Logger
	// Static is the embedded frontend bundle. DevDist, when non-empty,
	// overrides it with a directory on disk.
	Static  fs.FS
	DevDist string
	// AllowedHosts are hostnames (optionally host:port) the UI is served on.
	// Loopback names are always allowed. AllowAnyHost skips the check.
	AllowedHosts []string
	AllowAnyHost bool
}

func New(o Options) *Server {
	s := &Server{
		store:   o.Store,
		keys:    o.Keyring,
		broker:  o.Broker,
		log:     o.Log,
		static:  o.Static,
		devDist: o.DevDist,
		hosts:   map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true},
		anyHost: o.AllowAnyHost,
	}
	for _, h := range o.AllowedHosts {
		s.hosts[hostOnly(h)] = true
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.originOK,
	}
	s.engine = actions.NewEngine(o.Broker, o.Store, s.resolveTarget)
	return s
}

// Handler builds the router. Go 1.22 pattern routing keeps this dependency-free.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Fleet
	mux.HandleFunc("GET /api/machines", s.listMachines)
	mux.HandleFunc("POST /api/machines", s.createMachine)
	mux.HandleFunc("GET /api/machines/{id}", s.getMachine)
	mux.HandleFunc("PUT /api/machines/{id}", s.updateMachine)
	mux.HandleFunc("DELETE /api/machines/{id}", s.deleteMachine)
	mux.HandleFunc("POST /api/machines/{id}/probe", s.probeMachine)
	mux.HandleFunc("POST /api/machines/{id}/power", s.powerMachine)
	mux.HandleFunc("GET /api/machines/{id}/actions", s.listActions)
	mux.HandleFunc("POST /api/machines/{id}/actions/{action}", s.runAction)
	mux.HandleFunc("GET /api/machines/{id}/hostkey", s.getHostKey)
	mux.HandleFunc("POST /api/machines/{id}/hostkey/trust", s.trustHostKey)
	mux.HandleFunc("GET /api/machines/{id}/terminal", s.terminal)

	// Credentials & accounts
	mux.HandleFunc("GET /api/credentials", s.listCredentials)
	mux.HandleFunc("POST /api/credentials", s.createCredential)
	mux.HandleFunc("DELETE /api/credentials/{id}", s.deleteCredential)
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/sync", s.syncAccount)

	// Audit & metadata
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/catalog", s.catalog)
	mux.HandleFunc("GET /api/providers", s.providers)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Everything else is the SPA.
	mux.HandleFunc("/", s.spa)

	return s.logRequests(s.guard(mux))
}

// resolveTarget is the only place a sealed credential becomes plaintext. The
// result lives on the stack of one request and in the broker's connection.
func (s *Server) resolveTarget(m store.Machine) (sshx.Target, error) {
	if m.CredentialID == nil {
		return sshx.Target{}, actions.ErrNoHost
	}
	cred, err := s.store.GetCredential(*m.CredentialID)
	if err != nil {
		return sshx.Target{}, fmt.Errorf("credential: %w", err)
	}
	plain, err := s.keys.OpenSealed(cred.Sealed)
	if err != nil {
		return sshx.Target{}, err
	}
	var secret sealedSecret
	if err := json.Unmarshal(plain, &secret); err != nil {
		return sshx.Target{}, fmt.Errorf("credential %d: malformed secret", cred.ID)
	}

	port := m.SSHPort
	if port == 0 {
		port = 22
	}
	return sshx.Target{
		ID:   m.ID,
		Addr: net.JoinHostPort(strings.Trim(m.SSHHost, "[]"), strconv.Itoa(port)),
		User: m.SSHUser,
		Auth: sshx.Auth{
			PrivateKey: []byte(secret.PrivateKey),
			Passphrase: []byte(secret.Passphrase),
			Password:   secret.Password,
		},
		HostKey: s.hostKeyPolicy(m.ID),
	}, nil
}

// sealedSecret is the JSON shape inside every credential blob. One shape for
// both kinds keeps the keyring ignorant of what it's protecting.
type sealedSecret struct {
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Password   string `json:"password,omitempty"`
}

// hostKeyPolicy pins on first sight and refuses on change. A changed key
// blocks every action on the machine until the user explicitly trusts it via
// the hostkey/trust endpoint.
func (s *Server) hostKeyPolicy(machineID int64) sshx.HostKeyPolicy {
	return sshx.HostKeyPolicyFunc(func(_ string, key ssh.PublicKey) error {
		fp := sshx.Fingerprint(key)
		pinned, err := s.store.GetHostKey(machineID)
		if errors.Is(err, store.ErrNotFound) {
			return s.store.PinHostKey(machineID, key.Type(), fp)
		}
		if err != nil {
			return err
		}
		if pinned.Fingerprint != fp {
			// Record what we saw so the UI can show old vs. new. The error
			// itself still refuses the connection.
			_ = s.store.RecordSeenHostKey(machineID, key.Type(), fp)
			return &sshx.HostKeyMismatchError{Algorithm: key.Type(), Fingerprint: fp}
		}
		if pinned.SeenFingerprint != "" {
			_ = s.store.ClearSeenHostKey(machineID)
		}
		return nil
	})
}

// --- static / spa -----------------------------------------------------------

func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	var root fs.FS
	if s.devDist != "" {
		root = os.DirFS(s.devDist)
	} else {
		root = s.static
	}
	if root == nil {
		http.Error(w, "frontend not built — run `make web` or start with -dev", 503)
		return
	}

	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if f, err := root.Open(p); err == nil {
		f.Close()
		http.ServeFileFS(w, r, root, p)
		return
	}
	// Client-side routes fall back to the shell.
	http.ServeFileFS(w, r, root, "index.html")
}

// --- middleware & helpers ---------------------------------------------------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// guard is the browser-side defense for an API that has no login yet. It
// refuses three things on every /api route:
//
//   - a Host header that is not this server (DNS rebinding: an attacker's
//     page resolves its own name to 127.0.0.1 and the browser happily sends
//     Host: attacker.example);
//   - an Origin or Sec-Fetch-Site that says the request came from another
//     site (cross-site request forgery from any page the admin visits);
//   - a state-changing request whose body is not application/json, which
//     is what forces a browser to preflight a cross-site call at all.
//
// Non-browser clients (curl, the e2e script) send none of these headers and
// pass. None of this replaces authentication; it keeps the loopback-bound
// default honest until v2.5 adds users.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hostAllowed(r.Host) {
			writeJSON(w, 421, apiError{
				Error: fmt.Sprintf("bosun is not configured to serve on host %q — start it with -hosts %s", r.Host, hostOnly(r.Host)),
				Code:  "bad_host",
			})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originMatches(origin, r.Host) {
			writeJSON(w, 403, apiError{Error: "cross-origin request refused", Code: "forbidden_origin"})
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeJSON(w, 403, apiError{Error: "cross-site request refused", Code: "forbidden_origin"})
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.ContentLength != 0 && !isJSON(r.Header.Get("Content-Type")) {
				writeJSON(w, 415, apiError{Error: "request body must be application/json", Code: "bad_request"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed checks a Host header (or an Origin's host) against the allowlist.
func (s *Server) hostAllowed(host string) bool {
	if s.anyHost {
		return true
	}
	return s.hosts[hostOnly(host)]
}

// originMatches requires the Origin to name this server AND to be the host
// the request arrived on — same-origin, not merely an allowed origin.
func (s *Server) originMatches(origin, reqHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return s.hostAllowed(u.Host) && strings.EqualFold(u.Host, reqHost)
}

// originOK is the websocket upgrader's check; same rule as guard.
func (s *Server) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return s.hostAllowed(r.Host)
	}
	return s.originMatches(origin, r.Host)
}

// hostOnly lower-cases and strips the port and IPv6 brackets from a host.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func isJSON(contentType string) bool {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	return mt == "application/json"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type apiError struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

// fail maps domain errors to status codes and a stable code string the
// frontend can switch on (e.g. to open the confirm dialog).
func fail(w http.ResponseWriter, err error) {
	var status int
	var code string
	var details any
	var hk *sshx.HostKeyMismatchError
	if errors.As(err, &hk) {
		details = map[string]string{"algorithm": hk.Algorithm, "fingerprint": hk.Fingerprint}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, actions.ErrNeedsConfirm):
		status, code = 428, "needs_confirm"
	case errors.Is(err, actions.ErrNeedsName):
		status, code = 428, "needs_name"
	case errors.Is(err, actions.ErrForbidden):
		status, code = 403, "forbidden"
	case errors.Is(err, actions.ErrBadParam), errors.Is(err, actions.ErrUnknownAction):
		status, code = 400, "bad_request"
	case errors.Is(err, actions.ErrNotAvailable), errors.Is(err, actions.ErrNoHost),
		errors.Is(err, actions.ErrNoFacts), errors.Is(err, actions.ErrSudoUnavailable):
		status, code = 409, "not_available"
	case errors.Is(err, sshx.ErrHostKeyMismatch):
		status, code = 409, "hostkey_changed"
	case errors.Is(err, sshx.ErrAuth):
		status, code = 502, "auth_failed"
	case errors.Is(err, sshx.ErrTimeout), errors.Is(err, sshx.ErrRefused), errors.Is(err, sshx.ErrNoRoute):
		status, code = 502, "unreachable"
	default:
		status, code = 500, "internal"
	}
	writeJSON(w, status, apiError{Error: err.Error(), Code: code, Details: details})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, 400, apiError{Error: msg, Code: "bad_request"})
}
