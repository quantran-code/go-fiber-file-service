# go-fiber-file-service

A small Go Fiber service for storing and serving files through an S3-compatible
object store. This repository is the scaffold for four follow-up tickets:

1. file streaming upload/download,
2. file hash and integrity,
3. presigned URL upload,
4. file metadata and listing.

The scaffold already contains the pieces every ticket builds on:

- Fiber application and `GET /health`;
- S3-compatible storage behind `internal/storage.Store` (minio-go, path-style);
- in-memory file metadata behind `internal/files.Store`;
- configuration from the environment.

## Run

```sh
go run ./cmd/server
```

The service listens on `:8080` by default. `GET /health` answers
`{"status":"ok"}` even when object storage is not configured; the feature
tickets add the file routes and answer 503 when storage is unavailable.

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `PORT` | HTTP port | `8080` |
| `S3_ENDPOINT` | S3-compatible endpoint, e.g. `http://localhost:9000` | — |
| `S3_REGION` | signing region | `us-east-1` |
| `S3_BUCKET` | private bucket | — |
| `S3_ACCESS_KEY` | access key | — |
| `S3_SECRET_KEY` | secret key | — |
| `S3_FORCE_PATH_STYLE` | path-style addressing | `true` |
| `PRESIGN_TTL_SECONDS` | presigned URL lifetime | `900` |

A local RustFS instance is available at `http://localhost:9000` with bucket
`octiq-uploads` for manual testing:

```sh
export S3_ENDPOINT=http://localhost:9000
export S3_REGION=us-east-1
export S3_BUCKET=octiq-uploads
export S3_ACCESS_KEY=rustfsadmin
export S3_SECRET_KEY=rustfsadmin
export S3_FORCE_PATH_STYLE=true
go run ./cmd/server
```

## Test

```sh
go test ./...
```

The scaffold tests need no broker, bucket or network connection.
