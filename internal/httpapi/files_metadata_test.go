package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
)

// listFiles requests the listing route and returns the records of its items
// array, decoded from the {items,total,page,page_size} envelope.
func listFiles(t *testing.T, app *fiber.App) []map[string]any {
	t.Helper()

	envelope := listEnvelope(t, app, "")
	items, ok := envelope["items"].([]any)
	if !ok {
		t.Fatalf("listing items %v is not a JSON array", envelope["items"])
	}

	records := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		record, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("listing item %v is not a JSON object", raw)
		}
		records = append(records, record)
	}
	return records
}

// listEnvelope requests the listing route with query and returns the decoded
// envelope, so a test can assert total, page and page_size as well as the items.
func listEnvelope(t *testing.T, app *fiber.App, query string) map[string]any {
	t.Helper()

	path := "/files"
	if query != "" {
		path += "?" + query
	}

	resp, body := fetchContent(t, app, "GET", path, "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s status %d, want 200 (body %s)", path, resp.StatusCode, body)
	}

	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decoding the listing %q: %v", body, err)
	}
	return envelope
}

func TestListFilesExposesSHA256(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, digest := uploadFile(t, app, "listed.bin", patternBytes(2048))

	records := listFiles(t, app)
	if len(records) != 1 {
		t.Fatalf("listing carries %d records, want 1", len(records))
	}
	if got := records[0]["id"]; got != id {
		t.Fatalf("listing id %v, want %q", got, id)
	}
	if got := records[0]["sha256"]; got != digest {
		t.Fatalf("listing sha256 %v, want the upload digest %q", got, digest)
	}
}

func TestFileMetadataExposesSHA256(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, digest := uploadFile(t, app, "metadata.bin", patternBytes(4096))

	resp, body := fetchContent(t, app, "GET", "/files/"+id, "")
	if resp.StatusCode != 200 {
		t.Fatalf("metadata status %d, want 200 (body %s)", resp.StatusCode, body)
	}

	payload := decodeJSONObject(t, body)
	if got := payload["id"]; got != id {
		t.Fatalf("metadata id %v, want %q", got, id)
	}
	if got := payload["sha256"]; got != digest {
		t.Fatalf("metadata sha256 %v, want the upload digest %q", got, digest)
	}

	contentResp, contentBody := fetchContent(t, app, "GET", contentPath(id), "")
	if contentResp.StatusCode != 200 {
		t.Fatalf("content status %d, want 200 (body %s)", contentResp.StatusCode, contentBody)
	}
	if got := contentResp.Header.Get("X-Content-SHA256"); got != digest {
		t.Fatalf("X-Content-SHA256 %q, want the metadata digest %q", got, digest)
	}
}

func TestFileMetadataUnknownFileAnswers404(t *testing.T) {
	app := New(Deps{Files: files.NewMemory(), Storage: newFakeStorage(), PresignTTL: testPresignTTL})

	resp, body := fetchContent(t, app, "GET", "/files/"+files.NewID(), "")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404 (body %s)", resp.StatusCode, body)
	}

	payload := decodeJSONObject(t, body)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != "file not found" {
		t.Fatalf("error message %q, want %q", got, "file not found")
	}
}

func TestMetadataRoutesWorkWithoutStorage(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, digest := uploadFile(t, app, "no-storage.bin", patternBytes(256))

	// The listing and get-metadata routes only read the metadata store, so they
	// answer without an object store at all.
	metadataOnly := New(Deps{Files: metadata, PresignTTL: testPresignTTL})

	records := listFiles(t, metadataOnly)
	if len(records) != 1 {
		t.Fatalf("listing without storage carries %d records, want 1", len(records))
	}
	if got := records[0]["sha256"]; got != digest {
		t.Fatalf("listing sha256 %v, want the upload digest %q", got, digest)
	}

	resp, body := fetchContent(t, metadataOnly, "GET", "/files/"+id, "")
	if resp.StatusCode != 200 {
		t.Fatalf("metadata status %d, want 200 without storage (body %s)", resp.StatusCode, body)
	}
	payload := decodeJSONObject(t, body)
	if got := payload["sha256"]; got != digest {
		t.Fatalf("metadata sha256 %v, want the upload digest %q", got, digest)
	}
}
