package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// integrityPath is the route that verifies one file's stored bytes.
func integrityPath(id string) string {
	return "/files/" + id + "/integrity"
}

// uploadFile uploads content through POST /files and returns the id and the
// digest the 201 body reported.
func uploadFile(t *testing.T, app *fiber.App, name string, content []byte) (string, string) {
	t.Helper()

	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    name,
		contentType: "application/octet-stream",
		content:     content,
	})

	resp := postUpload(t, app, body, contentType)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("upload status %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	payload := readJSONBody(t, resp)
	id, _ := payload["id"].(string)
	digest, _ := payload["sha256"].(string)
	if id == "" {
		t.Fatalf("upload response %v carries no id", payload)
	}
	if digest == "" {
		t.Fatalf("upload response %v carries no sha256", payload)
	}
	return id, digest
}

// decodeJSONObject decodes a raw JSON response body into a map.
func decodeJSONObject(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding the response body %q: %v", body, err)
	}
	return payload
}

func TestIntegrityVerifiesAnUntouchedObject(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := patternBytes(64 << 10)
	id, digest := uploadFile(t, app, "verified.bin", content)

	resp, body := fetchContent(t, app, "GET", integrityPath(id), "")
	if resp.StatusCode != 200 {
		t.Fatalf("integrity status %d, want 200 (body %s)", resp.StatusCode, body)
	}

	payload := decodeJSONObject(t, body)
	if len(payload) != 5 {
		t.Fatalf("integrity body %v carries %d fields, want 5", payload, len(payload))
	}
	for _, field := range []string{"id", "sha256", "size", "verified", "checkedAt"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("integrity body %v carries no %q field", payload, field)
		}
	}
	if got := payload["id"]; got != id {
		t.Fatalf("integrity id %v, want %q", got, id)
	}
	if got := payload["sha256"]; got != digest {
		t.Fatalf("integrity sha256 %v, want the upload digest %q", got, digest)
	}
	if got, ok := payload["verified"].(bool); !ok || !got {
		t.Fatalf("integrity verified %v, want true", payload["verified"])
	}
	if got, ok := payload["size"].(float64); !ok || int64(got) != int64(len(content)) {
		t.Fatalf("integrity size %v, want the %d stored bytes", payload["size"], len(content))
	}
	checkedAt, _ := payload["checkedAt"].(string)
	if checkedAt == "" {
		t.Fatalf("integrity body %v carries no checkedAt", payload)
	}
	if _, err := time.Parse(time.RFC3339Nano, checkedAt); err != nil {
		t.Fatalf("checkedAt %q is not an RFC 3339 timestamp: %v", checkedAt, err)
	}

	gets := store.recordedGets()
	if len(gets) != 1 {
		t.Fatalf("storage recorded %d object reads, want the single streaming one", len(gets))
	}
	if gets[0].offset != 0 || gets[0].length != 0 {
		t.Fatalf("integrity read offset %d length %d, want the whole object", gets[0].offset, gets[0].length)
	}
	reader := store.lastBody()
	if reader == nil {
		t.Fatal("storage handed out no object body")
	}
	if reader.reads < 2 {
		t.Fatalf("the object body was read in %d calls, want the digest taken in a streaming copy", reader.reads)
	}
	if !reader.closed {
		t.Fatal("the object body was not closed once the digest was taken")
	}
}

func TestIntegrityReportsAMismatchAs409(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := patternBytes(32 << 10)
	id, digest := uploadFile(t, app, "tampered.bin", content)

	// Same length, different bytes: only the digest can tell the difference.
	tampered := append([]byte(nil), content...)
	for i := range tampered {
		tampered[i] ^= 0xff
	}
	store.corrupt(objectKeyPrefix+id, tampered)

	resp, body := fetchContent(t, app, "GET", integrityPath(id), "")
	if resp.StatusCode == 200 {
		t.Fatalf("integrity answered 200 for tampered bytes (body %s)", body)
	}
	if resp.StatusCode != 409 {
		t.Fatalf("integrity status %d, want 409 (body %s)", resp.StatusCode, body)
	}

	payload := decodeJSONObject(t, body)
	if got, _ := payload["error"].(string); got != "integrity mismatch" {
		t.Fatalf("error message %q, want %q", got, "integrity mismatch")
	}
	if len(payload) != 6 {
		t.Fatalf("mismatch body %v carries %d fields, want the five metadata fields plus error", payload, len(payload))
	}
	if got := payload["id"]; got != id {
		t.Fatalf("integrity id %v, want %q", got, id)
	}
	if got := payload["sha256"]; got != digest {
		t.Fatalf("integrity sha256 %v, want the stored digest %q", got, digest)
	}
	if got, ok := payload["size"].(float64); !ok || int64(got) != int64(len(content)) {
		t.Fatalf("integrity size %v, want %d", payload["size"], len(content))
	}
	if got, ok := payload["verified"].(bool); !ok || got {
		t.Fatalf("integrity verified %v, want false", payload["verified"])
	}
	if got, _ := payload["checkedAt"].(string); got == "" {
		t.Fatalf("mismatch body %v carries no checkedAt", payload)
	}
}

func TestIntegrityReportsASizeMismatchAs409(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := patternBytes(8 << 10)
	id, digest := uploadFile(t, app, "size-mismatch.bin", content)

	// Only the recorded size is wrong: the bytes still hash to the stored
	// digest, so the size comparison is what has to fail.
	record, ok := metadata.Get(id)
	if !ok {
		t.Fatalf("metadata holds no record for %q", id)
	}
	record.Size++
	if err := metadata.Replace(record); err != nil {
		t.Fatalf("updating the metadata size: %v", err)
	}

	resp, body := fetchContent(t, app, "GET", integrityPath(id), "")
	if resp.StatusCode != 409 {
		t.Fatalf("integrity status %d, want 409 for a size mismatch (body %s)", resp.StatusCode, body)
	}

	payload := decodeJSONObject(t, body)
	if got, _ := payload["error"].(string); got != "integrity mismatch" {
		t.Fatalf("error message %q, want %q", got, "integrity mismatch")
	}
	if got, ok := payload["verified"].(bool); !ok || got {
		t.Fatalf("integrity verified %v, want false", payload["verified"])
	}
	if got := payload["sha256"]; got != digest {
		t.Fatalf("integrity sha256 %v, want the stored digest %q", got, digest)
	}
	if got, ok := payload["size"].(float64); !ok || int64(got) != int64(len(content)) {
		t.Fatalf("integrity size %v, want the storage size %d", payload["size"], len(content))
	}
}

func TestIntegrityUnknownFileAnswers404(t *testing.T) {
	app := New(Deps{Files: files.NewMemory(), Storage: newFakeStorage(), PresignTTL: testPresignTTL})

	resp, body := fetchContent(t, app, "GET", integrityPath(files.NewID()), "")
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

func TestIntegrityStorageFailureAnswers502(t *testing.T) {
	content := patternBytes(1024)

	digest, _, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the content: %v", err)
	}

	t.Run("stat fails", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
		file := seedFile(t, metadata, store, "stat.bin", "application/octet-stream", content)
		store.statErr = storage.ErrUnavailable

		resp, body := fetchContent(t, app, "GET", integrityPath(file.ID), "")
		if resp.StatusCode != 502 {
			t.Fatalf("status %d, want 502 (body %s)", resp.StatusCode, body)
		}
		assertJSONError(t, body, "storage unavailable")
	})

	t.Run("get fails", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
		file := seedFile(t, metadata, store, "get.bin", "application/octet-stream", content)
		store.getErr = storage.ErrUnavailable

		resp, body := fetchContent(t, app, "GET", integrityPath(file.ID), "")
		if resp.StatusCode != 502 {
			t.Fatalf("status %d, want 502 (body %s)", resp.StatusCode, body)
		}
		assertJSONError(t, body, "storage unavailable")
	})

	t.Run("no storage", func(t *testing.T) {
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, PresignTTL: testPresignTTL})
		file := files.File{
			ID:          files.NewID(),
			Key:         objectKeyPrefix + files.NewID(),
			Name:        "unreachable.bin",
			Size:        int64(len(content)),
			ContentType: "application/octet-stream",
			SHA256:      digest,
			CreatedAt:   time.Now().UTC(),
		}
		if err := metadata.Add(file); err != nil {
			t.Fatalf("seeding the metadata record: %v", err)
		}

		resp, body := fetchContent(t, app, "GET", integrityPath(file.ID), "")
		if resp.StatusCode != 503 {
			t.Fatalf("status %d, want 503 (body %s)", resp.StatusCode, body)
		}
		assertJSONError(t, body, "storage unavailable")
	})
}
