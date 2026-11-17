package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

const testPresignTTL = 15 * time.Minute

// uploadPart describes one part of a multipart/form-data request.
type uploadPart struct {
	field       string
	fileName    string
	contentType string
	content     []byte
}

// putHookStorage runs beforePut when storage is written to, so a test can take a
// snapshot of the client side of a streaming upload at that exact moment.
type putHookStorage struct {
	*fakeStorage
	beforePut func()
}

func (s *putHookStorage) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if s.beforePut != nil {
		s.beforePut()
	}
	return s.fakeStorage.Put(ctx, key, body, size, contentType)
}

// drainPutStorage discards what it is given instead of keeping it, so the
// oversized-upload test can push more than the 100 MiB cap through the handler
// without holding the rejected payload in memory.
type drainPutStorage struct {
	*fakeStorage
	putCount int
}

func (s *drainPutStorage) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	s.putCount++
	_, err := io.Copy(io.Discard, body)
	return err
}

// zeroReader yields an endless run of zero bytes, so the large-payload tests
// never hold the payload in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// multipartUpload encodes the parts as a multipart/form-data body and returns it
// together with the request's Content-Type header value.
func multipartUpload(t *testing.T, parts ...uploadPart) ([]byte, string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for _, part := range parts {
		header := textproto.MIMEHeader{}
		disposition := fmt.Sprintf("form-data; name=%q", part.field)
		if part.fileName != "" {
			disposition += fmt.Sprintf("; filename=%q", part.fileName)
		}
		header.Set("Content-Disposition", disposition)
		if part.contentType != "" {
			header.Set("Content-Type", part.contentType)
		}

		partWriter, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("creating the %q part: %v", part.field, err)
		}
		if _, err := partWriter.Write(part.content); err != nil {
			t.Fatalf("writing the %q part: %v", part.field, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("closing the multipart body: %v", err)
	}

	return buf.Bytes(), writer.FormDataContentType()
}

// postUpload sends a multipart body to POST /files through the real router.
func postUpload(t *testing.T, app *fiber.App, body []byte, contentType string) *http.Response {
	t.Helper()

	req := httptest.NewRequest("POST", "/files", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST /files: %v", err)
	}
	return resp
}

// streamRequest runs one request through the router with the body handed over as
// a stream. app.Test is not used here because it reads the whole request body
// into memory before the handler runs, which would hide whether the handler
// streams the part.
func streamRequest(t *testing.T, app *fiber.App, method, path, contentType string, body io.Reader, size int) (int, []byte) {
	t.Helper()

	// One trivial request first, so the application has built its router before
	// its fasthttp handler is taken out of it below.
	if warm, err := app.Test(httptest.NewRequest("GET", "/health", nil)); err == nil {
		_ = warm.Body.Close()
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	if contentType != "" {
		ctx.Request.Header.SetContentType(contentType)
	}
	ctx.Request.SetBodyStream(body, size)

	handler := app.Handler()
	if handler == nil {
		t.Fatal("the application exposes no fasthttp handler")
	}
	handler(ctx)

	out := make([]byte, len(ctx.Response.Body()))
	copy(out, ctx.Response.Body())
	return ctx.Response.StatusCode(), out
}

// readJSONBody decodes the response body, closing it on the way.
func readJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding the response body %q: %v", raw, err)
	}
	return payload
}

// assertErrorBody checks the response status and its {"error": ...} body.
func assertErrorBody(t *testing.T, resp *http.Response, wantStatus int, wantMessage string) {
	t.Helper()

	if resp.StatusCode != wantStatus {
		payload := readJSONBody(t, resp)
		t.Fatalf("status %d, want %d (body %v)", resp.StatusCode, wantStatus, payload)
	}

	payload := readJSONBody(t, resp)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != wantMessage {
		t.Fatalf("error message %q, want %q", got, wantMessage)
	}
}

// assertJSONError checks a raw response body against an error message.
func assertJSONError(t *testing.T, body []byte, wantMessage string) {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding the response body %q: %v", body, err)
	}
	if got, _ := payload["error"].(string); got != wantMessage {
		t.Fatalf("error message %q, want %q", got, wantMessage)
	}
}

func TestUploadReturnsMetadataAndStoresObject(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := []byte("the file service keeps these bytes")
	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    "notes.txt",
		contentType: "text/plain; charset=utf-8",
		content:     content,
	})

	resp := postUpload(t, app, body, contentType)
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding the response body %q: %v", raw, err)
	}

	for _, field := range []string{"id", "name", "size", "contentType", "sha256", "createdAt"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("response %s carries no %q field", raw, field)
		}
	}
	for _, internal := range []string{"key"} {
		if _, ok := payload[internal]; ok {
			t.Fatalf("response %s exposes the internal %q field", raw, internal)
		}
	}
	if len(payload) != 6 {
		t.Fatalf("response %s carries %d fields, want 6", raw, len(payload))
	}

	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("response %s carries no id", raw)
	}
	if got := payload["name"]; got != "notes.txt" {
		t.Fatalf("name %v, want %q", got, "notes.txt")
	}
	if got, ok := payload["size"].(float64); !ok || int64(got) != int64(len(content)) {
		t.Fatalf("size %v, want %d", payload["size"], len(content))
	}
	if got := payload["contentType"]; got != "text/plain; charset=utf-8" {
		t.Fatalf("contentType %v, want %q", got, "text/plain; charset=utf-8")
	}
	if got, _ := payload["createdAt"].(string); got == "" {
		t.Fatalf("response %s carries no createdAt", raw)
	}

	key := objectKeyPrefix + id
	stored, ok := store.object(key)
	if !ok {
		t.Fatalf("storage holds no object under %q", key)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored bytes %q, want %q", stored, content)
	}
	if got := store.contentTypeOf(key); got != "text/plain; charset=utf-8" {
		t.Fatalf("stored content type %q, want %q", got, "text/plain; charset=utf-8")
	}
	puts := store.recordedPuts()
	if len(puts) != 1 {
		t.Fatalf("storage recorded %d puts, want 1", len(puts))
	}
	if puts[0].key != key {
		t.Fatalf("storage received key %q, want %q", puts[0].key, key)
	}

	list := metadata.List()
	if len(list) != 1 {
		t.Fatalf("metadata store holds %d records, want 1", len(list))
	}
	record := list[0]
	if record.ID != id {
		t.Fatalf("metadata id %q, want %q", record.ID, id)
	}
	if record.Key != key {
		t.Fatalf("metadata key %q, want %q", record.Key, key)
	}
	if record.Name != "notes.txt" {
		t.Fatalf("metadata name %q, want %q", record.Name, "notes.txt")
	}
	if record.Size != int64(len(content)) {
		t.Fatalf("metadata size %d, want %d", record.Size, len(content))
	}
	if record.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("metadata content type %q, want %q", record.ContentType, "text/plain; charset=utf-8")
	}
}

func TestUploadStoresSHA256(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := []byte("the digest is taken over exactly these bytes")
	want, wantSize, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the expected content: %v", err)
	}

	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    "digest.txt",
		contentType: "text/plain",
		content:     content,
	})

	resp := postUpload(t, app, body, contentType)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	payload := readJSONBody(t, resp)

	got, _ := payload["sha256"].(string)
	if got != want {
		t.Fatalf("response sha256 %q, want %q", got, want)
	}
	decoded, err := hex.DecodeString(got)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != got {
		t.Fatalf("response sha256 %q is not 64 lowercase hex characters", got)
	}

	list := metadata.List()
	if len(list) != 1 {
		t.Fatalf("metadata store holds %d records, want 1", len(list))
	}
	if list[0].SHA256 != want {
		t.Fatalf("metadata sha256 %q, want %q", list[0].SHA256, want)
	}
	if list[0].Size != wantSize {
		t.Fatalf("metadata size %d, want the %d bytes that were hashed", list[0].Size, wantSize)
	}

	if puts := store.recordedPuts(); len(puts) != 1 {
		t.Fatalf("storage recorded %d puts, want exactly one streaming upload", len(puts))
	}
	if gets := store.recordedGets(); len(gets) != 0 {
		t.Fatalf("storage recorded %d gets for an upload, want 0: the digest is taken in the same pass as the write", len(gets))
	}
}

func TestUploadRejectsMissingFilePart(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	body, contentType := multipartUpload(t, uploadPart{field: "other", content: []byte("ignored")})

	resp := postUpload(t, app, body, contentType)
	assertErrorBody(t, resp, 400, "invalid request")

	if puts := store.recordedPuts(); len(puts) != 0 {
		t.Fatalf("storage recorded %d puts for a rejected upload, want 0", len(puts))
	}
	if list := metadata.List(); len(list) != 0 {
		t.Fatalf("metadata store holds %d records after a rejected upload, want 0", len(list))
	}
}

func TestUploadRejectsDuplicateFilePart(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	body, contentType := multipartUpload(t,
		uploadPart{field: "file", fileName: "first.txt", contentType: "text/plain", content: []byte("first")},
		uploadPart{field: "file", fileName: "second.txt", contentType: "text/plain", content: []byte("second")},
	)

	resp := postUpload(t, app, body, contentType)
	assertErrorBody(t, resp, 400, "invalid request")

	if list := metadata.List(); len(list) != 0 {
		t.Fatalf("metadata store holds %d records after a rejected upload, want 0", len(list))
	}
	if deleted := store.deletedKeys(); len(deleted) != 1 {
		t.Fatalf("storage deleted %d keys, want the one object the rejected request stored", len(deleted))
	}
}

func TestUploadRejectsOversizedFile(t *testing.T) {
	store := &drainPutStorage{fakeStorage: newFakeStorage()}
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()

	go func() {
		part, err := writer.CreateFormFile("file", "too-large.bin")
		if err == nil {
			_, err = io.CopyN(part, zeroReader{}, maxUploadBytes+1)
		}
		if err == nil {
			err = writer.Close()
		}
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	status, body := streamRequest(t, app, "POST", "/files", contentType, pr, -1)
	if status != 413 {
		t.Fatalf("status %d, want 413 (body %s)", status, body)
	}
	assertJSONError(t, body, "file too large")

	if list := metadata.List(); len(list) != 0 {
		t.Fatalf("metadata store holds %d records after a rejected upload, want 0", len(list))
	}
	if store.putCount != 1 {
		t.Fatalf("storage received %d uploads, want the single aborted one", store.putCount)
	}
	if puts := store.recordedPuts(); len(puts) != 0 {
		t.Fatalf("storage recorded %d stored objects for a rejected upload, want 0", len(puts))
	}
}

func TestUploadStreamsRequestBodyIntoStorage(t *testing.T) {
	const (
		payloadSize   = 32 << 20
		bufferedLimit = 1 << 20
	)

	store := newFakeStorage()

	pr, pw := io.Pipe()
	counter := &countingReadCloser{r: pr}

	readAtPut := int64(-1)
	streaming := &putHookStorage{fakeStorage: store}
	streaming.beforePut = func() {
		readAtPut = counter.n
	}

	app := New(Deps{Files: files.NewMemory(), Storage: streaming, PresignTTL: testPresignTTL})

	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()

	go func() {
		part, err := writer.CreateFormFile("file", "large.bin")
		if err == nil {
			_, err = io.CopyN(part, zeroReader{}, payloadSize)
		}
		if err == nil {
			err = writer.Close()
		}
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	status, body := streamRequest(t, app, "POST", "/files", contentType, counter, -1)
	if status != 201 {
		t.Fatalf("status %d, want 201 (body %s)", status, body)
	}
	if readAtPut < 0 {
		t.Fatal("storage.Put was never called")
	}
	if readAtPut >= bufferedLimit {
		t.Fatalf("%d bytes of the request body had been read when storage.Put started, want fewer than %d: the part is being buffered", readAtPut, bufferedLimit)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding the response body %s: %v", body, err)
	}
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("response %s carries no id", body)
	}

	stored, ok := store.object(objectKeyPrefix + id)
	if !ok {
		t.Fatalf("storage holds no object under %q", objectKeyPrefix+id)
	}
	if len(stored) != payloadSize {
		t.Fatalf("stored %d bytes, want %d", len(stored), payloadSize)
	}
}

func TestUploadWithoutStorageAnswers503(t *testing.T) {
	app := New(Deps{Files: files.NewMemory(), PresignTTL: testPresignTTL})

	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    "hello.txt",
		contentType: "text/plain",
		content:     []byte("hello"),
	})

	resp := postUpload(t, app, body, contentType)
	assertErrorBody(t, resp, 503, "storage unavailable")
}

func TestUploadStorageFailureAnswers502(t *testing.T) {
	store := newFakeStorage()
	store.putErr = storage.ErrUnavailable
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    "hello.txt",
		contentType: "text/plain",
		content:     []byte("hello"),
	})

	resp := postUpload(t, app, body, contentType)
	assertErrorBody(t, resp, 502, "storage unavailable")

	if list := metadata.List(); len(list) != 0 {
		t.Fatalf("metadata store holds %d records after a failed upload, want 0", len(list))
	}
}

func TestUploadDefaultsContentType(t *testing.T) {
	store := newFakeStorage()
	app := New(Deps{Files: files.NewMemory(), Storage: store, PresignTTL: testPresignTTL})

	body, contentType := multipartUpload(t, uploadPart{
		field:    "file",
		fileName: "raw.bin",
		content:  []byte{0x01, 0x02, 0x03},
	})

	resp := postUpload(t, app, body, contentType)
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding the response body %q: %v", raw, err)
	}
	if got := payload["contentType"]; got != "application/octet-stream" {
		t.Fatalf("contentType %v, want %q", got, "application/octet-stream")
	}

	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("response %s carries no id", raw)
	}
	if got := store.contentTypeOf(objectKeyPrefix + id); got != "application/octet-stream" {
		t.Fatalf("stored content type %q, want %q", got, "application/octet-stream")
	}
}
