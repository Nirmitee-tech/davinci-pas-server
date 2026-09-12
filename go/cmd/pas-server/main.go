// Command pas-server hosts the Da Vinci PAS payer handler as a standalone server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/audit"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/logging"
	"github.com/Nirmitee-tech/davinci-pas-server/go/pas"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pas-server:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "", "listen address, overrides PAS_LISTEN and PAS_PORT (e.g. 0.0.0.0:8085)")
	flag.Parse()

	cfg, err := pas.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	logger := logging.New(os.Stdout, cfg.LogLevel)
	if len(cfg.AuthTokens) == 0 {
		logger.Warn("bearer authentication is disabled because PAS_AUTH_TOKENS is empty; keep this server on a test network")
	}
	if cfg.EnableAdmin {
		logger.Warn("test-only control routes under /admin are enabled (PAS_ADMIN_ENABLED); never enable them in a deployment")
	}

	auditLog, err := audit.New(cfg.AuditDir)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	cfg.Logger = logger
	cfg.Audit = auditLog
	handler, err := pas.NewHandler(cfg)
	if err != nil {
		return err
	}
	if closer, ok := handler.(io.Closer); ok {
		defer closer.Close()
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("pas-server listening", "addr", cfg.ListenAddr, "prefix", cfg.Prefix,
			"publicBaseUrl", cfg.PublicBaseURL, "auditDir", cfg.AuditDir,
			"autoFinalize", cfg.PendedAutoFinalize.String(), "notificationRetry", cfg.NotificationRetry.String(),
			"notificationMax", cfg.NotificationMax.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Duration)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
