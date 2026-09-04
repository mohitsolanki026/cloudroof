package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bosun/internal/sshx"
	"bosun/internal/store"
)

// Request is one execution ask from the API layer.
type Request struct {
	MachineID int64
	ActionID  string
	Params    map[string]string
	// Confirm satisfies Tier 1. ConfirmName satisfies Tier 2 by matching the
	// machine's name exactly. Tier 3 cannot be satisfied.
	Confirm     bool
	ConfirmName string
	Actor       string
}

// Result pairs the audit record with the parsed output.
type Result struct {
	Run  store.Run `json:"run"`
	Data any       `json:"data"`
}

// Typed refusals let the API return the right status and a message the user
// can act on.
var (
	ErrUnknownAction   = errors.New("unknown action")
	ErrNotAvailable    = errors.New("action not available on this host")
	ErrBadParam        = errors.New("invalid parameter")
	ErrNeedsConfirm    = errors.New("action requires confirmation")
	ErrNeedsName       = errors.New("action requires typing the machine name to confirm")
	ErrForbidden       = errors.New("action is terminal-only and cannot be run as a button")
	ErrNoHost          = errors.New("machine has no SSH host configured")
	ErrSudoUnavailable = errors.New("action needs root, and sudo on this host requires a password")
	ErrNoFacts         = errors.New("host has not been fingerprinted yet")
)

// TargetResolver turns a stored machine into a dial-able target by unsealing
// its credential and attaching a host-key policy. It lives in the API layer
// because that's where the keyring and store meet; the engine stays ignorant
// of secrets.
type TargetResolver func(m store.Machine) (sshx.Target, error)

type Engine struct {
	broker  *sshx.Broker
	store   *store.Store
	resolve TargetResolver
}

func NewEngine(b *sshx.Broker, s *store.Store, r TargetResolver) *Engine {
	return &Engine{broker: b, store: s, resolve: r}
}

// Run executes one action end to end: gate, render, exec, parse, audit.
// Every path that reaches the host — including failures — writes a Run.
func (e *Engine) Run(ctx context.Context, req Request) (Result, error) {
	act, ok := Lookup(req.ActionID)
	if !ok {
		return Result{}, ErrUnknownAction
	}

	m, err := e.store.GetMachine(req.MachineID)
	if err != nil {
		return Result{}, err
	}
	if !m.HasHost() {
		return Result{}, ErrNoHost
	}

	// Validate the request before consulting host state: a missing
	// confirmation or a bad parameter is wrong regardless of what the host
	// looks like, and the caller should hear that specific reason.
	if err := gate(act, m, req); err != nil {
		return Result{}, err
	}
	command, err := render(act, req.Params)
	if err != nil {
		return Result{}, err
	}

	// The overview probe is the one action that may run before facts exist,
	// because the UI runs fingerprinting and overview back-to-back.
	facts, err := e.store.GetFacts(m.ID)
	if err != nil && !(errors.Is(err, store.ErrNotFound) && act.ID == "system.overview") {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, ErrNoFacts
		}
		return Result{}, err
	}
	for _, r := range act.Requires {
		if !facts.Has(r) {
			return Result{}, fmt.Errorf("%w: needs %s", ErrNotAvailable, r)
		}
	}

	command, err = applySudo(act.Sudo, facts.SudoMode, command)
	if err != nil {
		return Result{}, err
	}

	target, err := e.resolve(m)
	if err != nil {
		return Result{}, err
	}

	opts := sshx.ExecOpts{Timeout: act.Timeout}
	if act.Script != "" {
		opts.Stdin = strings.NewReader(act.Script)
	}

	run := store.Run{
		MachineID:   &m.ID,
		MachineName: m.Name,
		ActionID:    act.ID,
		Command:     command,
		Danger:      act.Danger,
		Actor:       req.Actor,
		StartedAt:   time.Now().UTC(),
	}

	res, execErr := e.broker.Exec(ctx, target, command, opts)
	run.Stdout = res.Stdout
	run.Stderr = res.Stderr
	run.DurationMS = res.Duration.Milliseconds()
	if execErr != nil {
		run.Error = execErr.Error()
	} else {
		code := res.ExitCode
		run.ExitCode = &code
	}

	// Audit before returning, regardless of outcome. If the audit write
	// itself fails, that is the error the caller sees — an unrecorded action
	// is worse than a failed one.
	saved, err := e.store.InsertRun(run)
	if err != nil {
		return Result{}, err
	}
	if execErr != nil {
		return Result{Run: saved}, execErr
	}

	var data any
	if p, ok := parsers[act.Parse]; ok && act.Parse != "" {
		data = p(res.Stdout, res.ExitCode)
	} else {
		data = parseRaw(res.Stdout, res.ExitCode)
	}
	return Result{Run: saved, Data: data}, nil
}

// gate enforces the danger tier. It is intentionally simple and intentionally
// not configurable.
func gate(act Action, m store.Machine, req Request) error {
	switch act.Danger {
	case DangerRead:
		return nil
	case DangerReversible:
		if !req.Confirm {
			return ErrNeedsConfirm
		}
		return nil
	case DangerDisruptive:
		if req.ConfirmName != m.Name {
			return ErrNeedsName
		}
		return nil
	default:
		return ErrForbidden
	}
}

// render substitutes params into the command template. Every value must match
// its param's allowlist pattern and is then single-quoted, so even a pattern
// that turned out too loose cannot break out of the argument.
func render(act Action, params map[string]string) (string, error) {
	if act.Script != "" {
		return "sh -s", nil
	}
	cmd := act.Command
	for _, p := range act.Params {
		v, ok := params[p.Name]
		if !ok || v == "" {
			return "", fmt.Errorf("%w: %s is required", ErrBadParam, p.Name)
		}
		if !p.Pattern.MatchString(v) {
			return "", fmt.Errorf("%w: %s=%q", ErrBadParam, p.Name, v)
		}
		cmd = strings.ReplaceAll(cmd, "{{"+p.Name+"}}", shellQuote(v))
	}
	if strings.Contains(cmd, "{{") {
		return "", fmt.Errorf("%w: unresolved placeholder in %s", ErrBadParam, act.ID)
	}
	return cmd, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// applySudo decides whether to prefix `sudo -n` given what the action wants
// and what the host offers. sudoMode comes from facts: root | nopasswd |
// password | none | unknown. The `-n` is what guarantees nothing ever waits
// on a password prompt.
func applySudo(want Sudo, sudoMode, command string) (string, error) {
	if sudoMode == "root" || want == SudoNone {
		return command, nil
	}
	switch want {
	case SudoRequired:
		if sudoMode != "nopasswd" {
			return "", ErrSudoUnavailable
		}
		return "sudo -n " + command, nil
	case SudoPreferred:
		if sudoMode == "nopasswd" {
			return "sudo -n " + command, nil
		}
		return command, nil
	}
	return command, nil
}
