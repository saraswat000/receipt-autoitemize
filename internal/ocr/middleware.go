package ocr

import (
	"container/list"
	"context"
	"log/slog"
	"sync"
	"time"
)

// Middleware wraps an Engine with extra behaviour and returns an Engine
// (the Decorator pattern). The service only sees the Engine interface, so
// timeouts, caching and logging are added in cmd/server without touching it,
// and a real vendor engine gets them for free.
type Middleware func(Engine) Engine

// Chain wraps e so that the first middleware is the outermost:
// Chain(e, a, b) calls a, then b, then e.
func Chain(e Engine, mws ...Middleware) Engine {
	for i := len(mws) - 1; i >= 0; i-- {
		e = mws[i](e)
	}
	return e
}

// WithTimeout bounds every call. Async jobs already have a per-attempt timeout
// in the worker pool; this also covers synchronous /process calls.
func WithTimeout(d time.Duration) Middleware {
	return func(next Engine) Engine { return timeoutEngine{next, d} }
}

type timeoutEngine struct {
	Engine
	d time.Duration
}

func (e timeoutEngine) ExtractText(ctx context.Context, in Input) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.d)
	defer cancel()
	return e.Engine.ExtractText(ctx, in)
}

// WithLogging records the duration and outcome of every call.
func WithLogging(log *slog.Logger) Middleware {
	return func(next Engine) Engine { return loggingEngine{next, log} }
}

type loggingEngine struct {
	Engine
	log *slog.Logger
}

func (e loggingEngine) ExtractText(ctx context.Context, in Input) (string, error) {
	start := time.Now()
	text, err := e.Engine.ExtractText(ctx, in)
	attrs := []any{"engine", e.Name(), "sha256", in.SHA256, "duration", time.Since(start), "chars", len(text)}
	if err != nil {
		e.log.WarnContext(ctx, "ocr failed", append(attrs, "err", err)...)
	} else {
		e.log.InfoContext(ctx, "ocr done", attrs...)
	}
	return text, err
}

// WithCache remembers successful results for the last size files, so processing
// the same file again (a retry, a re-upload) does not pay for another vendor
// call. The key is the file hash plus the file name: the stub resolves images by
// name, and keeping the name makes the cache safe for it too. Errors are never
// cached, so a transient vendor failure is retried for real.
func WithCache(size int) Middleware {
	return func(next Engine) Engine {
		return &cachingEngine{Engine: next, size: size, order: list.New(), entries: map[cacheKey]*list.Element{}}
	}
}

type cacheKey struct{ sha256, filename string }

type cacheEntry struct {
	key  cacheKey
	text string
}

type cachingEngine struct {
	Engine
	size int

	mu      sync.Mutex
	order   *list.List // front = most recently used
	entries map[cacheKey]*list.Element
}

func (e *cachingEngine) ExtractText(ctx context.Context, in Input) (string, error) {
	if in.SHA256 == "" || e.size <= 0 {
		return e.Engine.ExtractText(ctx, in)
	}
	key := cacheKey{in.SHA256, in.Filename}
	if text, ok := e.get(key); ok {
		return text, nil
	}
	text, err := e.Engine.ExtractText(ctx, in)
	if err == nil {
		e.put(key, text)
	}
	return text, err
}

func (e *cachingEngine) get(k cacheKey) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	el, ok := e.entries[k]
	if !ok {
		return "", false
	}
	e.order.MoveToFront(el)
	return el.Value.(*cacheEntry).text, true
}

func (e *cachingEngine) put(k cacheKey, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if el, ok := e.entries[k]; ok {
		el.Value.(*cacheEntry).text = text
		e.order.MoveToFront(el)
		return
	}
	e.entries[k] = e.order.PushFront(&cacheEntry{k, text})
	if e.order.Len() > e.size {
		oldest := e.order.Back()
		e.order.Remove(oldest)
		delete(e.entries, oldest.Value.(*cacheEntry).key)
	}
}
