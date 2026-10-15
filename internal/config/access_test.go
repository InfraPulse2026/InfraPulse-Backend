package config

import "testing"

func TestAuthenticationDefaultsAndDevelopmentBoundary(t *testing.T) {
	for _, k := range []string{"HTTP_ADDRESS", "AUTH_MODE", "AUTH_SESSION_TTL", "AUTH_PUBLIC_ORIGIN", "AUTH_COOKIE_SECURE"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil || c.Access.Mode != "required" || c.Access.CookieSecure {
		t.Fatalf("loopback auth defaults %+v %v", c, err)
	}
	t.Setenv("AUTH_MODE", "disabled")
	if _, err = Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_ADDRESS", "0.0.0.0:8080")
	if _, err = Load(); err == nil {
		t.Fatal("unauthenticated wildcard listener accepted")
	}
	t.Setenv("AUTH_MODE", "required")
	c, err = Load()
	if err != nil || !c.Access.CookieSecure {
		t.Fatal("non-loopback cookies not secure by default")
	}
	t.Setenv("AUTH_COOKIE_SECURE", "false")
	if _, err = Load(); err == nil {
		t.Fatal("insecure remote cookies accepted implicitly")
	}
	t.Setenv("AUTH_PUBLIC_ORIGIN", "http://127.0.0.1:8080")
	if _, err = Load(); err != nil {
		t.Fatal("explicit loopback Compose origin rejected")
	}
}
