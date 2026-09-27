// Package reqid carries the request ID through a context and stamps it on every log
// record, so HTTP, service and OCR logs of one request can be correlated without
// each call site passing it by hand.
package reqid

import (
	"context"
	"log/slog"
)

type key struct{}

// With returns ctx carrying id.
func With(ctx context.Context, id string) context.Context { return context.WithValue(ctx, key{}, id) }

// From returns the request ID in ctx, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}

// Handler adds request_id to every record logged with a context that carries one
// (the *Context logging methods: InfoContext, WarnContext, ...).
type Handler struct{ slog.Handler }

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	if id := From(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return Handler{h.Handler.WithAttrs(attrs)}
}
func (h Handler) WithGroup(name string) slog.Handler { return Handler{h.Handler.WithGroup(name)} }
