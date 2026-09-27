package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/repository"
)

// ErrQueueFull means the OCR queue has no room; the API answers 503 + Retry-After.
var ErrQueueFull = errors.New("processing queue is full")

// Queue is the part of a job queue the service needs (worker.Pool in cmd/server).
type Queue interface {
	TrySubmit(receiptID string) bool
	Submit(ctx context.Context, receiptID string) error
}

// UseQueue switches Process requests to async mode.
func (s *Service) UseQueue(q Queue) { s.queue = q }

// Async reports whether process requests are queued.
func (s *Service) Async() bool { return s.queue != nil }

// ProcessAsync marks the receipt PROCESSING and queues it. A receipt that is already
// PROCESSING is not queued again, so repeated calls collapse into one job.
func (s *Service) ProcessAsync(ctx context.Context, receiptID string) (ReceiptView, error) {
	if s.queue == nil {
		return ReceiptView{}, errors.New("async processing is not enabled")
	}
	prev, claimed, err := s.repo.MarkProcessing(ctx, receiptID)
	if err != nil {
		return ReceiptView{}, mapStoreErr(err)
	}
	if claimed && !s.queue.TrySubmit(receiptID) {
		// Put the receipt back so the client can retry later; nothing is lost.
		// Detached from the request: if the client hangs up now, the receipt must
		// still go back, or it would sit PROCESSING with no job behind it.
		if err := s.repo.RestoreStatus(context.WithoutCancel(ctx), receiptID, prev); err != nil {
			return ReceiptView{}, err
		}
		return ReceiptView{}, ErrQueueFull
	}
	return s.GetReceipt(ctx, receiptID)
}

// PendingJobs snapshots the receipts left in PROCESSING by a crash or shutdown. The
// database is the durable queue; the in-memory channel is only a hand-off.
//
// Take the snapshot before the server accepts requests. A receipt that a live
// request claims afterwards is not in it, so no job is ever queued twice (which
// would be harmless, since saves are idempotent, but pays the vendor twice).
func (s *Service) PendingJobs(ctx context.Context) ([]string, error) {
	if s.queue == nil {
		return nil, nil
	}
	return s.repo.PendingReceipts(ctx)
}

// Requeue hands a PendingJobs snapshot to the queue, waiting for room as needed.
func (s *Service) Requeue(ctx context.Context, ids []string) (int, error) {
	if s.queue == nil {
		return 0, nil
	}
	for i, id := range ids {
		if err := s.queue.Submit(ctx, id); err != nil {
			return i, fmt.Errorf("re-queue %s: %w", id, err)
		}
	}
	return len(ids), nil
}

// RunJob is one async processing attempt. It is idempotent, so a queue may deliver
// the same job more than once.
func (s *Service) RunJob(ctx context.Context, receiptID string) error {
	_, err := s.processOnce(ctx, receiptID)
	return err
}

// FailJob records a job that will not be retried. The pool has already logged err.
func (s *Service) FailJob(ctx context.Context, receiptID string, err error) error {
	return s.repo.MarkReceiptFailed(ctx, receiptID, FailureReason(err), s.now())
}

// Retryable separates transient failures (vendor timeout, 5xx, a DB hiccup) from
// permanent ones that will fail the same way every time. Both the async worker and
// the synchronous Process use it, so a failure is classified the same way in both.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, ocr.ErrNoText), // the engine read the file and found nothing
		errors.Is(err, fs.ErrNotExist), // the stored upload is gone
		errors.Is(err, ErrNotFound),
		errors.Is(err, repository.ErrNotFound):
		return false
	}
	return true
}
