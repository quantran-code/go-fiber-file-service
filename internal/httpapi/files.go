package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// Limits, key layout and defaults of the file routes. The upload cap is fixed
// by the API contract at 100 MiB, so it is a constant rather than configuration.
const (
	maxUploadBytes int64 = 100 << 20

	// maxMultipartOverhead is the slack the Fiber body limit keeps above the
	// file cap so the multipart framing around the part still fits; the upload
	// handler enforces the cap on the part itself.
	maxMultipartOverhead = 1 << 20

	// objectKeyPrefix keeps uploaded objects in one key space. Keys are
	// internal and never part of a response.
	objectKeyPrefix = "files/"

	// defaultContentType is stored when a part carries no usable content type.
	defaultContentType = "application/octet-stream"
)

var (
	// errTooLarge reports that a file part grew past maxUploadBytes.
	errTooLarge = errors.New("file too large")

	// errNotMultipart reports a request body that is not multipart/form-data.
	errNotMultipart = errors.New("not a multipart request")
)

// limitedPartReader counts the bytes read from a multipart part and stops as
// soon as they pass limit, so an oversized part is never held whole.
type limitedPartReader struct {
	r        io.Reader
	limit    int64
	n        int64
	exceeded bool
}

// Read returns errTooLarge once the bytes it would hand back push the count past
// the limit, so the copy that drives it aborts immediately.
func (l *limitedPartReader) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, errTooLarge
	}

	// Ask for at most one byte past the limit, which is enough to notice that
	// the part is too long without reading a whole extra buffer.
	if remaining := l.limit - l.n + 1; int64(len(p)) > remaining {
		p = p[:remaining]
	}

	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.limit {
		l.exceeded = true
		return n, errTooLarge
	}
	return n, err
}

// sanitizeFileName strips any directory component from an uploaded name so a
// client cannot smuggle a path into the metadata, and reports an empty name when
// nothing usable is left.
func sanitizeFileName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return ""
	}

	// Clients may send either separator, so cut on both.
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}

	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

// partContentType returns the content type stored for a part, falling back to
// application/octet-stream when the part carries none that parses.
func partContentType(part *multipart.Part) string {
	value := strings.TrimSpace(part.Header.Get("Content-Type"))
	if value == "" {
		return defaultContentType
	}
	if _, _, err := mime.ParseMediaType(value); err != nil {
		return defaultContentType
	}
	return value
}

// multipartReader returns a reader over the request's multipart body. The body
// is consumed straight from the request stream: c.FormFile and c.MultipartForm
// are deliberately not used, because both buffer the whole part first.
func multipartReader(c *fiber.Ctx) (*multipart.Reader, error) {
	mediaType, params, err := mime.ParseMediaType(string(c.Request().Header.ContentType()))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, errNotMultipart
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, errNotMultipart
	}

	body := c.Context().Request.BodyStream()
	if body == nil {
		// The server buffered the request instead of streaming it; parse what
		// it holds so the route still works.
		body = bytes.NewReader(c.Body())
	}
	return multipart.NewReader(body, boundary), nil
}

// drainBody discards whatever is left of the request body after an early error
// response, so the connection is left in a readable state. The body is bounded
// by the server's body limit, so this cannot read without end.
func drainBody(c *fiber.Ctx) {
	if body := c.Context().Request.BodyStream(); body != nil {
		_, _ = io.Copy(io.Discard, body)
	}
}

// writeStorageError maps an object-store failure onto the documented response.
func writeStorageError(c *fiber.Ctx, err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return writeError(c, fiber.StatusNotFound, "file not found")
	}
	return writeError(c, fiber.StatusBadGateway, "storage unavailable")
}

// handleUpload streams the single "file" part of a multipart request into object
// storage and records its metadata.
func handleUpload(deps Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Storage == nil {
			drainBody(c)
			return writeError(c, fiber.StatusServiceUnavailable, "storage unavailable")
		}

		reader, err := multipartReader(c)
		if err != nil {
			drainBody(c)
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		// The key is known before the bytes arrive, so the object can be removed
		// again when the request turns out to be invalid.
		id := files.NewID()
		key := objectKeyPrefix + id

		var (
			uploaded    bool
			fileName    string
			contentType string
			size        int64
		)

		discard := func() {
			if uploaded {
				_ = deps.Storage.Delete(c.Context(), key)
			}
		}

		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				discard()
				drainBody(c)
				return writeError(c, fiber.StatusBadRequest, "invalid request")
			}

			if part.FormName() != "file" {
				_ = part.Close()
				continue
			}

			if uploaded {
				// The contract allows exactly one file part.
				_ = part.Close()
				discard()
				drainBody(c)
				return writeError(c, fiber.StatusBadRequest, "invalid request")
			}

			uploaded = true
			fileName = sanitizeFileName(part.FileName())
			contentType = partContentType(part)

			// The part is copied straight from the request into storage. Its
			// length is not known up front, so it is handed over as -1 and
			// counted on the way through.
			limited := &limitedPartReader{r: part, limit: maxUploadBytes}
			putErr := deps.Storage.Put(c.Context(), key, limited, -1, contentType)
			_ = part.Close()

			if limited.exceeded {
				discard()
				drainBody(c)
				return writeError(c, fiber.StatusRequestEntityTooLarge, "file too large")
			}
			if putErr != nil {
				discard()
				drainBody(c)
				return writeError(c, fiber.StatusBadGateway, "storage unavailable")
			}
			size = limited.n
		}

		drainBody(c)

		if !uploaded {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		file := files.File{
			ID:          id,
			Key:         key,
			Name:        fileName,
			Size:        size,
			ContentType: contentType,
			CreatedAt:   time.Now().UTC(),
		}
		if err := deps.Files.Add(file); err != nil {
			// The object is already in storage, so remove it again; the
			// metadata store is the only place the API can reach it from.
			discard()
			return writeError(c, fiber.StatusInternalServerError, "internal error")
		}

		return c.Status(fiber.StatusCreated).JSON(file)
	}
}

// byteRange is one resolved byte range of an object. A zero Length means the
// range runs to the end of the object, which the storage seam reads as
// read-to-end.
type byteRange struct {
	Start  int64
	Length int64
}

// errInvalidRange reports a Range header this route cannot satisfy: only single
// byte ranges of a non-empty object are supported.
var errInvalidRange = errors.New("invalid range")

// parseRange resolves the single byte range in header against an object of the
// given size. A malformed header, a multi-range request, a zero-length suffix
// and a range that starts at or past the end are all rejected; an end past the
// last byte and a suffix longer than the object are clamped.
func parseRange(header string, size int64) (byteRange, error) {
	const prefix = "bytes="

	spec := strings.TrimSpace(header)
	if len(spec) < len(prefix) || !strings.EqualFold(spec[:len(prefix)], prefix) {
		return byteRange{}, errInvalidRange
	}

	spec = strings.TrimSpace(spec[len(prefix):])
	if spec == "" || strings.Contains(spec, ",") {
		return byteRange{}, errInvalidRange
	}

	startRaw, endRaw, ok := strings.Cut(spec, "-")
	if !ok {
		return byteRange{}, errInvalidRange
	}
	startRaw = strings.TrimSpace(startRaw)
	endRaw = strings.TrimSpace(endRaw)

	if startRaw == "" {
		// A suffix range asks for the last n bytes of the object.
		suffix, err := strconv.ParseInt(endRaw, 10, 64)
		if err != nil || suffix <= 0 || size <= 0 {
			return byteRange{}, errInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return byteRange{Start: size - suffix, Length: suffix}, nil
	}

	start, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil || start < 0 || start >= size {
		return byteRange{}, errInvalidRange
	}
	if endRaw == "" {
		return byteRange{Start: start, Length: size - start}, nil
	}

	end, err := strconv.ParseInt(endRaw, 10, 64)
	if err != nil || end < start {
		return byteRange{}, errInvalidRange
	}
	if end >= size {
		end = size - 1
	}
	return byteRange{Start: start, Length: end - start + 1}, nil
}

// bodyStream hands the object reader to the response writer for exactly its
// remaining bytes and closes the reader once they have been transferred. The
// reader therefore stays open while the response is written, and is released
// when the transfer finishes instead of leaking for the life of the process.
type bodyStream struct {
	body      io.ReadCloser
	remaining int64
}

func (b *bodyStream) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		_ = b.body.Close()
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}

	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	if err != nil || b.remaining == 0 {
		_ = b.body.Close()
	}
	return n, err
}

// handleContent streams a stored object back to the client and answers a single
// byte range with 206. With headOnly the same headers are computed and the
// object reader is never opened.
func handleContent(deps Deps, headOnly bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if headOnly {
			// A HEAD answer carries the headers of the equivalent GET and no
			// body. Skipping the body explicitly keeps the declared
			// Content-Length from being replaced by the empty body's length.
			c.Response().SkipBody = true
		}

		file, ok := deps.Files.Get(c.Params("id"))
		if !ok {
			return writeError(c, fiber.StatusNotFound, "file not found")
		}
		if deps.Storage == nil {
			return writeError(c, fiber.StatusServiceUnavailable, "storage unavailable")
		}

		// The stored object is authoritative for the size and the ETag, so the
		// metadata record is only used to find the key.
		info, err := deps.Storage.Stat(c.Context(), file.Key)
		if err != nil {
			return writeStorageError(c, err)
		}

		status := fiber.StatusOK
		var requested byteRange
		if header := strings.TrimSpace(c.Get(fiber.HeaderRange)); header != "" {
			requested, err = parseRange(header, info.Size)
			if err != nil {
				c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes */%d", info.Size))
				return writeError(c, fiber.StatusRequestedRangeNotSatisfiable, "invalid range")
			}
			status = fiber.StatusPartialContent
		}

		contentLength := info.Size
		if status == fiber.StatusPartialContent {
			contentLength = requested.Length
		}

		// A HEAD answer carries the metadata of the GET without touching the
		// object; a GET hands the reader to the response, which copies it out
		// as it is written instead of holding the object in memory.
		var body io.ReadCloser
		if !headOnly {
			object, getErr := deps.Storage.Get(c.Context(), file.Key, requested.Start, requested.Length)
			if getErr != nil {
				return writeStorageError(c, getErr)
			}
			body = object.Body
		}

		contentType := strings.TrimSpace(info.ContentType)
		if contentType == "" {
			contentType = defaultContentType
		}
		c.Set(fiber.HeaderContentType, contentType)
		if etag := strings.TrimSpace(info.ETag); etag != "" {
			c.Set(fiber.HeaderETag, etag)
		}
		c.Set(fiber.HeaderAcceptRanges, "bytes")
		c.Set(fiber.HeaderContentLength, strconv.FormatInt(contentLength, 10))
		if status == fiber.StatusPartialContent {
			c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes %d-%d/%d", requested.Start, requested.Start+requested.Length-1, info.Size))
		}
		c.Status(status)

		if headOnly {
			return nil
		}
		return c.SendStream(&bodyStream{body: body, remaining: contentLength}, int(contentLength))
	}
}

// registerFileRoutes mounts the file routes on the application.
func registerFileRoutes(app *fiber.App, deps Deps) {
	app.Post("/files", handleUpload(deps))
	// app.Get also registers a HEAD route carrying the same handler, which
	// would win over the explicit HEAD route below and make a HEAD request
	// open the object. Adding the GET method directly keeps the two apart.
	app.Add(fiber.MethodGet, "/files/:id/content", handleContent(deps, false))
	app.Head("/files/:id/content", handleContent(deps, true))
}
