package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
)

func TestHealthReportsTheServiceIsUp(t *testing.T) {
	app := New(Deps{Files: files.NewMemory(), PresignTTL: 15 * time.Minute})

	req := httptest.NewRequest("GET", "/health", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}
