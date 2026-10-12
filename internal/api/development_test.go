package api

import (
	"log/slog"
	"net/http"
)

// Existing domain tests intentionally isolate response contracts from login.
// Authentication, role and CSRF enforcement are exercised by access tests.
func testAPI(repo Repository, log *slog.Logger) http.Handler {
	return New(repo, log, WithDevelopmentAuthDisabled())
}
