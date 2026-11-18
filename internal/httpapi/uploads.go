package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/config"
	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// Key layout and bounds of the direct-upload routes. A presigned key is always
// built from this prefix, a server-generated id and a sanitized name, never from
// caller input.
const (
	// uploadsKeyPrefix is the only prefix a presigned key is built from.
	uploadsKeyPrefix = "uploads/"

	// maxUploadKeyLength bounds the key the completion route accepts.
	maxUploadKeyLength = 512

	// maxUploadNameLength bounds a file name in characters, not bytes.
	maxUploadNameLength = 255
)

// presignRequest is the body of POST /uploads/presign. It deliberately carries
// no key and no URL, so a caller can never point a signature at an existing
// object.
type presignRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// presignResponse is the body of a successful presign. It carries the signed URL
// and the headers the client has to send; no credential, bucket or endpoint is
// copied into it.
type presignResponse struct {
	UploadURL       string            `json:"uploadUrl"`
	Key             string            `json:"key"`
	ExpiresAt       time.Time         `json:"expiresAt"`
	RequiredHeaders map[string]string `json:"requiredHeaders"`
}

// presignTTL returns the lifetime of a signed PUT. A Deps without a TTL falls
// back to the configured default rather than signing a URL that expires at once.
func presignTTL(deps Deps) time.Duration {
	if deps.PresignTTL > 0 {
		return deps.PresignTTL
	}
	return config.DefaultPresignTTL
}

// newPresignedKey builds the server-owned key of an upload and returns the
// safe-name segment it ends with.
func newPresignedKey(name string) (key string, safeName string) {
	safeName = sanitizeFileName(name)
	return uploadsKeyPrefix + files.NewID() + "/" + safeName, safeName
}

// handlePresignUpload mints a server-owned key, signs a PUT for it and answers
// the URL together with the headers the client has to send.
func handlePresignUpload(deps Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var request presignRequest
		if err := c.BodyParser(&request); err != nil {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		name := strings.TrimSpace(request.Name)
		if name == "" || utf8.RuneCountInString(name) > maxUploadNameLength {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		contentType := strings.TrimSpace(request.ContentType)
		if contentType == "" {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		// The key always ends with the sanitized name, so a name that leaves
		// nothing usable behind is rejected here rather than producing a key
		// whose last segment is empty.
		if sanitizeFileName(name) == "" {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		if request.Size < 1 {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}
		if request.Size > maxUploadBytes {
			return writeError(c, fiber.StatusRequestEntityTooLarge, "file too large")
		}

		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		if deps.Storage == nil {
			return writeError(c, fiber.StatusServiceUnavailable, "storage unavailable")
		}

		key, _ := newPresignedKey(name)
		ttl := presignTTL(deps)
		uploadURL, err := deps.Storage.PresignPut(c.Context(), key, ttl, contentType)
		if err != nil {
			return writeError(c, fiber.StatusBadGateway, "storage unavailable")
		}

		return c.Status(fiber.StatusCreated).JSON(presignResponse{
			UploadURL: uploadURL,
			Key:       key,
			ExpiresAt: time.Now().UTC().Add(ttl),
			RequiredHeaders: map[string]string{
				"Content-Type":   contentType,
				"Content-Length": strconv.FormatInt(request.Size, 10),
			},
		})
	}
}

// completeRequest is the body of POST /uploads/complete. Key names the object
// the client uploaded itself; name, contentType and the optional digest describe
// what it believes it stored.
type completeRequest struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ExpectedSize int64  `json:"expectedSize"`
	SHA256       string `json:"sha256"`
}

// errInvalidUploadKey reports a key the completion route refuses to look up.
var errInvalidUploadKey = errors.New("invalid upload key")

// validateUploadKey rejects a caller-supplied key that is not a plain
// bucket-relative key: empty, longer than the limit, absolute, carrying a ".."
// segment or shaped like a URL.
func validateUploadKey(key string) error {
	if key == "" || len(key) > maxUploadKeyLength {
		return errInvalidUploadKey
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "://") {
		return errInvalidUploadKey
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == ".." {
			return errInvalidUploadKey
		}
	}
	return nil
}

// writeUploadStorageError maps an object-store failure of the upload routes onto
// the documented response. It differs from writeStorageError only in the message
// of the missing-object answer.
func writeUploadStorageError(c *fiber.Ctx, err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return writeError(c, fiber.StatusNotFound, "upload not found")
	}
	return writeError(c, fiber.StatusBadGateway, "storage unavailable")
}

// handleCompleteUpload verifies an object the client uploaded itself and only
// then records its metadata. The digest is always taken server-side from the
// stored bytes; a client-supplied digest is only compared, never stored.
func handleCompleteUpload(deps Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var request completeRequest
		if err := c.BodyParser(&request); err != nil {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		if err := validateUploadKey(request.Key); err != nil {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		// The completion body's name is the authoritative one, but it has to
		// agree with the safe-name the presigned key was built with.
		safeName := request.Key[strings.LastIndex(request.Key, "/")+1:]
		if request.Name == "" || request.Name != safeName {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		// A size outside the upload bounds is a bad body; a size that is merely
		// different from the stored object is a mismatch, checked further down.
		if request.ExpectedSize < 1 || request.ExpectedSize > maxUploadBytes {
			return writeError(c, fiber.StatusBadRequest, "invalid request")
		}

		expected := strings.TrimSpace(request.SHA256)
		if expected != "" {
			decoded, err := hex.DecodeString(expected)
			if err != nil || len(decoded) != sha256.Size {
				return writeError(c, fiber.StatusBadRequest, "invalid request")
			}
		}

		if deps.Storage == nil {
			return writeError(c, fiber.StatusServiceUnavailable, "storage unavailable")
		}

		info, err := deps.Storage.Stat(c.Context(), request.Key)
		if err != nil {
			return writeUploadStorageError(c, err)
		}

		// The stored bytes are hashed in one streaming pass, so the digest the
		// metadata ends up with is always the server's own.
		object, err := deps.Storage.Get(c.Context(), request.Key, 0, 0)
		if err != nil {
			return writeUploadStorageError(c, err)
		}
		digest, _, hashErr := sha256Hex(object.Body)
		_ = object.Body.Close()
		if hashErr != nil {
			return writeError(c, fiber.StatusBadGateway, "storage unavailable")
		}

		// Nothing is registered unless the stored object is exactly what the
		// client said it uploaded.
		if info.Size != request.ExpectedSize || (expected != "" && !strings.EqualFold(expected, digest)) {
			return writeError(c, fiber.StatusConflict, "upload mismatch")
		}

		contentType := strings.TrimSpace(info.ContentType)
		if contentType == "" {
			contentType = strings.TrimSpace(request.ContentType)
		}
		if contentType == "" {
			contentType = defaultContentType
		}

		file := files.File{
			ID:          files.NewID(),
			Key:         request.Key,
			Name:        request.Name,
			Size:        info.Size,
			ContentType: contentType,
			SHA256:      digest,
			CreatedAt:   time.Now().UTC(),
		}
		if err := deps.Files.Add(file); err != nil {
			return writeError(c, fiber.StatusInternalServerError, "internal error")
		}

		return c.Status(fiber.StatusCreated).JSON(file)
	}
}

// registerUploadRoutes mounts the direct-to-object-store upload routes.
func registerUploadRoutes(app *fiber.App, deps Deps) {
	app.Post("/uploads/presign", handlePresignUpload(deps))
	app.Post("/uploads/complete", handleCompleteUpload(deps))
}
