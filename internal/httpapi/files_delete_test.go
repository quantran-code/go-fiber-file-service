package httpapi

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// deleteFile sends DELETE /files/{id} through the real router and returns the
// status together with the body, already read.
func deleteFile(t *testing.T, app *fiber.App, id string) (int, []byte) {
	t.Helper()

	req := httptest.NewRequest("DELETE", "/files/"+id, nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("DELETE /files/%s: %v", id, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the DELETE body: %v", err)
	}
	return resp.StatusCode, body
}

// listingIDs returns the ids every record of the listing reports.
func listingIDs(t *testing.T, app *fiber.App) []string {
	t.Helper()

	records := listFiles(t, app)
	ids := make([]string, 0, len(records))
	for _, record := range records {
		id, _ := record["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// containsID reports whether ids carries id.
func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func TestDeleteRemovesObjectAndMetadata(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, _ := uploadFile(t, app, "gone.bin", patternBytes(128))
	key := objectKeyPrefix + id

	if _, ok := store.object(key); !ok {
		t.Fatalf("storage holds no object under %q before the delete", key)
	}

	status, body := deleteFile(t, app, id)
	if status != 204 {
		t.Fatalf("DELETE status %d, want 204 (body %s)", status, body)
	}
	if len(body) != 0 {
		t.Fatalf("DELETE answered a body of %d bytes %q, want none", len(body), body)
	}
	if strings.Contains(string(body), key) {
		t.Fatalf("the DELETE body %q carries the internal object key %q", body, key)
	}

	if ids := listingIDs(t, app); containsID(ids, id) {
		t.Fatalf("the listing still carries %q after the delete", id)
	}
	if _, ok := metadata.Get(id); ok {
		t.Fatalf("the metadata store still holds %q after the delete", id)
	}
	if _, ok := store.object(key); ok {
		t.Fatalf("storage still holds the object under %q after the delete", key)
	}
	if deleted := store.deletedKeys(); len(deleted) != 1 || deleted[0] != key {
		t.Fatalf("storage deleted %v, want the single key %q: the object goes before the metadata", deleted, key)
	}
}

func TestDeleteUnknownFileAnswers404(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id := files.NewID()

	status, body := deleteFile(t, app, id)
	if status != 404 {
		t.Fatalf("DELETE status %d, want 404 (body %s)", status, body)
	}

	payload := decodeJSONObject(t, body)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != "file not found" {
		t.Fatalf("error message %q, want %q", got, "file not found")
	}
	if strings.Contains(string(body), objectKeyPrefix+id) {
		t.Fatalf("the 404 body %q carries the internal object key", body)
	}
	if deleted := store.deletedKeys(); len(deleted) != 0 {
		t.Fatalf("storage deleted %v for an unknown id, want no storage call", deleted)
	}
}

func TestDeleteStorageFailureKeepsMetadata(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, _ := uploadFile(t, app, "kept.bin", patternBytes(64))
	key := objectKeyPrefix + id
	store.deleteErr = storage.ErrUnavailable

	status, body := deleteFile(t, app, id)
	if status != 502 {
		t.Fatalf("DELETE status %d, want 502 (body %s)", status, body)
	}

	payload := decodeJSONObject(t, body)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != "storage unavailable" {
		t.Fatalf("error message %q, want %q", got, "storage unavailable")
	}
	if strings.Contains(string(body), key) {
		t.Fatalf("the 502 body %q carries the internal object key %q", body, key)
	}

	if ids := listingIDs(t, app); !containsID(ids, id) {
		t.Fatalf("the listing dropped %q after a storage failure, want it kept: %v", id, ids)
	}
	if _, ok := metadata.Get(id); !ok {
		t.Fatalf("the metadata store dropped %q after a storage failure", id)
	}
	if _, ok := store.object(key); !ok {
		t.Fatalf("storage dropped the object under %q after a failed delete", key)
	}
}

func TestDeleteTreatsMissingObjectAsDeleted(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	id, _ := uploadFile(t, app, "already-gone.bin", patternBytes(64))
	store.deleteErr = storage.ErrNotFound

	status, body := deleteFile(t, app, id)
	if status != 204 {
		t.Fatalf("DELETE status %d, want 204 when the object is already gone (body %s)", status, body)
	}
	if len(body) != 0 {
		t.Fatalf("DELETE answered a body of %d bytes %q, want none", len(body), body)
	}

	if ids := listingIDs(t, app); containsID(ids, id) {
		t.Fatalf("the listing still carries %q after the metadata was removed", id)
	}
	if _, ok := metadata.Get(id); ok {
		t.Fatalf("the metadata store still holds %q, want it removed", id)
	}
}

func TestDeleteWithoutStorageAnswers503(t *testing.T) {
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, PresignTTL: testPresignTTL})

	id := files.NewID()
	file := files.File{
		ID:          id,
		Key:         objectKeyPrefix + id,
		Name:        "unreachable.bin",
		Size:        4,
		ContentType: "application/octet-stream",
		SHA256:      strings.Repeat("a", 64),
		CreatedAt:   time.Now().UTC(),
	}
	if err := metadata.Add(file); err != nil {
		t.Fatalf("seeding the metadata record: %v", err)
	}

	status, body := deleteFile(t, app, id)
	if status != 503 {
		t.Fatalf("DELETE status %d, want 503 without storage (body %s)", status, body)
	}

	payload := decodeJSONObject(t, body)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != "storage unavailable" {
		t.Fatalf("error message %q, want %q", got, "storage unavailable")
	}

	if ids := listingIDs(t, app); !containsID(ids, id) {
		t.Fatalf("the listing dropped %q without an object store, want it kept: %v", id, ids)
	}
}
