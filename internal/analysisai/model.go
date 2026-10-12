package analysisai

import (
	"context"
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid AI request")
var ErrDisabled = errors.New("external AI is disabled")
var ErrExternalUnsupported = errors.New("external signal analysis is not implemented")
var ErrConflict = errors.New("AI idempotency/preview conflict")
var ErrNotFound = errors.New("AI resource not found")

const Destination = "https://api.openai.com/v1/responses"
const PromptVersion = "infrapulse-evidence-v1"

type Config struct {
	Enabled bool
	Model   string
	APIKey  string
	Timeout time.Duration
}
type Fact struct {
	SampleCount       int      `json:"sample_count"`
	WindowSeconds     float64  `json:"window_seconds"`
	Slope             *float64 `json:"slope_percent_per_second"`
	Fit               *float64 `json:"r_squared"`
	SecondsToCritical *float64 `json:"seconds_to_critical"`
	Ref               string   `json:"ref"`
	Incident          string   `json:"incident"`
	Resource          string   `json:"resource"`
	Rule              string   `json:"rule"`
	Status            string   `json:"status"`
	Severity          string   `json:"severity"`
	Simulated         bool     `json:"simulated"`
	SecondsFromSeed   float64  `json:"seconds_from_seed"`
	Value             *float64 `json:"value"`
	Unit              string   `json:"unit"`
	Connected         *bool    `json:"connected"`
}
type Relation struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Kind     string `json:"kind"`
	Strength string `json:"strength"`
}
type Payload struct {
	LogSummaryRef     string           `json:"log_summary_ref,omitempty"`
	Version           string           `json:"version"`
	Facts             []Fact           `json:"facts"`
	Relations         []Relation       `json:"relations"`
	LogSeverityCounts map[string]int64 `json:"log_severity_counts"`
	LogEventCounts    map[string]int64 `json:"log_event_counts"`
	Limitations       []string         `json:"limitations"`
}
type Request struct {
	Model           string `json:"model"`
	Instructions    string `json:"instructions"`
	Input           string `json:"input"`
	Store           bool   `json:"store"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Text            any    `json:"text"`
}
type Preview struct {
	ID            string    `json:"id"`
	IncidentID    string    `json:"incident_id"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Destination   string    `json:"destination"`
	Enabled       bool      `json:"enabled"`
	PromptVersion string    `json:"prompt_version"`
	Hash          string    `json:"request_hash"`
	Request       Request   `json:"request"`
	Payload       Payload   `json:"payload"`
	Excluded      []string  `json:"excluded_fields"`
}
type Statement struct {
	Text string   `json:"text"`
	Refs []string `json:"refs"`
}
type Hypothesis struct {
	Text       string   `json:"text"`
	Refs       []string `json:"refs"`
	Confidence string   `json:"confidence"`
}
type Recommendation struct {
	Text   string   `json:"text"`
	Refs   []string `json:"refs"`
	Safety string   `json:"safety"`
}
type Result struct {
	Summary         string           `json:"summary"`
	Observations    []Statement      `json:"observations"`
	Hypotheses      []Hypothesis     `json:"hypotheses"`
	Recommendations []Recommendation `json:"recommendations"`
	Limitations     []string         `json:"limitations"`
}
type Job struct {
	ID          string    `json:"id"`
	IncidentID  string    `json:"incident_id"`
	PreviewID   string    `json:"preview_id"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	State       string    `json:"state"`
	Failure     string    `json:"failure"`
	Model       string    `json:"model"`
	RequestHash string    `json:"request_hash"`
	Result      *Result   `json:"result"`
}
type Create struct {
	PreviewID   string `json:"preview_id"`
	RequestHash string `json:"request_hash"`
	Confirmed   bool   `json:"confirmed"`
}
type Repository interface {
	CreatePreview(context.Context, string, Config) (Preview, error)
	CreateJob(context.Context, string, Create, string, Config) (Job, bool, error)
	Job(context.Context, string) (Job, error)
	Jobs(context.Context, string, int) ([]Job, error)
	ClaimJob(context.Context, Config) (Job, Preview, error)
	FinishJob(context.Context, string, *Result, string, bool) error
	RecoverJobs(context.Context) error
}
type Provider interface {
	Analyze(context.Context, Preview) (Result, error)
}
type ProviderError struct {
	Code      string
	Uncertain bool
}

func (e *ProviderError) Error() string { return e.Code }
