// Command server runs the receipt auto-itemize HTTP API.
//
// Configuration (environment variables, all optional):
//
//	ADDR            listen address            (default ":8080")
//	DATA_DIR        SQLite DB + uploaded files (default "./data")
//	FIXTURES_DIR    stub OCR fixture texts     (default "./fixtures/task-a")
//	MAX_UPLOAD_MB   upload size limit          (default 10)
//	LOG_FORMAT      "json" or "text"           (default "text")
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"receipt-autoitemize/internal/api"
	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/service"
	"receipt-autoitemize/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, nil)
	if os.Getenv("LOG_FORMAT") == "json" {
		handler = slog.NewJSONHandler(os.Stdout, nil)
	}
	log := slog.New(handler)

	addr := env("ADDR", ":8080")
	dataDir := env("DATA_DIR", "./data")
	fixturesDir := env("FIXTURES_DIR", "./fixtures/task-a")
	maxUploadMB, err := strconv.ParseInt(env("MAX_UPLOAD_MB", "10"), 10, 64)
	if err != nil || maxUploadMB <= 0 {
		return errors.New("MAX_UPLOAD_MB must be a positive integer")
	}

	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(dataDir, "receipts.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	svc := service.New(st, ocr.StubEngine{FixturesDir: fixturesDir}, uploadsDir)
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(svc, log, maxUploadMB<<20).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "data_dir", dataDir, "ocr_engine", svc.OCREngineName())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
