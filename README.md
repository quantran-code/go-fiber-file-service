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

## Direct uploads

A browser can upload straight to the object store instead of proxying the bytes
through the service: ask for a signed PUT, upload the bytes itself, then ask the
service to register the completed object.

### `POST /uploads/presign`

```json
{"name": "shot.png", "contentType": "image/png", "size": 1024}
```

`name` is required and 1..255 characters, `contentType` is required and `size`
is required in the range 1 byte .. 100 MiB. The service owns the key: it is
always `uploads/<uuid>/<safe-name>`, built from the request name, and a key, url
or bucket supplied in the body is ignored.

`201 Created`:

```json
{
  "uploadUrl": "http://localhost:9000/octiq-uploads/uploads/6f1c.../shot.png?X-Amz-Signature=...",
  "key": "uploads/6f1c.../shot.png",
  "expiresAt": "2024-01-01T00:15:00Z",
  "requiredHeaders": {"Content-Type": "image/png", "Content-Length": "1024"}
}
```

`requiredHeaders` maps each header name to the value the client has to send on
the PUT, so `Content-Type` and `Content-Length` are always present. `uploadUrl`
is the only place a signature appears: no access key, secret, session token,
bucket or endpoint is returned to the client. The URL stays valid for
`PRESIGN_TTL_SECONDS` (default `900`), the variable already listed in the
configuration table above — the direct-upload routes add no new variable.

The client then `PUT`s the bytes to `uploadUrl` with exactly those headers. The
service never sees that request and never proxies the file body.

### `POST /uploads/complete`

```json
{"key": "uploads/6f1c.../shot.png", "name": "shot.png", "contentType": "image/png", "expectedSize": 1024, "sha256": "9c56..."}
```

`key` has to be a bucket-relative key: it may not be empty, longer than 512
characters, start with `/`, carry a `..` segment or contain `://`. `name` has to
match the last segment of the key. `expectedSize` has to be 1..100 MiB.
`sha256` is optional; when it is sent it has to be 64 hexadecimal characters and
it has to match the digest the service computes by streaming the stored object.
The metadata record always carries the service's own digest, never the supplied
one.

`201 Created` answers with the same body as `POST /files`:

```json
{
  "id": "0b41...",
  "name": "shot.png",
  "size": 1024,
  "contentType": "image/png",
  "sha256": "9c56...",
  "createdAt": "2024-01-01T00:00:04Z"
}
```

`GET /files/{id}/content` then serves the uploaded bytes.

### Errors

| Status | Body | When |
|---|---|---|
| `400` | `{"error":"invalid request"}` | unparsable body, a missing or out-of-range field, an unusable key or a name that disagrees with the key |
| `404` | `{"error":"upload not found"}` | completing a key whose object does not exist |
| `409` | `{"error":"upload mismatch"}` | the stored size or digest differs from what the request declared |
| `413` | `{"error":"file too large"}` | presigning a file above 100 MiB |
| `502` | `{"error":"storage unavailable"}` | the object store failed |
| `503` | `{"error":"storage unavailable"}` | the service has no object store configured |

## Test

```sh
go test ./...
```

The scaffold tests need no broker, bucket or network connection.
