// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package sshx is the connection broker: it owns one pooled SSH client per
// machine and multiplexes both non-interactive exec (for actions) and PTY
// sessions (for the terminal) over it.
//
// It knows nothing about the database. Callers hand it a Target with plaintext
// auth material that has already been unsealed; the plaintext lives only for
// the life of the connection.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Typed errors let the caller map a failure to a ReachState without string
// matching.
var (
	ErrAuth            = errors.New("ssh: authentication failed")
	ErrHostKeyMismatch = errors.New("ssh: host key changed")
	ErrTimeout         = errors.New("ssh: connection timed out")
	ErrRefused         = errors.New("ssh: connection refused")
	ErrNoRoute         = errors.New("ssh: host unreachable")
)

// HostKeyMismatchError carries the observed key so the UI can show the user
// what changed before they decide to trust it.
type HostKeyMismatchError struct {
	Algorithm   string
	Fingerprint string
}

func (e *HostKeyMismatchError) Error() string        { return ErrHostKeyMismatch.Error() }
func (e *HostKeyMismatchError) Is(target error) bool { return target == ErrHostKeyMismatch }

// HostKeyPolicy decides whether an observed host key is acceptable.
//
// Return nil to accept. Implementations back this with the pinned key in the
// store: accept-and-pin on first sight, reject on mismatch.
type HostKeyPolicy interface {
	Check(addr string, key ssh.PublicKey) error
}

// HostKeyPolicyFunc adapts a function to HostKeyPolicy.
type HostKeyPolicyFunc func(addr string, key ssh.PublicKey) error

func (f HostKeyPolicyFunc) Check(addr string, key ssh.PublicKey) error { return f(addr, key) }

// Auth is the unsealed credential for one connection.
type Auth struct {
	PrivateKey []byte // PEM; used when non-empty
	Passphrase []byte // optional, for encrypted keys
	Password   string // used when PrivateKey is empty
}

// Target is everything needed to reach one machine.
type Target struct {
	ID      int64  // caller's machine ID; keys the pool
	Addr    string // host:port
	User    string
	Auth    Auth
	HostKey HostKeyPolicy
}

const (
	dialTimeout    = 10 * time.Second
	keepaliveTick  = 30 * time.Second
	keepaliveReply = 10 * time.Second
	shellSetup     = 30 * time.Second
)

// Broker holds live connections. Connections are dialed lazily and dropped on
// any error; the next request redials.
type Broker struct {
	mu    sync.Mutex
	conns map[int64]*conn
}

type conn struct {
	client *ssh.Client
	done   chan struct{}
}

func NewBroker() *Broker {
	return &Broker{conns: make(map[int64]*conn)}
}

// Get returns a live client for the target, dialing if necessary.
func (b *Broker) Get(ctx context.Context, t Target) (*ssh.Client, error) {
	b.mu.Lock()
	if c, ok := b.conns[t.ID]; ok {
		b.mu.Unlock()
		return c.client, nil
	}
	b.mu.Unlock()

	client, err := dial(ctx, t)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	// Another goroutine may have won the race; keep theirs, drop ours.
	if existing, ok := b.conns[t.ID]; ok {
		b.mu.Unlock()
		client.Close()
		return existing.client, nil
	}
	c := &conn{client: client, done: make(chan struct{})}
	b.conns[t.ID] = c
	b.mu.Unlock()

	go b.keepalive(t.ID, c)
	return client, nil
}

// Drop closes and forgets the connection for a machine. Safe to call when
// there is none.
func (b *Broker) Drop(id int64) {
	b.mu.Lock()
	c, ok := b.conns[id]
	if ok {
		delete(b.conns, id)
	}
	b.mu.Unlock()
	if ok {
		close(c.done)
		c.client.Close()
	}
}

// Close drops every connection.
func (b *Broker) Close() {
	b.mu.Lock()
	ids := make([]int64, 0, len(b.conns))
	for id := range b.conns {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.Drop(id)
	}
}

// keepalive sends periodic global requests; a failure or a missing reply
// means the transport is dead and the connection is evicted so the next call
// redials cleanly. The reply has its own deadline: a black-holed peer never
// answers, and SendRequest alone would block until TCP gives up.
func (b *Broker) keepalive(id int64, c *conn) {
	t := time.NewTicker(keepaliveTick)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			reply := make(chan error, 1)
			go func() {
				_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
				reply <- err
			}()
			select {
			case err := <-reply:
				if err != nil {
					b.Drop(id)
					return
				}
			case <-time.After(keepaliveReply):
				b.Drop(id) // closing the client unblocks the pending SendRequest
				return
			case <-c.done:
				return
			}
		}
	}
}

// withDeadline runs fn, which has no context of its own (NewSession, Start,
// RequestPty), and abandons it when ctx expires. Abandoning means dropping
// the connection: closing the client is what unblocks the stuck call, and a
// transport that cannot open a session is not worth keeping anyway.
func (b *Broker) withDeadline(ctx context.Context, id int64, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		b.Drop(id)
		return ErrTimeout
	}
}

func dial(ctx context.Context, t Target) (*ssh.Client, error) {
	methods, err := authMethods(t.Auth)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            methods,
		Timeout:         dialTimeout,
		HostKeyCallback: hostKeyCallback(t.HostKey),
	}

	d := net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, classifyDial(err)
	}

	// ssh.NewClientConn has no context; the deadline covers the handshake.
	_ = raw.SetDeadline(time.Now().Add(dialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(raw, t.Addr, cfg)
	if err != nil {
		raw.Close()
		return nil, classifyHandshake(err)
	}
	_ = raw.SetDeadline(time.Time{})

	return ssh.NewClient(c, chans, reqs), nil
}

func authMethods(a Auth) ([]ssh.AuthMethod, error) {
	if len(a.PrivateKey) > 0 {
		var (
			signer ssh.Signer
			err    error
		)
		if len(a.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(a.PrivateKey, a.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(a.PrivateKey)
		}
		if err != nil {
			var pe *ssh.PassphraseMissingError
			if errors.As(err, &pe) {
				return nil, fmt.Errorf("ssh: private key is encrypted and no passphrase was given")
			}
			return nil, fmt.Errorf("ssh: parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if a.Password != "" {
		return []ssh.AuthMethod{
			ssh.Password(a.Password),
			// Some sshd builds only offer keyboard-interactive for passwords.
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = a.Password
				}
				return answers, nil
			}),
		}, nil
	}
	return nil, errors.New("ssh: no credential provided")
}

func hostKeyCallback(policy HostKeyPolicy) ssh.HostKeyCallback {
	if policy == nil {
		// No policy means the caller has not wired pinning yet. Fail closed
		// rather than silently trusting everything.
		return func(string, net.Addr, ssh.PublicKey) error {
			return errors.New("ssh: no host key policy configured")
		}
	}
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		return policy.Check(hostname, key)
	}
}

func classifyDial(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return ErrRefused
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return ErrNoRoute
	}
	return fmt.Errorf("ssh: dial: %w", err)
}

func classifyHandshake(err error) error {
	var mismatch *HostKeyMismatchError
	if errors.As(err, &mismatch) {
		return mismatch
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "unable to authenticate"), strings.Contains(s, "no supported methods remain"):
		return ErrAuth
	case strings.Contains(s, "i/o timeout"):
		return ErrTimeout
	case strings.Contains(s, "host key"):
		return ErrHostKeyMismatch
	}
	return fmt.Errorf("ssh: handshake: %w", err)
}

// Fingerprint renders a public key the way ssh-keygen -l does.
func Fingerprint(key ssh.PublicKey) string { return ssh.FingerprintSHA256(key) }

// --- exec -------------------------------------------------------------------

// ExecResult is the outcome of one non-interactive command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// ExecOpts tunes a single Exec call.
type ExecOpts struct {
	// Stdin is written to the remote command's stdin and then closed. Handy
	// for `sh -s` scripts, which sidestep all quoting problems.
	Stdin io.Reader
	// Timeout bounds the whole command. Zero means 60s.
	Timeout time.Duration
	// MaxOutput caps captured stdout/stderr individually. Zero means 1 MiB.
	MaxOutput int
}

const (
	defaultExecTimeout = 60 * time.Second
	defaultMaxOutput   = 1 << 20
)

// Exec runs one command and waits for it. A non-zero exit is not an error —
// it is reported in ExitCode so the caller can decide what it means.
func (b *Broker) Exec(ctx context.Context, t Target, command string, opts ExecOpts) (ExecResult, error) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultExecTimeout
	}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultMaxOutput
	}

	// The deadline covers everything: dial, session open, the command, and
	// the wait. A half-open transport can otherwise block NewSession until
	// the kernel gives up on TCP retransmits, long after any handler deadline.
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	client, err := b.Get(ctx, t)
	if err != nil {
		return ExecResult{}, err
	}

	var sess *ssh.Session
	if err := b.withDeadline(ctx, t.ID, func() error {
		var e error
		sess, e = client.NewSession()
		return e
	}); err != nil {
		// A dead transport surfaces here; evict so the retry redials.
		b.Drop(t.ID)
		return ExecResult{}, fmt.Errorf("ssh: open session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = opts.MaxOutput, opts.MaxOutput
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	if opts.Stdin != nil {
		stdin, err := sess.StdinPipe()
		if err != nil {
			return ExecResult{}, fmt.Errorf("ssh: stdin pipe: %w", err)
		}
		go func() {
			io.Copy(stdin, opts.Stdin)
			stdin.Close()
		}()
	}

	start := time.Now()
	if err := b.withDeadline(ctx, t.ID, func() error { return sess.Start(command) }); err != nil {
		return ExecResult{}, fmt.Errorf("ssh: start: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	select {
	case <-ctx.Done():
		// Best effort: ask nicely, then close the channel. Not every sshd
		// honors signals, which is why the session close follows. Then give
		// the copy goroutines a moment to finish writing before the buffers
		// are read; Wait is what joins them.
		_ = sess.Signal(ssh.SIGTERM)
		sess.Close()
		select {
		case <-waitErr:
		case <-time.After(2 * time.Second):
		}
		return ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: -1,
			Duration: time.Since(start),
		}, fmt.Errorf("ssh: command timed out after %s", opts.Timeout)

	case err := <-waitErr:
		res := ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: time.Since(start),
		}
		if err == nil {
			return res, nil
		}
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitStatus()
			return res, nil
		}
		var missing *ssh.ExitMissingError
		if errors.As(err, &missing) {
			res.ExitCode = -1
			return res, nil
		}
		return res, fmt.Errorf("ssh: wait: %w", err)
	}
}

// limitedBuffer keeps the first max bytes and drops the rest, so a runaway
// `tail -f` cannot exhaust memory.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := l.max - l.buf.Len()
	if room <= 0 {
		l.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		l.truncated = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return l.buf.String() + "\n… output truncated"
	}
	return l.buf.String()
}

// --- pty --------------------------------------------------------------------

// Shell is an interactive PTY session. The caller pumps bytes between it and
// a websocket.
type Shell struct {
	sess   *ssh.Session
	Stdin  io.WriteCloser
	Stdout io.Reader
}

// OpenShell requests a PTY and starts the user's login shell.
func (b *Broker) OpenShell(ctx context.Context, t Target, cols, rows int) (*Shell, error) {
	// Setup is bounded; the shell itself lives as long as the caller's ctx.
	setupCtx, cancel := context.WithTimeout(ctx, shellSetup)
	defer cancel()

	client, err := b.Get(setupCtx, t)
	if err != nil {
		return nil, err
	}
	var sess *ssh.Session
	if err := b.withDeadline(setupCtx, t.ID, func() error {
		var e error
		sess, e = client.NewSession()
		return e
	}); err != nil {
		b.Drop(t.ID)
		return nil, fmt.Errorf("ssh: open session: %w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if err := b.withDeadline(setupCtx, t.ID, func() error { return sess.RequestPty("xterm-256color", rows, cols, modes) }); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	// With a PTY the remote shell's stderr is written to the pty, which
	// arrives on the stdout channel. The SSH extended-data channel stays
	// unused, so there is nothing to merge.
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}

	if err := b.withDeadline(setupCtx, t.ID, func() error { return sess.Shell() }); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: start shell: %w", err)
	}

	return &Shell{
		sess:   sess,
		Stdin:  stdin,
		Stdout: stdout,
	}, nil
}

// Resize propagates a terminal size change.
func (s *Shell) Resize(cols, rows int) error {
	return s.sess.WindowChange(rows, cols)
}

// Wait blocks until the remote shell exits.
func (s *Shell) Wait() error { return s.sess.Wait() }

// Close tears the session down.
func (s *Shell) Close() error { return s.sess.Close() }

// --- stream -----------------------------------------------------------------

// Stream is a long-lived command whose combined stdout+stderr is piped to the
// caller until the caller closes it or the command exits. It backs follow
// actions (journalctl -f, docker logs -f). Unlike Exec it imposes no command
// timeout — following is the point — only a bounded setup.
type Stream struct {
	sess   *ssh.Session
	Output io.Reader
}

// OpenStream starts command on a new session and returns its merged output.
// stdin, if non-nil, is written and closed (used to feed `sh -s`).
func (b *Broker) OpenStream(ctx context.Context, t Target, command string, stdin io.Reader) (*Stream, error) {
	setupCtx, cancel := context.WithTimeout(ctx, shellSetup)
	defer cancel()

	client, err := b.Get(setupCtx, t)
	if err != nil {
		return nil, err
	}
	var sess *ssh.Session
	if err := b.withDeadline(setupCtx, t.ID, func() error {
		var e error
		sess, e = client.NewSession()
		return e
	}); err != nil {
		b.Drop(t.ID)
		return nil, fmt.Errorf("ssh: open session: %w", err)
	}

	// One pipe carries both streams, in interleaved order, the way a terminal
	// would show them. docker logs writes to both; journalctl to stdout only.
	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw

	if stdin != nil {
		wc, err := sess.StdinPipe()
		if err != nil {
			sess.Close()
			return nil, fmt.Errorf("ssh: stdin pipe: %w", err)
		}
		go func() {
			io.Copy(wc, stdin)
			wc.Close()
		}()
	}

	if err := b.withDeadline(setupCtx, t.ID, func() error { return sess.Start(command) }); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: start: %w", err)
	}

	// When the remote command ends, close the pipe so the reader sees EOF.
	go func() {
		_ = sess.Wait()
		pw.Close()
	}()

	return &Stream{sess: sess, Output: pr}, nil
}

// Close tears the session down; the reader then sees EOF.
func (s *Stream) Close() error { return s.sess.Close() }
