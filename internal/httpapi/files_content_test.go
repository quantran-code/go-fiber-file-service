package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// contentPath is the route that serves one file's bytes.
func contentPath(id string) string {
	return "/files/" + id + "/content"
}

// patternBytes builds a deterministic payload, so a wrong offset or length shows
// up as a difference instead of matching by accident.
func patternBytes(size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	return content
}

// seedFile stores content straight in the fake object store and records its
// metadata, so the content tests do not depend on the upload path.
func seedFile(t *testing.T, metadata *files.Memory, store *fakeStorage, name, contentType string, content []byte) files.File {
	t.Helper()

	id := files.NewID()
	key := objectKeyPrefix + id
	store.seed(key, content, contentType)

	digest, _, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the seeded content of %q: %v", name, err)
	}

	file := files.File{
		ID:          id,
		Key:         key,
		Name:        name,
		Size:        int64(len(content)),
		ContentType: contentType,
		SHA256:      digest,
		CreatedAt:   time.Now().UTC(),
	}
	if err := metadata.Add(file); err != nil {
		t.Fatalf("seeding the metadata of %q: %v", name, err)
	}
	return file
}

// fetchContent sends one content request through the real router and returns the
// response together with its body, already read.
func fetchContent(t *testing.T, app *fiber.App, method, path, rangeHeader string, msTimeout ...int) (*http.Response, []byte) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if rangeHeader != "" {
		req.Header.Set(fiber.HeaderRange, rangeHeader)
	}

	resp, err := app.Test(req, msTimeout...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the %s %s body: %v", method, path, err)
	}
	return resp, body
}

func TestUploadThenDownloadRoundTrip(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := patternBytes(68 << 10)

	body, contentType := multipartUpload(t, uploadPart{
		field:       "file",
		fileName:    "round-trip.bin",
		contentType: "application/octet-stream",
		content:     content,
	})

	upload := postUpload(t, app, body, contentType)
	if upload.StatusCode != 201 {
		raw, _ := io.ReadAll(upload.Body)
		upload.Body.Close()
		t.Fatalf("upload status %d, want 201 (body %s)", upload.StatusCode, raw)
	}

	payload := readJSONBody(t, upload)
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("upload response %v carries no id", payload)
	}

	resp, downloaded := fetchContent(t, app, "GET", contentPath(id), "")
	if resp.StatusCode != 200 {
		t.Fatalf("download status %d, want 200 (body %s)", resp.StatusCode, downloaded)
	}
	if !bytes.Equal(downloaded, content) {
		t.Fatalf("downloaded %d bytes that differ from the %d uploaded bytes", len(downloaded), len(content))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type %q, want %q", got, "application/octet-stream")
	}
	if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(content)); got != want {
		t.Fatalf("Content-Length %q, want %q", got, want)
	}
	if got := resp.Header.Get("ETag"); got == "" {
		t.Fatal("the download carries no ETag header")
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges %q, want %q", got, "bytes")
	}
}

func TestDownloadByteRanges(t *testing.T) {
	content := patternBytes(1024)
	size := len(content)

	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
	file := seedFile(t, metadata, store, "ranges.bin", "application/octet-stream", content)

	cases := []struct {
		name        string
		rangeHeader string
		start       int
		end         int
	}{
		{name: "start to end", rangeHeader: "bytes=0-3", start: 0, end: 3},
		{name: "middle slice", rangeHeader: "bytes=512-767", start: 512, end: 767},
		{name: "open ended", rangeHeader: "bytes=1000-", start: 1000, end: size - 1},
		{name: "suffix", rangeHeader: "bytes=-16", start: size - 16, end: size - 1},
		{name: "suffix longer than the object", rangeHeader: "bytes=-2048", start: 0, end: size - 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := content[tc.start : tc.end+1]

			resp, body := fetchContent(t, app, "GET", contentPath(file.ID), tc.rangeHeader)
			if resp.StatusCode != 206 {
				t.Fatalf("status %d, want 206 (body %s)", resp.StatusCode, body)
			}
			if got, wantRange := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end, size); got != wantRange {
				t.Fatalf("Content-Range %q, want %q", got, wantRange)
			}
			if got, wantLength := resp.Header.Get("Content-Length"), strconv.Itoa(len(want)); got != wantLength {
				t.Fatalf("Content-Length %q, want %q", got, wantLength)
			}
			if !bytes.Equal(body, want) {
				t.Fatalf("body of %d bytes differs from the %d bytes range %q asks for", len(body), len(want), tc.rangeHeader)
			}
		})
	}
}

func TestDownloadInvalidRangeAnswers416(t *testing.T) {
	content := patternBytes(1024)
	size := len(content)

	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
	file := seedFile(t, metadata, store, "invalid-range.bin", "application/octet-stream", content)

	cases := []struct {
		name        string
		rangeHeader string
	}{
		{name: "start past the end", rangeHeader: "bytes=1024-"},
		{name: "start past the end with an end", rangeHeader: "bytes=1024-1100"},
		{name: "reversed", rangeHeader: "bytes=900-100"},
		{name: "zero suffix", rangeHeader: "bytes=-0"},
		{name: "non numeric", rangeHeader: "bytes=abc-def"},
		{name: "multi range", rangeHeader: "bytes=0-1,3-4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := fetchContent(t, app, "GET", contentPath(file.ID), tc.rangeHeader)
			if resp.StatusCode != 416 {
				t.Fatalf("status %d, want 416 (body %s)", resp.StatusCode, body)
			}
			assertJSONError(t, body, "invalid range")
			if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes */%d", size); got != want {
				t.Fatalf("Content-Range %q, want %q", got, want)
			}
		})
	}
}

func TestHeadReturnsMetadataAndNoBody(t *testing.T) {
	content := patternBytes(4096)

	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
	file := seedFile(t, metadata, store, "head.bin", "application/octet-stream", content)

	getResp, getBody := fetchContent(t, app, "GET", contentPath(file.ID), "")
	if getResp.StatusCode != 200 {
		t.Fatalf("GET status %d, want 200 (body %s)", getResp.StatusCode, getBody)
	}
	if !bytes.Equal(getBody, content) {
		t.Fatalf("GET body of %d bytes differs from the %d stored bytes", len(getBody), len(content))
	}

	headResp, headBody := fetchContent(t, app, "HEAD", contentPath(file.ID), "")
	if headResp.StatusCode != 200 {
		t.Fatalf("HEAD status %d, want 200 (body %s)", headResp.StatusCode, headBody)
	}
	if len(headBody) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes, want none", len(headBody))
	}

	for _, header := range []string{"Content-Type", "Content-Length", "ETag", "Accept-Ranges", "X-Content-SHA256"} {
		if got, want := headResp.Header.Get(header), getResp.Header.Get(header); got != want {
			t.Fatalf("%s on HEAD is %q, want the GET value %q", header, got, want)
		}
	}
	if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(content)); got != want {
		t.Fatalf("Content-Length on HEAD is %q, want %q", got, want)
	}

	if gets := store.recordedGets(); len(gets) != 1 {
		t.Fatalf("storage recorded %d object reads, want only the GET's: HEAD must not open the object", len(gets))
	}
}

func TestContentAdvertisesSHA256(t *testing.T) {
	content := patternBytes(4096)

	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
	file := seedFile(t, metadata, store, "digest.bin", "application/octet-stream", content)

	digest, _, err := sha256Hex(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the content: %v", err)
	}
	if file.SHA256 != digest {
		t.Fatalf("the seeded metadata carries sha256 %q, want %q", file.SHA256, digest)
	}

	resp, body := fetchContent(t, app, "GET", contentPath(file.ID), "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET status %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Content-SHA256"); got != digest {
		t.Fatalf("GET X-Content-SHA256 %q, want the stored digest %q", got, digest)
	}

	headResp, headBody := fetchContent(t, app, "HEAD", contentPath(file.ID), "")
	if headResp.StatusCode != 200 {
		t.Fatalf("HEAD status %d, want 200 (body %s)", headResp.StatusCode, headBody)
	}
	if got := headResp.Header.Get("X-Content-SHA256"); got != digest {
		t.Fatalf("HEAD X-Content-SHA256 %q, want the stored digest %q", got, digest)
	}

	rangeResp, rangeBody := fetchContent(t, app, "GET", contentPath(file.ID), "bytes=0-511")
	if rangeResp.StatusCode != 206 {
		t.Fatalf("range status %d, want 206 (body %s)", rangeResp.StatusCode, rangeBody)
	}
	if got := rangeResp.Header.Get("X-Content-SHA256"); got != digest {
		t.Fatalf("range X-Content-SHA256 %q, want the stored digest %q", got, digest)
	}
	if !bytes.Equal(rangeBody, content[:512]) {
		t.Fatalf("range body of %d bytes differs from the %d bytes it asks for", len(rangeBody), 512)
	}
}

func TestContentUnknownFileAnswers404(t *testing.T) {
	app := New(Deps{Files: files.NewMemory(), Storage: newFakeStorage(), PresignTTL: testPresignTTL})

	unknown := contentPath(files.NewID())

	resp, body := fetchContent(t, app, "GET", unknown, "")
	if resp.StatusCode != 404 {
		t.Fatalf("GET status %d, want 404 (body %s)", resp.StatusCode, body)
	}
	assertJSONError(t, body, "file not found")

	headResp, headBody := fetchContent(t, app, "HEAD", contentPath(files.NewID()), "")
	if headResp.StatusCode != 404 {
		t.Fatalf("HEAD status %d, want 404 (body %s)", headResp.StatusCode, headBody)
	}
	if len(headBody) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes, want none", len(headBody))
	}
}

func TestContentWithoutStorageAnswers503(t *testing.T) {
	metadata := files.NewMemory()
	file := files.File{
		ID:          files.NewID(),
		Key:         objectKeyPrefix + files.NewID(),
		Name:        "unreachable.bin",
		Size:        4,
		ContentType: "application/octet-stream",
		CreatedAt:   time.Now().UTC(),
	}
	if err := metadata.Add(file); err != nil {
		t.Fatalf("seeding the metadata record: %v", err)
	}

	app := New(Deps{Files: metadata, PresignTTL: testPresignTTL})

	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			resp, body := fetchContent(t, app, method, contentPath(file.ID), "")
			if resp.StatusCode != 503 {
				t.Fatalf("%s status %d, want 503 (body %s)", method, resp.StatusCode, body)
			}
			if method == "GET" {
				assertJSONError(t, body, "storage unavailable")
			}
		})
	}
}

func TestContentStorageFailureAnswers502(t *testing.T) {
	content := patternBytes(64)

	t.Run("stat fails", func(t *testing.T) {
		store := newFakeStorage()
		metadata := files.NewMemory()
		app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
		file := seedFile(t, metadata, store, "stat.bin", "application/octet-stream", content)
		store.statErr = storage.ErrUnavailable

		resp, body := fetchContent(t, app, "GET", contentPath(file.ID), "")
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

		resp, body := fetchContent(t, app, "GET", contentPath(file.ID), "")
		if resp.StatusCode != 502 {
			t.Fatalf("status %d, want 502 (body %s)", resp.StatusCode, body)
		}
		assertJSONError(t, body, "storage unavailable")
	})
}

func TestLargeObjectStreamsWithoutBuffering(t *testing.T) {
	const (
		payloadSize = 32 << 20
		rangeStart  = 1 << 20
	)

	content := patternBytes(payloadSize)

	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})
	file := seedFile(t, metadata, store, "large.bin", "application/octet-stream", content)

	resp, body := fetchContent(t, app, "GET", contentPath(file.ID), fmt.Sprintf("bytes=%d-", rangeStart), 60000)
	if resp.StatusCode != 206 {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}

	want := content[rangeStart:]
	if len(body) != len(want) {
		t.Fatalf("downloaded %d bytes, want %d", len(body), len(want))
	}
	if !bytes.Equal(body, want) {
		t.Fatal("the downloaded bytes differ from the requested range")
	}

	gets := store.recordedGets()
	if len(gets) != 1 {
		t.Fatalf("storage recorded %d object reads, want 1", len(gets))
	}
	if gets[0].offset != rangeStart {
		t.Fatalf("storage read from offset %d, want %d", gets[0].offset, rangeStart)
	}
	if wantLength := int64(len(want)); gets[0].length != wantLength {
		t.Fatalf("storage read %d bytes, want %d: the whole object must not be fetched for a range", gets[0].length, wantLength)
	}

	reader := store.lastBody()
	if reader == nil {
		t.Fatal("storage handed out no object body")
	}
	if reader.reads < 2 {
		t.Fatalf("the object body was read in %d calls, want the response to copy it out in chunks", reader.reads)
	}
	if !reader.closed {
		t.Fatal("the object body was not closed once the response finished")
	}
}
