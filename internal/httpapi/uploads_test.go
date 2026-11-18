package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/quantran-code/go-fiber-file-service/internal/config"
	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// presignCall records one call to presignRecorder.PresignPut.
type presignCall struct {
	key         string
	ttl         time.Duration
	contentType string
}

// presignRecorder is a fakeStorage whose PresignPut records the key, lifetime
// and content type a signature was minted for and answers a distinguishable URL.
// fakeStorage's own PresignPut returns a placeholder and records nothing, which
// would leave the presign route's success path unobservable.
type presignRecorder struct {
	*fakeStorage
	url        string
	presignErr error
	calls      []presignCall
}

var _ storage.Store = (*presignRecorder)(nil)

// PresignPut records the call and answers the configured URL, or a placeholder
// built from the key when none is configured.
func (p *presignRecorder) PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error) {
	p.calls = append(p.calls, presignCall{key: key, ttl: ttl, contentType: contentType})
	if p.presignErr != nil {
		return "", p.presignErr
	}
	if p.url != "" {
		return p.url, nil
	}
	return "https://storage.invalid/" + key + "?X-Amz-Signature=recorded", nil
}

// recordedPresigns returns a copy of the recorded PresignPut calls.
func (p *presignRecorder) recordedPresigns() []presignCall {
	return append([]presignCall(nil), p.calls...)
}

// newPresignRecorder returns a recorder over an empty in-memory store.
func newPresignRecorder() *presignRecorder {
	return &presignRecorder{fakeStorage: newFakeStorage()}
}

// postJSON sends payload to path as a JSON body through the real router.
func postJSON(t *testing.T, app *fiber.App, path string, payload any) *http.Response {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the %s body: %v", path, err)
	}
	return postRawBody(t, app, path, body)
}

// postRawBody sends body to path as application/json, so a test can hand over a
// body that is not JSON at all.
func postRawBody(t *testing.T, app *fiber.App, path string, body []byte) *http.Response {
	t.Helper()

	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// presignBody posts body to the presign route and returns the decoded 201
// payload, failing when the route answers anything else.
func presignBody(t *testing.T, app *fiber.App, body map[string]any) map[string]any {
	t.Helper()

	resp := postJSON(t, app, "/uploads/presign", body)
	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("presign status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	return readJSONBody(t, resp)
}

// presignFile asks for a signed PUT for a file of the given name, content type
// and size and returns the decoded 201 body.
func presignFile(t *testing.T, app *fiber.App, name, contentType string, size int64) map[string]any {
	t.Helper()
	return presignBody(t, app, map[string]any{
		"name":        name,
		"contentType": contentType,
		"size":        size,
	})
}

// keySafeName splits a presigned key into its id segment and its safe-name
// segment, failing when the key is not shaped uploads/<uuid>/<safe-name>.
func keySafeName(t *testing.T, key string) (id string, safeName string) {
	t.Helper()

	rest, ok := strings.CutPrefix(key, uploadsKeyPrefix)
	if !ok {
		t.Fatalf("key %q does not start with %q", key, uploadsKeyPrefix)
	}

	id, safeName, ok = strings.Cut(rest, "/")
	if !ok {
		t.Fatalf("key %q carries no safe-name segment", key)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("the id segment %q of key %q is not a UUID: %v", id, key, err)
	}
	if strings.Contains(safeName, "/") {
		t.Fatalf("the safe-name segment %q of key %q carries a separator", safeName, key)
	}
	return id, safeName
}

// seedPresignedObject stores content under a presigned-style key and returns the
// key together with its safe-name segment, so a completion test does not have to
// PUT through a signed URL.
func seedPresignedObject(t *testing.T, store *fakeStorage, name, contentType string, content []byte) (key, safeName string) {
	t.Helper()

	key = uploadsKeyPrefix + files.NewID() + "/" + name
	store.seed(key, content, contentType)
	return key, name
}

func TestPresignUploadReturnsSignedURLAndHeaders(t *testing.T) {
	store := newPresignRecorder()
	app := New(Deps{Files: files.NewMemory(), Storage: store})

	content := patternBytes(1024)
	payload := presignFile(t, app, "shot.png", "image/png", int64(len(content)))

	if len(payload) != 4 {
		t.Fatalf("presign body %v carries %d fields, want 4", payload, len(payload))
	}
	for _, field := range []string{"uploadUrl", "key", "expiresAt", "requiredHeaders"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("presign body %v carries no %q field", payload, field)
		}
	}
	for _, internal := range []string{"accessKey", "secretKey", "sessionToken", "bucket", "endpoint", "credentials"} {
		if _, ok := payload[internal]; ok {
			t.Fatalf("presign body %v exposes %q", payload, internal)
		}
	}

	key, _ := payload["key"].(string)
	if key == "" {
		t.Fatalf("presign body %v carries no key", payload)
	}
	if _, safeName := keySafeName(t, key); safeName != "shot.png" {
		t.Fatalf("the safe-name segment of key %q is %q, want %q", key, safeName, "shot.png")
	}

	calls := store.recordedPresigns()
	if len(calls) != 1 {
		t.Fatalf("storage recorded %d presign calls, want 1", len(calls))
	}
	if calls[0].key != key {
		t.Fatalf("the signature was minted for key %q, want the returned key %q", calls[0].key, key)
	}
	if calls[0].contentType != "image/png" {
		t.Fatalf("the signature covers content type %q, want %q", calls[0].contentType, "image/png")
	}
	if calls[0].ttl != config.DefaultPresignTTL {
		t.Fatalf("the signature is valid for %s, want the %s default", calls[0].ttl, config.DefaultPresignTTL)
	}

	uploadURL, _ := payload["uploadUrl"].(string)
	wantURL := "https://storage.invalid/" + key + "?X-Amz-Signature=recorded"
	if uploadURL != wantURL {
		t.Fatalf("uploadUrl %q, want the URL storage signed for %q: %q", uploadURL, key, wantURL)
	}

	headers, ok := payload["requiredHeaders"].(map[string]any)
	if !ok {
		t.Fatalf("requiredHeaders %v is not a JSON object", payload["requiredHeaders"])
	}
	if len(headers) != 2 {
		t.Fatalf("requiredHeaders %v carries %d entries, want Content-Type and Content-Length", headers, len(headers))
	}
	if got := headers["Content-Type"]; got != "image/png" {
		t.Fatalf("requiredHeaders Content-Type %v, want %q", got, "image/png")
	}
	if got, want := headers["Content-Length"], strconv.Itoa(len(content)); got != want {
		t.Fatalf("requiredHeaders Content-Length %v, want %q", got, want)
	}

	expiresAt, _ := payload["expiresAt"].(string)
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not an RFC 3339 timestamp: %v", expiresAt, err)
	}
	if lifetime := time.Until(expiry); lifetime < 899*time.Second || lifetime > 901*time.Second {
		t.Fatalf("expiresAt is %s away, want about %s", lifetime, config.DefaultPresignTTL)
	}

	if puts := store.recordedPuts(); len(puts) != 0 {
		t.Fatalf("storage recorded %d puts for a presign, want 0", len(puts))
	}
	if deleted := store.deletedKeys(); len(deleted) != 0 {
		t.Fatalf("storage deleted %d keys for a presign, want 0", len(deleted))
	}
}

func TestPresignUploadUsesTheConfiguredTTL(t *testing.T) {
	const ttl = 2 * time.Minute

	store := newPresignRecorder()
	app := New(Deps{Files: files.NewMemory(), Storage: store, PresignTTL: ttl})

	payload := presignFile(t, app, "short-lived.txt", "text/plain", 8)

	calls := store.recordedPresigns()
	if len(calls) != 1 {
		t.Fatalf("storage recorded %d presign calls, want 1", len(calls))
	}
	if calls[0].ttl != ttl {
		t.Fatalf("the signature is valid for %s, want the configured %s", calls[0].ttl, ttl)
	}

	expiresAt, _ := payload["expiresAt"].(string)
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not an RFC 3339 timestamp: %v", expiresAt, err)
	}
	if lifetime := time.Until(expiry); lifetime < ttl-10*time.Second || lifetime > ttl {
		t.Fatalf("expiresAt is %s away, want about the configured %s", lifetime, ttl)
	}
}

func TestPresignUploadMintsServerOwnedKeys(t *testing.T) {
	store := newPresignRecorder()
	app := New(Deps{Files: files.NewMemory(), Storage: store})

	// A caller-supplied key, url and bucket are never read: the key is always
	// built by the service and the signature never points at an existing object.
	body := map[string]any{
		"name":        "shot.png",
		"contentType": "image/png",
		"size":        1024,
		"key":         "uploads/existing/object.png",
		"url":         "https://evil.invalid/object",
		"bucket":      "someone-elses-bucket",
	}

	first := presignBody(t, app, body)
	second := presignBody(t, app, body)

	firstKey, _ := first["key"].(string)
	secondKey, _ := second["key"].(string)
	if firstKey == secondKey {
		t.Fatalf("two presigns returned the same key %q, want two distinct keys", firstKey)
	}
	if _, safeName := keySafeName(t, firstKey); safeName != "shot.png" {
		t.Fatalf("the safe-name segment of key %q is %q, want %q", firstKey, safeName, "shot.png")
	}
	if _, safeName := keySafeName(t, secondKey); safeName != "shot.png" {
		t.Fatalf("the safe-name segment of key %q is %q, want %q", secondKey, safeName, "shot.png")
	}

	uploadURL, _ := first["uploadUrl"].(string)
	if strings.Contains(uploadURL, "evil.invalid") {
		t.Fatalf("uploadUrl %q reflects the caller-supplied url", uploadURL)
	}
	if !strings.Contains(uploadURL, firstKey) {
		t.Fatalf("uploadUrl %q is not the URL signed for the returned key %q", uploadURL, firstKey)
	}
}

func TestPresignUploadRejectsInvalidBodies(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "missing name", body: map[string]any{"contentType": "image/png", "size": 8}},
		{name: "blank name", body: map[string]any{"name": "   ", "contentType": "image/png", "size": 8}},
		{name: "name too long", body: map[string]any{"name": strings.Repeat("a", maxUploadNameLength+1), "contentType": "image/png", "size": 8}},
		{name: "name without a usable segment", body: map[string]any{"name": "..", "contentType": "image/png", "size": 8}},
		{name: "missing content type", body: map[string]any{"name": "shot.png", "size": 8}},
		{name: "unusable content type", body: map[string]any{"name": "shot.png", "contentType": "not a media type", "size": 8}},
		{name: "zero size", body: map[string]any{"name": "shot.png", "contentType": "image/png", "size": 0}},
		{name: "negative size", body: map[string]any{"name": "shot.png", "contentType": "image/png", "size": -1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPresignRecorder()
			app := New(Deps{Files: files.NewMemory(), Storage: store})

			resp := postJSON(t, app, "/uploads/presign", tc.body)
			assertErrorBody(t, resp, 400, "invalid request")

			if calls := store.recordedPresigns(); len(calls) != 0 {
				t.Fatalf("storage recorded %d presigns for a rejected body, want 0", len(calls))
			}
		})
	}
}

func TestPresignUploadRejectsANonJSONBody(t *testing.T) {
	store := newPresignRecorder()
	app := New(Deps{Files: files.NewMemory(), Storage: store})

	resp := postRawBody(t, app, "/uploads/presign", []byte("this is not JSON"))
	assertErrorBody(t, resp, 400, "invalid request")

	if calls := store.recordedPresigns(); len(calls) != 0 {
		t.Fatalf("storage recorded %d presigns for a rejected body, want 0", len(calls))
	}
}

func TestPresignUploadRejectsOversizedFile(t *testing.T) {
	store := newPresignRecorder()
	app := New(Deps{Files: files.NewMemory(), Storage: store})

	resp := postJSON(t, app, "/uploads/presign", map[string]any{
		"name":        "huge.bin",
		"contentType": "application/octet-stream",
		"size":        maxUploadBytes + 1,
	})
	assertErrorBody(t, resp, 413, "file too large")

	if calls := store.recordedPresigns(); len(calls) != 0 {
		t.Fatalf("storage recorded %d presigns for an oversized file, want 0", len(calls))
	}
}

func TestPresignUploadWithoutStorageAnswers503(t *testing.T) {
	app := New(Deps{Files: files.NewMemory()})

	resp := postJSON(t, app, "/uploads/presign", map[string]any{
		"name":        "shot.png",
		"contentType": "image/png",
		"size":        1024,
	})
	assertErrorBody(t, resp, 503, "storage unavailable")
}

func TestPresignUploadStorageFailureAnswers502(t *testing.T) {
	store := newPresignRecorder()
	store.presignErr = storage.ErrUnavailable
	app := New(Deps{Files: files.NewMemory(), Storage: store})

	resp := postJSON(t, app, "/uploads/presign", map[string]any{
		"name":        "shot.png",
		"contentType": "image/png",
		"size":        1024,
	})
	assertErrorBody(t, resp, 502, "storage unavailable")

	if puts := store.recordedPuts(); len(puts) != 0 {
		t.Fatalf("storage recorded %d puts for a failed presign, want 0", len(puts))
	}
}

func TestCompleteUploadRegistersMetadataAndServesContent(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	content := patternBytes(48 << 10)
	key, safeName := seedPresignedObject(t, store, "direct.bin", "application/octet-stream", content)

	digest, size, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the stored content: %v", err)
	}

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         safeName,
		"contentType":  "application/octet-stream",
		"expectedSize": size,
		"sha256":       digest,
	})
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("complete status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	payload := readJSONBody(t, resp)

	if len(payload) != 6 {
		t.Fatalf("complete body %v carries %d fields, want the six of POST /files", payload, len(payload))
	}
	for _, field := range []string{"id", "name", "size", "contentType", "sha256", "createdAt"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("complete body %v carries no %q field", payload, field)
		}
	}
	if _, ok := payload["key"]; ok {
		t.Fatalf("complete body %v exposes the internal key", payload)
	}
	if got := payload["name"]; got != safeName {
		t.Fatalf("complete name %v, want %q", got, safeName)
	}
	if got, ok := payload["size"].(float64); !ok || int64(got) != size {
		t.Fatalf("complete size %v, want the %d stored bytes", payload["size"], size)
	}
	if got := payload["sha256"]; got != digest {
		t.Fatalf("complete sha256 %v, want the digest computed over the stored bytes %q", got, digest)
	}
	if got := payload["contentType"]; got != "application/octet-stream" {
		t.Fatalf("complete contentType %v, want %q", got, "application/octet-stream")
	}
	if got, _ := payload["createdAt"].(string); got == "" {
		t.Fatalf("complete body %v carries no createdAt", payload)
	}

	records := metadata.List()
	if len(records) != 1 {
		t.Fatalf("metadata store holds %d records, want 1", len(records))
	}
	record := records[0]
	if record.Key != key {
		t.Fatalf("metadata key %q, want %q", record.Key, key)
	}
	if record.Name != safeName {
		t.Fatalf("metadata name %q, want %q", record.Name, safeName)
	}
	if record.Size != size {
		t.Fatalf("metadata size %d, want %d", record.Size, size)
	}
	if record.ContentType != "application/octet-stream" {
		t.Fatalf("metadata content type %q, want %q", record.ContentType, "application/octet-stream")
	}
	if record.SHA256 != digest {
		t.Fatalf("metadata sha256 %q, want %q", record.SHA256, digest)
	}

	if puts := store.recordedPuts(); len(puts) != 0 {
		t.Fatalf("storage recorded %d puts for a completion, want 0: the client already uploaded the bytes", len(puts))
	}

	contentResp, body := fetchContent(t, app, "GET", contentPath(record.ID), "")
	if contentResp.StatusCode != 200 {
		t.Fatalf("content status %d, want 200 (body %s)", contentResp.StatusCode, body)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("content served %d bytes that differ from the %d stored bytes", len(body), len(content))
	}
	if got := contentResp.Header.Get("X-Content-SHA256"); got != digest {
		t.Fatalf("X-Content-SHA256 %q, want the computed digest %q", got, digest)
	}
}

func TestCompleteUploadComputesTheDigestItself(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	content := []byte("the service hashes the stored object, not the client")
	key, safeName := seedPresignedObject(t, store, "no-digest.txt", "text/plain", content)

	want, size, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the stored content: %v", err)
	}

	// No sha256 in the body: the recorded digest is still the server's own.
	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         safeName,
		"contentType":  "text/plain",
		"expectedSize": size,
	})
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("complete status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	payload := readJSONBody(t, resp)

	if got := payload["sha256"]; got != want {
		t.Fatalf("complete sha256 %v, want the computed digest %q", got, want)
	}
	records := metadata.List()
	if len(records) != 1 {
		t.Fatalf("metadata store holds %d records, want 1", len(records))
	}
	if records[0].SHA256 != want {
		t.Fatalf("metadata sha256 %q, want the computed digest %q", records[0].SHA256, want)
	}
}

func TestCompleteUploadAcceptsAnUpperCaseDigest(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	content := patternBytes(512)
	key, safeName := seedPresignedObject(t, store, "upper.bin", "application/octet-stream", content)

	digest, size, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the stored content: %v", err)
	}

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         safeName,
		"contentType":  "application/octet-stream",
		"expectedSize": size,
		"sha256":       strings.ToUpper(digest),
	})
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("complete status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	payload := readJSONBody(t, resp)

	if got := payload["sha256"]; got != digest {
		t.Fatalf("complete sha256 %v, want the lowercase computed digest %q", got, digest)
	}
}

func TestCompleteUploadFallsBackToTheRequestContentType(t *testing.T) {
	t.Run("request content type", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store})

		content := []byte("no content type was recorded for this object")
		key, safeName := seedPresignedObject(t, store, "untyped.bin", "", content)

		resp := postJSON(t, app, "/uploads/complete", map[string]any{
			"key":          key,
			"name":         safeName,
			"contentType":  "application/octet-stream",
			"expectedSize": len(content),
		})
		if resp.StatusCode != 201 {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("complete status %d, want 201 (body %s)", resp.StatusCode, raw)
		}
		payload := readJSONBody(t, resp)

		if got := payload["contentType"]; got != "application/octet-stream" {
			t.Fatalf("complete contentType %v, want the request content type %q", got, "application/octet-stream")
		}
	})

	t.Run("default content type", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store})

		content := []byte("neither the object nor the request names a type")
		key, safeName := seedPresignedObject(t, store, "untyped.bin", "", content)

		resp := postJSON(t, app, "/uploads/complete", map[string]any{
			"key":          key,
			"name":         safeName,
			"expectedSize": len(content),
		})
		if resp.StatusCode != 201 {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("complete status %d, want 201 (body %s)", resp.StatusCode, raw)
		}
		payload := readJSONBody(t, resp)

		if got := payload["contentType"]; got != defaultContentType {
			t.Fatalf("complete contentType %v, want the %q default", got, defaultContentType)
		}
	})
}

func TestCompleteUploadRejectsInvalidKeys(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "too long", key: uploadsKeyPrefix + strings.Repeat("a", maxUploadKeyLength)},
		{name: "leading slash", key: "/uploads/abc/shot.png"},
		{name: "dot-dot segment", key: "uploads/../../etc/passwd"},
		{name: "url", key: "https://storage.invalid/uploads/abc/shot.png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStorage()
			metadata := files.NewMemory()
			app := New(Deps{Files: metadata, Storage: store})

			resp := postJSON(t, app, "/uploads/complete", map[string]any{
				"key":          tc.key,
				"name":         "shot.png",
				"contentType":  "image/png",
				"expectedSize": 8,
			})
			assertErrorBody(t, resp, 400, "invalid request")

			if records := metadata.List(); len(records) != 0 {
				t.Fatalf("metadata store holds %d records for a rejected key, want 0", len(records))
			}
			if gets := store.recordedGets(); len(gets) != 0 {
				t.Fatalf("storage read the object %d times for a rejected key, want 0", len(gets))
			}
			if deleted := store.deletedKeys(); len(deleted) != 0 {
				t.Fatalf("storage deleted %d keys for a rejected key, want 0", len(deleted))
			}
		})
	}
}

func TestCompleteUploadRejectsAMismatchedName(t *testing.T) {
	cases := []struct {
		name        string
		requestName string
	}{
		{name: "another name", requestName: "other.png"},
		{name: "empty name", requestName: ""},
		{name: "path in the name", requestName: "nested/shot.png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStorage()
			metadata := files.NewMemory()
			app := New(Deps{Files: metadata, Storage: store})

			content := []byte("matching bytes")
			key, _ := seedPresignedObject(t, store, "shot.png", "image/png", content)

			resp := postJSON(t, app, "/uploads/complete", map[string]any{
				"key":          key,
				"name":         tc.requestName,
				"contentType":  "image/png",
				"expectedSize": len(content),
			})
			assertErrorBody(t, resp, 400, "invalid request")

			if records := metadata.List(); len(records) != 0 {
				t.Fatalf("metadata store holds %d records for a mismatched name, want 0", len(records))
			}
		})
	}
}

func TestCompleteUploadRejectsABadBody(t *testing.T) {
	key := uploadsKeyPrefix + files.NewID() + "/shot.png"

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "zero expected size", mutate: func(body map[string]any) { body["expectedSize"] = 0 }},
		{name: "negative expected size", mutate: func(body map[string]any) { body["expectedSize"] = -1 }},
		{name: "expected size above the cap", mutate: func(body map[string]any) { body["expectedSize"] = maxUploadBytes + 1 }},
		{name: "short sha256", mutate: func(body map[string]any) { body["sha256"] = "abc123" }},
		{name: "non hex sha256", mutate: func(body map[string]any) { body["sha256"] = strings.Repeat("z", 64) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStorage()
			metadata := files.NewMemory()
			app := New(Deps{Files: metadata, Storage: store})

			body := map[string]any{
				"key":          key,
				"name":         "shot.png",
				"contentType":  "image/png",
				"expectedSize": 8,
				"sha256":       strings.Repeat("a", 64),
			}
			tc.mutate(body)

			resp := postJSON(t, app, "/uploads/complete", body)
			assertErrorBody(t, resp, 400, "invalid request")

			if records := metadata.List(); len(records) != 0 {
				t.Fatalf("metadata store holds %d records for a rejected body, want 0", len(records))
			}
			if gets := store.recordedGets(); len(gets) != 0 {
				t.Fatalf("storage read the object %d times for a rejected body, want 0", len(gets))
			}
		})
	}
}

func TestCompleteUploadUnknownObjectAnswers404(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	key := uploadsKeyPrefix + files.NewID() + "/missing.bin"

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         "missing.bin",
		"contentType":  "application/octet-stream",
		"expectedSize": 4,
	})
	assertErrorBody(t, resp, 404, "upload not found")

	if records := metadata.List(); len(records) != 0 {
		t.Fatalf("metadata store holds %d records for a missing object, want 0", len(records))
	}
}

func TestCompleteUploadSizeMismatchAnswers409(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	content := patternBytes(64)
	key, safeName := seedPresignedObject(t, store, "size.bin", "application/octet-stream", content)

	digest, size, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the stored content: %v", err)
	}

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         safeName,
		"contentType":  "application/octet-stream",
		"expectedSize": size + 1,
		"sha256":       digest,
	})
	assertErrorBody(t, resp, 409, "upload mismatch")

	if records := metadata.List(); len(records) != 0 {
		t.Fatalf("metadata store holds %d records after a size mismatch, want 0", len(records))
	}
}

func TestCompleteUploadDigestMismatchAnswers409(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store})

	content := patternBytes(64)
	key, safeName := seedPresignedObject(t, store, "digest.bin", "application/octet-stream", content)

	// Same length, different bytes: only the digest can tell them apart.
	tampered := append([]byte(nil), content...)
	for i := range tampered {
		tampered[i] ^= 0xff
	}
	declared, size, err := sha256Hex(bytes.NewReader(tampered))
	if err != nil {
		t.Fatalf("hashing the declared content: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("the declared content is %d bytes, want the stored %d", size, len(content))
	}

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          key,
		"name":         safeName,
		"contentType":  "application/octet-stream",
		"expectedSize": size,
		"sha256":       declared,
	})
	assertErrorBody(t, resp, 409, "upload mismatch")

	if records := metadata.List(); len(records) != 0 {
		t.Fatalf("metadata store holds %d records after a digest mismatch, want 0", len(records))
	}
}

func TestCompleteUploadStorageFailureAnswers502(t *testing.T) {
	content := patternBytes(64)

	t.Run("stat fails", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store})

		key, safeName := seedPresignedObject(t, store, "stat.bin", "application/octet-stream", content)
		store.statErr = storage.ErrUnavailable

		resp := postJSON(t, app, "/uploads/complete", map[string]any{
			"key":          key,
			"name":         safeName,
			"contentType":  "application/octet-stream",
			"expectedSize": len(content),
		})
		assertErrorBody(t, resp, 502, "storage unavailable")

		if records := metadata.List(); len(records) != 0 {
			t.Fatalf("metadata store holds %d records after a storage failure, want 0", len(records))
		}
	})

	t.Run("get fails", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store})

		key, safeName := seedPresignedObject(t, store, "get.bin", "application/octet-stream", content)
		store.getErr = storage.ErrUnavailable

		resp := postJSON(t, app, "/uploads/complete", map[string]any{
			"key":          key,
			"name":         safeName,
			"contentType":  "application/octet-stream",
			"expectedSize": len(content),
		})
		assertErrorBody(t, resp, 502, "storage unavailable")

		if records := metadata.List(); len(records) != 0 {
			t.Fatalf("metadata store holds %d records after a storage failure, want 0", len(records))
		}
	})
}

func TestCompleteUploadWithoutStorageAnswers503(t *testing.T) {
	app := New(Deps{Files: files.NewMemory()})

	resp := postJSON(t, app, "/uploads/complete", map[string]any{
		"key":          uploadsKeyPrefix + files.NewID() + "/shot.png",
		"name":         "shot.png",
		"contentType":  "image/png",
		"expectedSize": 8,
	})
	assertErrorBody(t, resp, 503, "storage unavailable")
}
