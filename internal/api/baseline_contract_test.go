package api

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// This fixture is intentionally frozen. V2 adds routes alongside the legacy
// API; removing a route or weakening its permission requires explicit review.
func TestV2PreservesStage9FRoutes(t *testing.T) {
	fixture, err := os.ReadFile("testdata/stage9f-routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var baseline []struct{ Method, Path, Permission string }
	if err = json.Unmarshal(bytes.TrimPrefix(fixture, []byte{0xef, 0xbb, 0xbf}), &baseline); err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 71 {
		t.Fatalf("unexpected baseline size: %d", len(baseline))
	}
	source, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`register\("([A-Z]+) ([^"]+)", ([^,]+),`)
	current := map[string]string{}
	for _, match := range routes.FindAllSubmatch(source, -1) {
		current[string(match[1])+" "+string(match[2])] = string(match[3])
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(openapi, &spec); err != nil {
		t.Fatal(err)
	}
	for _, route := range baseline {
		key := route.Method + " " + route.Path
		if permission, exists := current[key]; !exists || permission != route.Permission {
			t.Errorf("legacy route or permission changed: %s (want %s)", key, route.Permission)
		}
		method := string(bytes.ToLower([]byte(route.Method)))
		if _, exists := spec.Paths[route.Path][method]; !exists {
			t.Errorf("legacy route missing from OpenAPI: %s", key)
		}
	}
}
