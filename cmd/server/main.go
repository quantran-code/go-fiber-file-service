// Command server runs the file service.
package main

import (
	"log"

	"github.com/quantran-code/go-fiber-file-service/internal/config"
	"github.com/quantran-code/go-fiber-file-service/internal/files"
	"github.com/quantran-code/go-fiber-file-service/internal/httpapi"
	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

func main() {
	cfg := config.Load()

	var objectStore storage.Store
	if cfg.S3.Configured() {
		store, err := storage.NewMinIO(cfg.S3)
		if err != nil {
			log.Printf("object storage disabled: %v", err)
		} else {
			objectStore = store
		}
	} else {
		log.Printf("object storage disabled: S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}

	app := httpapi.New(httpapi.Deps{
		Files:      files.NewMemory(),
		Storage:    objectStore,
		PresignTTL: cfg.S3.PresignTTL,
	})

	log.Printf("file service listening on :%s", cfg.Port)
	log.Fatal(app.Listen(":" + cfg.Port))
}
