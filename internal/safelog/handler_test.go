package safelog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestOperationalErrorsAndInheritedNestedSecretsAreNotSerialized(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(New(slog.NewJSONHandler(&out, nil))).With("database_url", "postgres://private-user:private-password@private-host/db").WithGroup("worker")
	db := &pgconn.PgError{Code: "23514", Message: "private-query", Detail: "private-record", Where: "private-context"}
	log.Error("fixture operation failed", "error", fmt.Errorf("private-dsn: %w", db), "reason", errors.New("private-provider-body"), "job_id", "job-123", "details", slog.GroupValue(slog.String("token", "private-token"), slog.Any("cause", db)))
	log.Error("preformatted error fixture", "error", "private-preformatted-error")
	for _, private := range []string{"private-user", "private-password", "private-host", "private-query", "private-record", "private-context", "private-dsn", "private-provider-body", "private-token", "private-preformatted-error"} {
		if strings.Contains(out.String(), private) {
			t.Fatal("private value in log", private)
		}
	}
	for _, public := range []string{"database_constraint", "operation_failed", "job-123"} {
		if !strings.Contains(out.String(), public) {
			t.Fatal("missing operational diagnostic", public)
		}
	}
	if Code(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) != "timeout" || Code(context.Canceled) != "canceled" {
		t.Fatal("lost cancellation classification")
	}
	startup := At("database_connect", errors.New("private-connection-detail"))
	if Phase(startup) != "database_connect" || strings.Contains(startup.Error(), "private") || Phase(At("private-invalid-phase", context.Canceled)) != "startup" {
		t.Fatal("unsafe startup phase")
	}
	if Code(At("database_connect", context.DeadlineExceeded)) != "timeout" {
		t.Fatal("phase lost error classification")
	}
}
