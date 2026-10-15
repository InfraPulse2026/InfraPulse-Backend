package assethardware

import "strings"

// CanonicalHealth keeps unknown vendor text out of incident evidence.
func CanonicalHealth(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "ok", "warning", "critical", "":
		return v
	}
	return "unknown"
}

// Status uses only explicit vendor health. Power Off, missing sensors and
// numeric readings without a vendor policy cannot establish a fault or recovery.
func Status(o Observation) string {
	if o.Kind == "metric" {
		return "unknown"
	}
	status, unknown, count := "ok", false, 0
	for _, raw := range []string{o.Health, o.HealthRollup} {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		count++
		switch v {
		case "critical":
			status = "critical"
		case "warning":
			if status != "critical" {
				status = "warning"
			}
		case "ok":
		default:
			unknown = true
		}
	}
	if status != "ok" {
		return status
	}
	if count == 0 || unknown {
		return "unknown"
	}
	return "ok"
}
