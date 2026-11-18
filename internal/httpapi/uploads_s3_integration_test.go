package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/quantran-code/go-fiber-file-service/internal/config"
	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// TestPresignedUploadRoundTripThroughObjectStore drives the whole direct-upload
// flow against the configured S3-compatible store: the service presigns a PUT,
// the client PUTs the bytes straight to the signed URL, the service completes the
// upload and the streaming route serves the same bytes back.
//
// It needs a real bucket, so it skips unless the S3 environment variables are
// configured; every other test in this package runs against the in-memory fakes.
func TestPresignedUploadRoundTripThroughObjectStore(t *testing.T) {
	cfg := config.Load()
	if !cfg.S3.Configured() {
		t.Skip("S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are not set")
	}

	store, err := storage.NewMinIO(cfg.S3)
	if err != nil {
		t.Fatalf("building the object-store client: %v", err)
	}

	app := New(Deps{Files: files.NewMemory(), Storage: store, PresignTTL: cfg.S3.PresignTTL})

	name := "direct-upload-" + files.NewID() + ".txt"
	const contentType = "text/plain"
	content := []byte("the browser PUTs these bytes straight to the object store")
	digest, size, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the payload: %v", err)
	}

	payload := presignFile(t, app, name, contentType, size)

	key, _ := payload["key"].(string)
	if key == "" {
		t.Fatalf("presign body %v carries no key", payload)
	}
	t.Cleanup(func() {
		if err := store.Delete(context.Background(), key); err != nil {
			t.Logf("removing the uploaded object %q: %v", key, err)
		}
	})

	uploadURL, _ := payload["uploadUrl"].(string)
	if uploadURL == "" {
		t.Fatalf("presign body %v carries no uploadUrl", payload)
	}

	required, _ := payload["requiredHeaders"].(map[string]any)
	putRequest, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("building the direct PUT: %v", err)
	}
	for header, raw := range required {
		// The client sends the content type it was told to; Content-Length comes
		// from the body bytes net/http is handed.
		if strings.EqualFold(header, "Content-Length") {
			continue
		}
		value, _ := raw.(string)
		putRequest.Header.Set(header, value)
	}

	putResponse, err := http.DefaultClient.Do(putRequest)
	if err != nil {
		t.Fatalf("PUT %s: %v", uploadURL, err)
	}
	putBody, _ := io.ReadAll(putResponse.Body)
	putResponse.Body.Close()
	if putResponse.StatusCode < 200 || putResponse.StatusCode > 299 {
		t.Fatalf("the direct PUT answered %d: %s", putResponse.StatusCode, putBody)
	}

	complete := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         name,
		"contentType":  contentType,
		"expectedSize": size,
		"sha256":       digest,
	})
	if complete.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(complete.Body)
		complete.Body.Close()
		t.Fatalf("complete status %d, want 201 (body %s)", complete.StatusCode, raw)
	}
	completed := readJSONBody(t, complete)

	id, _ := completed["id"].(string)
	if id == "" {
		t.Fatalf("complete body %v carries no id", completed)
	}
	if got := completed["name"]; got != name {
		t.Fatalf("complete name %v, want %q", got, name)
	}
	if got, ok := completed["size"].(float64); !ok || int64(got) != size {
		t.Fatalf("complete size %v, want %d", completed["size"], size)
	}
	if got := completed["sha256"]; got != digest {
		t.Fatalf("complete sha256 %v, want the digest over the uploaded bytes %q", got, digest)
	}

	resp, body := fetchContent(t, app, "GET", contentPath(id), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("downloaded %d bytes that differ from the %d uploaded bytes", len(body), len(content))
	}
}
