package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pebbledb/sql/engine"
)

func TestHealthStatusAndMetrics(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler, err := Handler(database)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"/healthz": "ok", "/status": `"Upgrade"`, "/metrics": "pebbledb_cluster_version_active",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), content) {
			t.Fatalf("%s response: code=%d body=%q", path, response.Code, response.Body.String())
		}
	}
}
