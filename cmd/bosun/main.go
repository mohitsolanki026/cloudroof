// Command bosun is the self-hosted control plane: one binary, one data
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

	"bosun/internal/api"
	"bosun/internal/config"
	"bosun/internal/keyring"
	"bosun/internal/sshx"
	"bosun/internal/store"
	"bosun/web"

	// Provider adapters register themselves on import.
	_ "bosun/internal/provider/hetzner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
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
		Store:   db,
		Keyring: keys,
		Broker:  broker,
		Log:     log,
		Static:  web.Dist(),
	}
	if cfg.Dev {
		opts.DevDist = "web/dist"
	}
	srv := api.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv.StartBackground(ctx, 2*time.Minute)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the terminal websocket and long actions need it
		// unbounded. Individual handlers carry their own deadlines.
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("bosun listening", "addr", cfg.Addr, "data", cfg.DataDir, "dev", cfg.Dev)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("bosun stopped")
	return nil
}

func logLevel() slog.Level {
	switch os.Getenv("BOSUN_LOG") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
