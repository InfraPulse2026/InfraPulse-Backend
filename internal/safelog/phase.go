package safelog

import "errors"

type phaseError struct {
	phase string
	cause error
}

func (e *phaseError) Error() string { return e.phase + " failed" }
func (e *phaseError) Unwrap() error { return e.cause }

// At preserves a fixed startup hint without publishing the underlying detail.
func At(phase string, cause error) error {
	if cause == nil {
		return nil
	}
	switch phase {
	case "configuration", "database_connect", "database_migrate", "command_recovery", "analysis_recovery":
	default:
		phase = "startup"
	}
	return &phaseError{phase: phase, cause: cause}
}
func Phase(err error) string {
	var e *phaseError
	if errors.As(err, &e) {
		return e.phase
	}
	return "startup"
}
