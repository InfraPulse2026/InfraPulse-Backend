package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestTopologyAPIQueriesAndEmptyGraph(t *testing.T) {
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/topology", 200}, {"/api/topology?include_expired=true&limit=2000", 200},
		{"/api/devices/router/topology?agent_id=agent-1", 200},
		{"/api/topology?include_expired=maybe", 400}, {"/api/topology?limit=2001", 400}, {"/api/topology?offset=-1", 400},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			testAPI(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.code {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.code == 200 {
				var data map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"nodes", "edges", "collections"} {
					if string(data[key]) != "[]" {
						t.Fatalf("%s is not an empty array: %s", key, data[key])
					}
				}
			}
		})
	}
}
