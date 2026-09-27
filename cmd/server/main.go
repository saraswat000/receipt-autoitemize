// Command server runs the receipt auto-itemize HTTP API.
//
// Configuration (environment variables, all optional):
//
//	ADDR            listen address            (default ":8080")
//	DATA_DIR        SQLite DB + uploaded files (default "./data")
//	FIXTURES_DIR    stub OCR fixture texts     (default "./fixtures/task-a")
//	MAX_UPLOAD_MB   upload size limit          (default 10)
//	LOG_FORMAT      "json" or "text"           (default "text")
//	STORE           "sqlite" or "memory"       (default "sqlite")
//	PROCESS_MODE    "sync" or "async"          (default "sync")
//	OCR_WORKERS     async: concurrent OCR jobs (default 4)
//	OCR_QUEUE_SIZE  async: jobs that may wait  (default 100)
//	OCR_TIMEOUT     per OCR call / async attempt (default "30s")
//	OCR_CACHE_SIZE  OCR results kept in the LRU (default 1000; 0 = off)
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
	"receipt-autoitemize/internal/repository"
	"receipt-autoitemize/internal/repository/memory"
	"receipt-autoitemize/internal/repository/sqlite"
	"receipt-autoitemize/internal/reqid"
	"receipt-autoitemize/internal/service"
	"receipt-autoitemize/internal/storage"
	"receipt-autoitemize/internal/worker"
)

// The composition root is the only place that knows the service runs on a worker pool.
var _ service.Queue = (*worker.Pool)(nil)

// jobHandler adapts the service's job methods to the generic worker pool.
func jobHandler(svc *service.Service) worker.Handler {
	return worker.Handler{Run: svc.RunJob, Retryable: service.Retryable, Fail: svc.FailJob}
}

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
	log := slog.New(reqid.Handler{Handler: handler}) // request_id on every log line of a request

	addr := env("ADDR", ":8080")
	dataDir := env("DATA_DIR", "./data")
	fixturesDir := env("FIXTURES_DIR", "./fixtures/task-a")
	maxUploadMB, err := strconv.ParseInt(env("MAX_UPLOAD_MB", "10"), 10, 64)
	if err != nil || maxUploadMB <= 0 {
		return errors.New("MAX_UPLOAD_MB must be a positive integer")
	}

	files, err := storage.NewDisk(filepath.Join(dataDir, "uploads"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := openRepository(ctx, env("STORE", "sqlite"), dataDir)
	if err != nil {
		return err
	}
	defer repo.Close()

	ocrTimeout, err := time.ParseDuration(env("OCR_TIMEOUT", "30s"))
	if err != nil || ocrTimeout <= 0 {
		return errors.New("OCR_TIMEOUT must be a duration such as 30s")
	}
	cacheSize, err := strconv.Atoi(env("OCR_CACHE_SIZE", "1000"))
	if err != nil || cacheSize < 0 {
		return errors.New("OCR_CACHE_SIZE must be zero or a positive integer")
	}
	// The stub is decorated exactly as a real vendor engine would be.
	engine := ocr.Chain(ocr.StubEngine{FixturesDir: fixturesDir},
		ocr.WithLogging(log),        // outermost: sees cache hits and failures
		ocr.WithCache(cacheSize),    // a hit skips the timeout and the vendor
		ocr.WithTimeout(ocrTimeout), // innermost: bounds the real call
	)
	svc := service.New(repo, engine, files, log)

	var pool *worker.Pool
	switch mode := env("PROCESS_MODE", "sync"); mode {
	case "sync":
	case "async":
		cfg, err := workerConfig(ocrTimeout)
		if err != nil {
			return err
		}
		pool = worker.New(cfg, jobHandler(svc), log)
		pool.Start()
		svc.UseQueue(pool)
		// Snapshot receipts left PROCESSING by a crash before serving, so recovery
		// never picks up a receipt a live request has just claimed. They are re-queued
		// in the background, so a large backlog does not delay accepting requests.
		pending, err := svc.PendingJobs(ctx)
		if err != nil {
			return fmt.Errorf("find pending receipts: %w", err)
		}
		go func() {
			n, err := svc.Requeue(ctx, pending)
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
		WriteTimeout:      ocrTimeout + 10*time.Second, // a sync /process must be able to answer 504
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

// openRepository is the only place that knows which database backs the service.
// Adding Postgres means one more case here plus a package that passes repotest.
func openRepository(ctx context.Context, kind, dataDir string) (repository.Repository, error) {
	switch kind {
	case "sqlite":
		return sqlite.Open(ctx, filepath.Join(dataDir, "receipts.db"))
	case "memory":
		return memory.New(), nil
	default:
		return nil, fmt.Errorf("STORE must be sqlite or memory, got %q", kind)
	}
}

func workerConfig(jobTimeout time.Duration) (worker.Config, error) {
	var cfg worker.Config
	var err error
	if cfg.Workers, err = strconv.Atoi(env("OCR_WORKERS", "4")); err != nil || cfg.Workers < 1 {
		return cfg, errors.New("OCR_WORKERS must be a positive integer")
	}
	if cfg.QueueSize, err = strconv.Atoi(env("OCR_QUEUE_SIZE", "100")); err != nil || cfg.QueueSize < 1 {
		return cfg, errors.New("OCR_QUEUE_SIZE must be a positive integer")
	}
	// The job budget covers reading the file and saving the result, not just the OCR
	// call (which has its own OCR_TIMEOUT); an equal budget would let a slow but
	// successful OCR call starve the save and pay the vendor again on retry.
	cfg.JobTimeout = jobTimeout + 10*time.Second
	if cfg.MaxAttempts, err = strconv.Atoi(env("OCR_MAX_ATTEMPTS", "3")); err != nil || cfg.MaxAttempts < 1 {
		return cfg, errors.New("OCR_MAX_ATTEMPTS must be a positive integer")
	}
	cfg.Backoff = time.Second
	cfg.MaxBackoff = 30 * time.Second
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
