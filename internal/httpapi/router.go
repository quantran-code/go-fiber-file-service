// Package httpapi wires the Fiber application.
package httpapi

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// Deps are the dependencies the file routes will use. The scaffold only serves
// health; the feature tickets add routes on top of these seams.
type Deps struct {
	Files      files.Store
	Storage    storage.Store
	PresignTTL time.Duration
}

// New builds the Fiber application.
func New(deps Deps) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "go-fiber-file-service",
		DisableStartupMessage: true,
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"service": "file-service", "status": "ok"})
	})

	return app
}

// writeError is the shared error body every later file route uses.
func writeError(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(fiber.Map{"error": message})
}
