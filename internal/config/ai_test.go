package config

import "testing"

func TestExternalAIRequiresExplicitModelKeyAndAuth(t *testing.T) {
	for _, key := range []string{"AI_ENABLED", "AI_MODEL", "OPENAI_API_KEY", "AI_TIMEOUT", "AUTH_MODE", "PKI_ENABLED"} {
		t.Setenv(key, "")
	}
	c, err := Load()
	if err != nil || c.AI.Enabled {
		t.Fatal("AI enabled by default")
	}
	t.Setenv("AI_ENABLED", "true")
	if _, err = Load(); err == nil {
		t.Fatal("missing AI credentials accepted")
	}
	t.Setenv("AI_MODEL", "fixture-model")
	t.Setenv("OPENAI_API_KEY", "test-only-key")
	if _, err = Load(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{{"AUTH_MODE", "disabled"}, {"AI_TIMEOUT", "0s"}, {"AI_TIMEOUT", "61s"}, {"AI_MODEL", "bad\nmodel"}, {"OPENAI_API_KEY", "bad\nkey"}} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid AI config accepted")
			}
		})
	}
}
