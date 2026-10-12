package logbook

import "regexp"

// Defense in depth for searchable/public projections, not a promise of
// exhaustive anonymization. The Agent already sanitizes configured secrets.
var secret = regexp.MustCompile(`(?i)["']?\b(password|passwd|api[_-]?key|token|secret|community|enrollment[_-]?token|authorization|private[_-]?key)\b["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var bearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9+/_.=~\-]+`)
var privateKey = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)

func scrub(s string) string {
	return secret.ReplaceAllString(bearer.ReplaceAllString(privateKey.ReplaceAllString(s, "[REDACTED PRIVATE KEY]"), "$1 [REDACTED]"), "$1=[REDACTED]")
}

// SanitizeText applies the same additional projection redaction to command output.
func SanitizeText(s string) string { return scrub(s) }
func sanitizeRecord(r *Record) {
	clean := func(v *string) { before := *v; *v = scrub(before); r.Redacted = r.Redacted || *v != before }
	for _, v := range []*string{&r.Host, &r.Process, &r.ProcessID, &r.Message, &r.Raw} {
		clean(v)
	}
	if r.Audit != nil {
		for _, v := range []*string{&r.Audit.Executable, &r.Audit.Result, &r.Audit.Host} {
			clean(v)
		}
	}
}
