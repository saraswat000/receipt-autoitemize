// Command server runs the receipt auto-itemize HTTP API.
//
// Configuration (environment variables, all optional):
//
//	ADDR            listen address            (default ":8080")
//	DATA_DIR        SQLite DB + uploaded files (default "./data")
//	FIXTURES_DIR    stub OCR fixture texts     (default "./fixtures/task-a")
//	MAX_UPLOAD_MB   upload size limit          (default 10)
//	LOG_FORMAT      "json" or "text"           (default "text")
//	PROCESS_MODE    "sync" or "async"          (default "sync")
//	OCR_WORKERS     async: concurrent OCR jobs (default 4)
//	OCR_QUEUE_SIZE  async: jobs that may wait  (default 100)
//	OCR_TIMEOUT     async: per-attempt timeout (default "30s")
//	OCR_MAX_ATTEMPTS async: tries per job      (default 3)
package main

import (
	"context"
	"errors"
	"fmt"
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
	"receipt-autoitemize/internal/worker"
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

	var pool *worker.Pool
	switch mode := env("PROCESS_MODE", "sync"); mode {
	case "sync":
	case "async":
		cfg, err := workerConfig()
		if err != nil {
			return err
		}
		pool = worker.New(cfg, svc.WorkerHandler(), log)
		pool.Start()
		svc.UseQueue(pool)
		// Receipts left PROCESSING by a crash are re-queued in the background, so a
		// large backlog does not delay the server from accepting requests.
		go func() {
			n, err := svc.RecoverPending(ctx)
			if err != nil {
				log.Error("recovery stopped", "requeued", n, "err", err)
				return
			}
			log.Info("recovery done", "requeued", n)
		}()
		log.Info("async processing", "workers", cfg.Workers, "queue_size", cfg.QueueSize)
	default:
		return fmt.Errorf("PROCESS_MODE must be sync or async, got %q", mode)
	}
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
		// Stop HTTP first so no new jobs arrive, then drain the workers.
		err := srv.Shutdown(shutdownCtx)
		if pool != nil {
			if perr := pool.Shutdown(shutdownCtx); perr != nil {
				log.Warn("workers did not finish in time; unfinished receipts stay PROCESSING and resume on restart")
			}
		}
		return err
	}
	return nil
}

func workerConfig() (worker.Config, error) {
	var cfg worker.Config
	var err error
	if cfg.Workers, err = strconv.Atoi(env("OCR_WORKERS", "4")); err != nil || cfg.Workers < 1 {
		return cfg, errors.New("OCR_WORKERS must be a positive integer")
	}
	if cfg.QueueSize, err = strconv.Atoi(env("OCR_QUEUE_SIZE", "100")); err != nil || cfg.QueueSize < 1 {
		return cfg, errors.New("OCR_QUEUE_SIZE must be a positive integer")
	}
	if cfg.JobTimeout, err = time.ParseDuration(env("OCR_TIMEOUT", "30s")); err != nil || cfg.JobTimeout <= 0 {
		return cfg, errors.New("OCR_TIMEOUT must be a duration such as 30s")
	}
	if cfg.MaxAttempts, err = strconv.Atoi(env("OCR_MAX_ATTEMPTS", "3")); err != nil || cfg.MaxAttempts < 1 {
		return cfg, errors.New("OCR_MAX_ATTEMPTS must be a positive integer")
	}
	cfg.Backoff = time.Second
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
