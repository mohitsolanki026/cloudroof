// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloudroof/internal/sshx"
	"cloudroof/internal/store"
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

// lookup resolves an action id against the built-in catalog first, then the
// admin's saved custom actions. Built-ins always win a name clash.
func (e *Engine) lookup(id string) (Action, bool) {
	if a, ok := Lookup(id); ok {
		return a, true
	}
	ca, err := e.store.GetCustomAction(id)
	if err != nil {
		return Action{}, false
	}
	return FromCustom(ca), true
}

// Resolve exposes action lookup (built-in + custom) to the API layer — e.g.
// bulk needs an action's danger tier to gate before it runs anything.
func (e *Engine) Resolve(id string) (Action, bool) { return e.lookup(id) }

// FromCustom converts a stored custom action into an executable Action. Custom
// params carry no regex (Pattern stays nil); render() falls back to a safe
// free-text check for them. Output is always shown raw.
func FromCustom(ca store.CustomAction) Action {
	params := make([]Param, 0, len(ca.Params))
	for _, p := range ca.Params {
		params = append(params, Param{Name: p.Name, Label: p.Label})
	}
	cat := ca.Category
	if cat == "" {
		cat = "custom"
	}
	req := ca.Requires
	if req == nil {
		req = []string{}
	}
	return Action{
		ID: ca.ID, Label: ca.Label, Category: cat,
		Requires: req, Command: ca.Command, Params: params,
		Danger: ca.Danger, Sudo: Sudo(ca.Sudo), Parse: "raw",
	}
}

// Prepared is the resolved, gated, host-checked form of a request: what to
// run, how, and where. Both Run (buffered) and the streaming handler build one
// so the gate, the requires-check, the sudo decision, and the quoting live in
// exactly one place.
type Prepared struct {
	Action  Action
	Machine store.Machine
	Facts   store.HostFacts
	Target  sshx.Target
	// Shell is what Exec/OpenStream actually runs: "sh -s", possibly with a
	// sudo prefix. Script is fed to its stdin.
	Shell  string
	Script string
	// Command is the display and audit form: the rendered command with its
	// sudo prefix, never the `sh -s` transport.
	Command string
}

// Prepare validates a request against the machine and its facts and resolves
// everything needed to execute it, without touching the host except to unseal
// the credential. It does not run anything and writes no audit row.
func (e *Engine) Prepare(req Request) (Prepared, error) {
	act, ok := e.lookup(req.ActionID)
	if !ok {
		return Prepared{}, ErrUnknownAction
	}
	m, err := e.store.GetMachine(req.MachineID)
	if err != nil {
		return Prepared{}, err
	}
	if !m.HasHost() {
		return Prepared{}, ErrNoHost
	}

	// Validate the request before consulting host state: a missing
	// confirmation or a bad parameter is wrong regardless of what the host
	// looks like, and the caller should hear that specific reason.
	if err := gate(act, m, req); err != nil {
		return Prepared{}, err
	}
	command, err := render(act, req.Params)
	if err != nil {
		return Prepared{}, err
	}

	// The overview probe is the one action that may run before facts exist,
	// because the UI runs fingerprinting and overview back-to-back.
	facts, err := e.store.GetFacts(m.ID)
	if err != nil && !(errors.Is(err, store.ErrNotFound) && act.ID == "system.overview") {
		if errors.Is(err, store.ErrNotFound) {
			return Prepared{}, ErrNoFacts
		}
		return Prepared{}, err
	}
	for _, r := range act.Requires {
		if !facts.Has(r) {
			return Prepared{}, fmt.Errorf("%w: needs %s", ErrNotAvailable, r)
		}
	}

	// Every action runs inside `sh -s` with the rendered command on stdin —
	// Script actions and single commands alike. That makes the remote login
	// shell irrelevant (fish, zsh, a restricted shell), lets a sudo prefix
	// cover a whole `a || b` chain rather than only its first word, and keeps
	// quoting to the one place render() already handles. The audit log and
	// the UI preview show the command itself; `sh -s` is the transport.
	shell, err := applySudo(act.Sudo, facts.SudoMode, "sh -s")
	if err != nil {
		return Prepared{}, err
	}
	script := act.Script
	if script == "" {
		script = command + "\n"
		command, _ = applySudo(act.Sudo, facts.SudoMode, command) // display and audit form
	}

	target, err := e.resolve(m)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Action: act, Machine: m, Facts: facts, Target: target,
		Shell: shell, Script: script, Command: command,
	}, nil
}

// Run executes one action end to end: prepare, exec, parse, audit.
// Every path that reaches the host — including failures — writes a Run.
func (e *Engine) Run(ctx context.Context, req Request) (Result, error) {
	p, err := e.Prepare(req)
	if err != nil {
		return Result{}, err
	}
	act, m := p.Action, p.Machine

	opts := sshx.ExecOpts{Timeout: act.Timeout, Stdin: strings.NewReader(p.Script)}

	run := store.Run{
		MachineID:   &m.ID,
		MachineName: m.Name,
		ActionID:    act.ID,
		Command:     p.Command,
		Danger:      act.Danger,
		Actor:       req.Actor,
		StartedAt:   time.Now().UTC(),
	}

	res, execErr := e.broker.Exec(ctx, p.Target, p.Shell, opts)
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
		ph := "{{" + p.Name + "}}"
		// Author sanity: a declared param must appear in the template. (A
		// blanket "no {{ left" check can't be used — some commands carry
		// literal Go-template braces, e.g. docker --format '{{json .}}'.)
		if !strings.Contains(cmd, ph) {
			return "", fmt.Errorf("%w: param %s is not used by %s", ErrBadParam, p.Name, act.ID)
		}
		v, ok := params[p.Name]
		if !ok || v == "" {
			return "", fmt.Errorf("%w: %s is required", ErrBadParam, p.Name)
		}
		if p.Pattern != nil {
			// Built-in param: an allowlist regex.
			if !p.Pattern.MatchString(v) {
				return "", fmt.Errorf("%w: %s=%q", ErrBadParam, p.Name, v)
			}
		} else if err := safeFreeParam(v); err != nil {
			// Custom-action param: no author-supplied regex, so only reject
			// what shell-quoting can't neutralize (control chars) or what is
			// abusive (length). The single-quote wrapping below makes every
			// remaining byte, quotes included, inert to the shell.
			return "", fmt.Errorf("%w: %s: %v", ErrBadParam, p.Name, err)
		}
		cmd = strings.ReplaceAll(cmd, ph, shellQuote(v))
	}
	return cmd, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// safeFreeParam validates a custom-action param value, which has no allowlist
// regex. Shell-quoting handles injection; this only bars control characters
// (which quoting does not neutralize) and caps the length.
func safeFreeParam(v string) error {
	if len(v) > 512 {
		return fmt.Errorf("value too long (max 512)")
	}
	for _, r := range v {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return fmt.Errorf("value contains a control character")
		}
	}
	return nil
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
