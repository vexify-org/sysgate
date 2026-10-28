package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vexify-org/sysgate/internal/gateway"
	"github.com/vexify-org/sysgate/internal/pool"
	"github.com/vexify-org/sysgate/internal/sandbox"
	"github.com/vexify-org/sysgate/internal/sandbox/fake"
	"github.com/vexify-org/sysgate/internal/sandbox/runsc"
)

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var driver sandbox.Driver
	switch cfg.driver {
	case "fake":
		driver = fake.New()
	case "runsc":
		d, err := runsc.New(cfg.runscBin, cfg.root, cfg.rootfs, cfg.network)
		if err != nil {
			logger.Error("init runsc driver", "err", err)
			os.Exit(1)
		}
		driver = d
	default:
		logger.Error("unknown driver", "driver", cfg.driver, "want", "fake|runsc")
		os.Exit(1)
	}

	p := pool.New(driver, sandbox.Spec{Rootfs: cfg.rootfs}, cfg.poolSize)
	srv := gateway.New(driver, p, gateway.Config{
		DefaultTTL:   cfg.defaultTTL,
		MaxSandboxes: cfg.maxSandboxes,
		ExecTimeout:  cfg.execTimeout,
	})

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("sysgate listening", "addr", cfg.listen, "driver", driver.Name(), "pool", cfg.poolSize)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		logger.Error("server failed", "err", err)
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.Close()
	_ = p.Close()
	_ = driver.Close()
}
