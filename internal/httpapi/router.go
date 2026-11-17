// Package httpapi wires the Fiber application.
package httpapi

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// Deps are the dependencies the file routes use.
type Deps struct {
	Files      files.Store
	Storage    storage.Store
	PresignTTL time.Duration
}

// New builds the Fiber application.
func New(deps Deps) *fiber.App {
	// Uploads are copied straight from the request body into object storage, so
	// the server hands the handler a body stream instead of parsing the
	// multipart form (and buffering its parts) first. The body limit sits just
	// above the file cap to leave room for the multipart framing; the upload
	// handler enforces the cap on the part itself.
	app := fiber.New(fiber.Config{
		AppName:                      "go-fiber-file-service",
		DisableStartupMessage:        true,
		StreamRequestBody:            true,
		DisablePreParseMultipartForm: true,
		BodyLimit:                    int(maxUploadBytes) + maxMultipartOverhead,
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"service": "file-service", "status": "ok"})
	})

	registerFileRoutes(app, deps)

	return app
}

// writeError is the shared error body every later file route uses.
func writeError(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(fiber.Map{"error": message})
}
