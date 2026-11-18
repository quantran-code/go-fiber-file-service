package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
)

// wantItemFields is the field set one metadata record carries: a listing entry
// and a GET /files/{id} body both encode exactly these keys.
var wantItemFields = []string{"id", "name", "size", "contentType", "sha256", "createdAt"}

// assertItemFields checks that record carries exactly the metadata fields and no
// internal one.
func assertItemFields(t *testing.T, record map[string]any) {
	t.Helper()

	if len(record) != len(wantItemFields) {
		t.Fatalf("metadata record %v carries %d fields, want %d", record, len(record), len(wantItemFields))
	}
	for _, field := range wantItemFields {
		if _, ok := record[field]; !ok {
			t.Fatalf("metadata record %v carries no %q field", record, field)
		}
	}
}

// listingQuery builds the listing path for a query string.
func listingQuery(query string) string {
	if query == "" {
		return "/files"
	}
	return "/files?" + query
}

// rawListing requests the listing route with query and returns the status
// together with the undecoded body, so a test can tell an empty items array
// apart from a null one.
func rawListing(t *testing.T, app *fiber.App, query string) (int, []byte) {
	t.Helper()

	resp, body := fetchContent(t, app, "GET", listingQuery(query), "")
	return resp.StatusCode, body
}

// seedListingFile records one metadata entry created at createdAt, so a listing
// order is deterministic instead of depending on the clock.
func seedListingFile(t *testing.T, metadata *files.Memory, name string, createdAt time.Time) files.File {
	t.Helper()

	id := files.NewID()
	file := files.File{
		ID:          id,
		Key:         objectKeyPrefix + id,
		Name:        name,
		Size:        4,
		ContentType: "application/octet-stream",
		SHA256:      strings.Repeat("a", 64),
		CreatedAt:   createdAt,
	}
	if err := metadata.Add(file); err != nil {
		t.Fatalf("seeding the metadata of %q: %v", name, err)
	}
	return file
}

func TestListFilesNewestFirstAcrossPages(t *testing.T) {
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata})

	base := time.Now().UTC().Add(-time.Hour)
	seeded := make([]files.File, 0, 5)
	for i := 0; i < 5; i++ {
		seeded = append(seeded, seedListingFile(t, metadata, fmt.Sprintf("file-%d.bin", i), base.Add(time.Duration(i)*time.Second)))
	}

	want := make([]string, 0, len(seeded))
	for i := len(seeded) - 1; i >= 0; i-- {
		want = append(want, seeded[i].ID)
	}

	defaults := listEnvelope(t, app, "")
	if page, _ := defaults["page"].(float64); int(page) != 1 {
		t.Fatalf("a listing without a query echoed page %v, want the default 1", defaults["page"])
	}
	if size, _ := defaults["page_size"].(float64); int(size) != 20 {
		t.Fatalf("a listing without a query echoed page_size %v, want the default 20", defaults["page_size"])
	}
	if total, _ := defaults["total"].(float64); int(total) != len(seeded) {
		t.Fatalf("a listing without a query carries total %v, want %d", defaults["total"], len(seeded))
	}

	got := make([]string, 0, len(want))
	for page := 1; page <= 3; page++ {
		envelope := listEnvelope(t, app, fmt.Sprintf("page=%d&page_size=2", page))
		if total, _ := envelope["total"].(float64); int(total) != len(seeded) {
			t.Fatalf("page %d carries total %v, want the unchanged %d", page, envelope["total"], len(seeded))
		}
		if echoed, _ := envelope["page"].(float64); int(echoed) != page {
			t.Fatalf("page %d echoed page %v", page, envelope["page"])
		}
		if size, _ := envelope["page_size"].(float64); int(size) != 2 {
			t.Fatalf("page %d echoed page_size %v, want 2", page, envelope["page_size"])
		}

		items, ok := envelope["items"].([]any)
		if !ok {
			t.Fatalf("page %d items %v is not a JSON array", page, envelope["items"])
		}
		for _, raw := range items {
			record, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("page %d item %v is not a JSON object", page, raw)
			}
			assertItemFields(t, record)
			id, _ := record["id"].(string)
			got = append(got, id)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("the pages carried %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d is %q, want %q: the listing is not newest first (%v)", i, got[i], want[i], got)
		}
	}
}

func TestListFilesNameFilter(t *testing.T) {
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata})

	base := time.Now().UTC().Add(-time.Hour)
	lower := seedListingFile(t, metadata, "Shot.png", base)
	seedListingFile(t, metadata, "vacation.jpg", base.Add(time.Second))
	upper := seedListingFile(t, metadata, "MY-SHOT-LIST.TXT", base.Add(2*time.Second))

	envelope := listEnvelope(t, app, "name=SHOT")
	if total, _ := envelope["total"].(float64); int(total) != 2 {
		t.Fatalf("the filtered listing carries total %v, want the 2 matching names", envelope["total"])
	}

	items, ok := envelope["items"].([]any)
	if !ok {
		t.Fatalf("the filtered listing items %v is not a JSON array", envelope["items"])
	}
	if len(items) != 2 {
		t.Fatalf("the filtered listing carries %d items, want 2", len(items))
	}
	for i, wantID := range []string{upper.ID, lower.ID} {
		record, ok := items[i].(map[string]any)
		if !ok {
			t.Fatalf("filtered item %d is %v, not a JSON object", i, items[i])
		}
		if got := record["id"]; got != wantID {
			t.Fatalf("filtered item %d is %v, want %q: the filter keeps the newest-first order", i, got, wantID)
		}
	}

	status, body := rawListing(t, app, "name=nothing-matches")
	if status != 200 {
		t.Fatalf("a non-matching filter answered %d, want 200 (body %s)", status, body)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding the empty listing %q: %v", body, err)
	}
	if got := strings.TrimSpace(string(payload["items"])); got != "[]" {
		t.Fatalf("an empty result carries items %s, want []", got)
	}
	if got := strings.TrimSpace(string(payload["total"])); got != "0" {
		t.Fatalf("an empty result carries total %s, want 0", got)
	}
}

func TestListFilesRejectsInvalidPaging(t *testing.T) {
	metadata := files.NewMemory()
	seedListingFile(t, metadata, "shot.png", time.Now().UTC())
	app := New(Deps{Files: metadata})

	for _, query := range []string{"page_size=0", "page_size=101", "page_size=abc", "page=0", "page=abc"} {
		t.Run(query, func(t *testing.T) {
			resp, body := fetchContent(t, app, "GET", listingQuery(query), "")
			if resp.StatusCode != 400 {
				t.Fatalf("GET %s status %d, want 400 (body %s)", listingQuery(query), resp.StatusCode, body)
			}

			payload := decodeJSONObject(t, body)
			if len(payload) != 1 {
				t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
			}
			if got, _ := payload["error"].(string); got != "invalid paging" {
				t.Fatalf("error message %q, want %q", got, "invalid paging")
			}
		})
	}
}

func TestListFilesPagePastTheEnd(t *testing.T) {
	metadata := files.NewMemory()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		seedListingFile(t, metadata, fmt.Sprintf("past-%d.bin", i), base.Add(time.Duration(i)*time.Second))
	}
	app := New(Deps{Files: metadata})

	status, body := rawListing(t, app, "page=5&page_size=20")
	if status != 200 {
		t.Fatalf("a page past the last answered %d, want 200 (body %s)", status, body)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding the empty page %q: %v", body, err)
	}
	if got := strings.TrimSpace(string(payload["items"])); got != "[]" {
		t.Fatalf("a page past the last carries items %s, want []", got)
	}
	if got := strings.TrimSpace(string(payload["total"])); got != "3" {
		t.Fatalf("a page past the last carries total %s, want the unchanged 3", got)
	}
	if got := strings.TrimSpace(string(payload["page"])); got != "5" {
		t.Fatalf("a page past the last echoed page %s, want 5", got)
	}
}

func TestFileMetadataReadAndUnknownID(t *testing.T) {
	store := newFakeStorage()
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata, Storage: store, PresignTTL: testPresignTTL})

	content := patternBytes(300)
	id, digest := uploadFile(t, app, "read-me.bin", content)

	resp, body := fetchContent(t, app, "GET", "/files/"+id, "")
	if resp.StatusCode != 200 {
		t.Fatalf("metadata status %d, want 200 (body %s)", resp.StatusCode, body)
	}

	record := decodeJSONObject(t, body)
	assertItemFields(t, record)
	if got := record["id"]; got != id {
		t.Fatalf("metadata id %v, want %q", got, id)
	}
	if got := record["name"]; got != "read-me.bin" {
		t.Fatalf("metadata name %v, want %q", got, "read-me.bin")
	}
	if got, ok := record["size"].(float64); !ok || int64(got) != int64(len(content)) {
		t.Fatalf("metadata size %v, want %d", record["size"], len(content))
	}
	if got := record["contentType"]; got != "application/octet-stream" {
		t.Fatalf("metadata contentType %v, want %q", got, "application/octet-stream")
	}
	if got := record["sha256"]; got != digest {
		t.Fatalf("metadata sha256 %v, want the upload digest %q", got, digest)
	}
	if got, _ := record["createdAt"].(string); got == "" {
		t.Fatalf("metadata body %v carries no createdAt", record)
	}

	unknownResp, unknownBody := fetchContent(t, app, "GET", "/files/"+files.NewID(), "")
	if unknownResp.StatusCode != 404 {
		t.Fatalf("an unknown id answered %d, want 404 (body %s)", unknownResp.StatusCode, unknownBody)
	}

	payload := decodeJSONObject(t, unknownBody)
	if len(payload) != 1 {
		t.Fatalf("error body %v carries %d fields, want 1", payload, len(payload))
	}
	if got, _ := payload["error"].(string); got != "file not found" {
		t.Fatalf("error message %q, want %q", got, "file not found")
	}
}

func TestMetadataResponsesNeverCarryTheObjectKey(t *testing.T) {
	metadata := files.NewMemory()
	app := New(Deps{Files: metadata})

	file := seedListingFile(t, metadata, "internal-key.bin", time.Now().UTC())

	_, listingBody := rawListing(t, app, "")
	if strings.Contains(string(listingBody), file.Key) {
		t.Fatalf("the listing body %q carries the internal object key %q", listingBody, file.Key)
	}
	records := listFiles(t, app)
	if len(records) != 1 {
		t.Fatalf("listing carries %d records, want 1", len(records))
	}
	assertItemFields(t, records[0])

	metadataResp, metadataBody := fetchContent(t, app, "GET", "/files/"+file.ID, "")
	if metadataResp.StatusCode != 200 {
		t.Fatalf("metadata status %d, want 200 (body %s)", metadataResp.StatusCode, metadataBody)
	}
	if strings.Contains(string(metadataBody), file.Key) {
		t.Fatalf("the metadata body %q carries the internal object key %q", metadataBody, file.Key)
	}

	payload := decodeJSONObject(t, metadataBody)
	assertItemFields(t, payload)
	if _, ok := payload["key"]; ok {
		t.Fatalf("the metadata body %v exposes the internal key field", payload)
	}
}
