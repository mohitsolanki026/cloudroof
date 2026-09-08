// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Command cloudroof is the self-hosted control plane: one binary, one data
// directory, one port.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloudroof/internal/api"
	"cloudroof/internal/config"
	"cloudroof/internal/keyring"
	"cloudroof/internal/sshx"
	"cloudroof/internal/store"
	"cloudroof/web"
)

var version = "dev"

func main() {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Println("cloudroof", version)
			return
		}
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cloudroof:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(),
	}))

	keys, err := keyring.Open(cfg.MasterKeyPath, cfg.MasterKeyEnv)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	broker := sshx.NewBroker()
	defer broker.Close()

	opts := api.Options{
		Store:        db,
		Keyring:      keys,
		Broker:       broker,
		Log:          log,
		Static:       web.Dist(),
		AllowedHosts: cfg.Hosts,
	}
	if !cfg.BindsLoopback() && len(cfg.Hosts) == 0 {
		// Reachable from the network with no Host allowlist. Accept any Host
		// rather than lock the user out, but say so: this is the DNS-rebinding
		// exposure -hosts exists to close.
		opts.AllowAnyHost = true
		log.Warn("listening on a non-loopback address with no -hosts allowlist; "+
			"any Host header is accepted — set -hosts <name,ip> or put cloudroof behind a reverse proxy",
			"addr", cfg.Addr)
	}
	if cfg.Dev {
		opts.DevDist = "web/dist"
	}
	srv := api.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv.StartBackground(ctx, 2*time.Minute)
	srv.StartReachPoller(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the terminal websocket and long actions need it
		// unbounded. Individual handlers carry their own deadlines.
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("cloudroof listening", "version", version, "addr", cfg.Addr, "data", cfg.DataDir, "dev", cfg.Dev)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown begins; wait for it to
	// finish draining before the deferred broker/db Close pull the floor out
	// from under in-flight handlers.
	<-shutdownDone
	log.Info("cloudroof stopped")
	return nil
}

func logLevel() slog.Level {
	switch os.Getenv("CLOUDROOF_LOG") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
