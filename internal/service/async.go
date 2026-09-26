package service

import (
	"context"
	"errors"
	"fmt"

	"receipt-autoitemize/internal/ocr"
	"receipt-autoitemize/internal/store"
	"receipt-autoitemize/internal/worker"
)

// ErrQueueFull means the OCR queue has no room; the API answers 503 + Retry-After.
var ErrQueueFull = errors.New("processing queue is full")

// Queue is the part of worker.Pool the service needs.
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
	prev, claimed, err := s.store.MarkProcessing(ctx, receiptID)
	if err != nil {
		return ReceiptView{}, mapStoreErr(err)
	}
	if claimed && !s.queue.TrySubmit(receiptID) {
		// Put the receipt back so the client can retry later; nothing is lost.
		if err := s.store.RestoreStatus(ctx, receiptID, prev); err != nil {
			return ReceiptView{}, err
		}
		return ReceiptView{}, ErrQueueFull
	}
	return s.GetReceipt(ctx, receiptID)
}

// RecoverPending re-queues receipts left in PROCESSING by a crash or shutdown.
// The database is the durable queue; the in-memory channel is only a hand-off.
func (s *Service) RecoverPending(ctx context.Context) (int, error) {
	if s.queue == nil {
		return 0, nil
	}
	ids, err := s.store.PendingReceipts(ctx)
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if err := s.queue.Submit(ctx, id); err != nil {
			return i, fmt.Errorf("re-queue %s: %w", id, err)
		}
	}
	return len(ids), nil
}

// WorkerHandler adapts the service to the worker pool.
func (s *Service) WorkerHandler() worker.Handler {
	return worker.Handler{
		Run: func(ctx context.Context, id string) error {
			_, err := s.processOnce(ctx, id)
			return err
		},
		Retryable: Retryable,
		Fail: func(ctx context.Context, id string, err error) {
			_ = s.store.MarkReceiptFailed(ctx, id, err.Error(), s.now())
		},
	}
}

// Retryable separates transient failures (vendor timeout, 5xx, a DB hiccup) from
// permanent ones that will fail the same way every time.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, ocr.ErrNoText), // the engine read the file and found nothing
		errors.Is(err, ErrNotFound),
		errors.Is(err, store.ErrNotFound):
		return false
	}
	return true
}

var _ Queue = (*worker.Pool)(nil)
