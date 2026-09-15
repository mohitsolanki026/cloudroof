// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mohitsolanki026/cloudroof/internal/config"
	"github.com/mohitsolanki026/cloudroof/internal/store"
)

// setPassword implements `cloudroof set-password [-user NAME] [-data DIR]`. It
// creates the account if it does not exist (bootstrapping a headless install)
// or resets it (lockout recovery), reading the new password from stdin. Pipe it
// to avoid echoing on a terminal:
//
//	printf '%s' 'my-strong-password' | cloudroof set-password
func setPassword(args []string) error {
	// -user is handled here; everything else (notably -data and env) is left to
	// config.Load so the data directory resolves exactly as the server's does.
	user := "admin"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-user" || a == "--user":
			if i+1 >= len(args) {
				return errors.New("set-password: -user needs a value")
			}
			user, i = args[i+1], i+1
		case strings.HasPrefix(a, "-user="):
			user = strings.TrimPrefix(a, "-user=")
		case strings.HasPrefix(a, "--user="):
			user = strings.TrimPrefix(a, "--user=")
		default:
			rest = append(rest, a)
		}
	}

	cfg, err := config.Load(rest)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	pw, err := readPassword()
	if err != nil {
		return err
	}
	if n := len(pw); n < 10 || n > 72 {
		return errors.New("password must be 10–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		return err
	}

	u, err := db.GetUserByUsername(user)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := db.CreateUser(user, string(hash), "admin"); err != nil {
			return err
		}
		fmt.Printf("created admin account %q\n", user)
	case err != nil:
		return err
	default:
		if err := db.SetUserPassword(u.ID, string(hash)); err != nil {
			return err
		}
		fmt.Printf("password reset for %q (other sessions logged out)\n", user)
	}
	return nil
}

// readPassword reads a single line from stdin and trims the trailing newline.
// It intentionally does not disable echo (no extra dependency); piping the
// password in keeps it off the screen and out of shell history.
func readPassword() (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "New password (input is visible; Ctrl-D to submit): ")
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 128), 4096)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no password read from stdin")
	}
	return strings.TrimRight(sc.Text(), "\r\n"), nil
}

// startSessionSweeper deletes expired sessions on a slow timer so the table
// does not accumulate dead rows. Expired sessions are already rejected at read
// time; this is only housekeeping.
func startSessionSweeper(ctx context.Context, db *store.Store, log *slog.Logger) {
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := db.DeleteExpiredSessions(); err != nil {
					log.Debug("session sweep failed", "err", err)
				}
			}
		}
	}()
}
