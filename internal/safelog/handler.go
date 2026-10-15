// Package safelog bounds operational diagnostics without serializing database
// errors, connector responses, DSNs or credentials into the application log.
package safelog

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
)

type handler struct{ next slog.Handler }

func New(next slog.Handler) slog.Handler { return &handler{next: next} }

func Code(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var sql interface{ SQLState() string }
	if errors.As(err, &sql) {
		switch sql.SQLState() {
		case "23505":
			return "database_unique_conflict"
		case "23503", "23514", "23502":
			return "database_constraint"
		case "40001", "40P01":
			return "database_retryable_transaction"
		case "57014":
			return "database_canceled"
		case "08003", "08006", "57P01":
			return "database_unavailable"
		default:
			return "database_error"
		}
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return "timeout"
		}
		return "network_error"
	}
	return "operation_failed"
}

func clean(attr slog.Attr) slog.Attr {
	// Resolve LogValuer before inspection: an error can otherwise serialize later.
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			return slog.String(attr.Key, Code(err))
		}
	}
	key := strings.ToLower(strings.ReplaceAll(attr.Key, "-", "_"))
	switch key {
	case "error", "cause", "reason", "password", "passwd", "secret", "token", "api_key", "authorization", "credentials", "credential", "private_key", "key_pem", "database_url", "dsn", "payload", "raw_response", "response_body", "sql", "query", "detail":
		return slog.String(attr.Key, "[redacted]")
	}
	if attr.Value.Kind() == slog.KindGroup {
		children := attr.Value.Group()
		out := make([]slog.Attr, len(children))
		for i, v := range children {
			out[i] = clean(v)
		}
		attr.Value = slog.GroupValue(out...)
	}
	return attr
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	safe := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(v slog.Attr) bool { safe.AddAttrs(clean(v)); return true })
	return h.next.Handle(ctx, safe)
}
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, len(attrs))
	for i, v := range attrs {
		safe[i] = clean(v)
	}
	return &handler{next: h.next.WithAttrs(safe)}
}
func (h *handler) WithGroup(name string) slog.Handler { return &handler{next: h.next.WithGroup(name)} }
