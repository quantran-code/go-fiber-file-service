package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"

	"github.com/quantran-code/go-fiber-file-service/internal/storage"
)

// fakeChunkSize is the most a fakeStorage body hands out per Read, so a test can
// see that a response was copied out in many chunks instead of in one buffer.
const fakeChunkSize = 8 << 10

// putCall records one call to fakeStorage.Put.
type putCall struct {
	key         string
	size        int64
	contentType string
	body        []byte
}

// getCall records one call to fakeStorage.Get.
type getCall struct {
	key    string
	offset int64
	length int64
}

// fakeStorage is an in-memory storage.Store for the HTTP tests: it needs no
// bucket and no network, and it records every call so a test can assert how the
// handlers used it.
type fakeStorage struct {
	mu           sync.Mutex
	objects      map[string][]byte
	contentTypes map[string]string
	putKeys      []string
	puts         []putCall
	getCalls     []getCall
	bodies       []*chunkReader
	deleted      []string
	putErr       error
	getErr       error
	statErr      error
	overrideKey  string
	override     []byte
}

var _ storage.Store = (*fakeStorage)(nil)

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
	}
}

// seed stores an object without going through an upload, for tests that
// exercise the content routes directly.
func (f *fakeStorage) seed(key string, content []byte, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), content...)
	f.contentTypes[key] = contentType
}

// object returns the bytes stored under key and whether the key exists.
func (f *fakeStorage) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.objects[key]
	return content, ok
}

// contentTypeOf returns the content type recorded for key.
func (f *fakeStorage) contentTypeOf(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contentTypes[key]
}

// recordedPuts returns a copy of the recorded Put calls.
func (f *fakeStorage) recordedPuts() []putCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]putCall(nil), f.puts...)
}

// recordedGets returns a copy of the recorded Get calls.
func (f *fakeStorage) recordedGets() []getCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]getCall(nil), f.getCalls...)
}

// lastBody returns the most recent body reader Get handed out, so a test can
// assert how the response consumed the object.
func (f *fakeStorage) lastBody() *chunkReader {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// deletedKeys returns a copy of the keys Delete removed.
func (f *fakeStorage) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// stored returns the bytes Get and Stat serve for key, with a corruption
// override taking precedence over the uploaded object, together with the
// recorded content type and whether the key exists. The caller holds the lock.
func (f *fakeStorage) stored(key string) ([]byte, string, bool) {
	if f.overrideKey == key {
		return f.override, f.contentTypes[key], true
	}
	content, ok := f.objects[key]
	return content, f.contentTypes[key], ok
}

// corrupt makes Get and Stat serve content for key instead of the bytes that
// were uploaded, so a test can drive the stored bytes apart from the digest
// taken at upload time. The recorded Put calls are left as they were.
func (f *fakeStorage) corrupt(key string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrideKey = key
	f.override = append([]byte(nil), content...)
}

// Put streams body into memory and records the call. The whole body is never
// needed by the handler at once, so the copy stays a plain io.Copy.
func (f *fakeStorage) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	f.mu.Lock()
	putErr := f.putErr
	f.mu.Unlock()
	if putErr != nil {
		return putErr
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return err
	}
	content := buf.Bytes()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = content
	f.contentTypes[key] = contentType
	f.putKeys = append(f.putKeys, key)
	f.puts = append(f.puts, putCall{key: key, size: size, contentType: contentType, body: content})
	return nil
}

// Get returns the stored object, honouring offset and length so a test can
// assert the range the handler asked for.
func (f *fakeStorage) Get(ctx context.Context, key string, offset, length int64) (*storage.Object, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, getCall{key: key, offset: offset, length: length})
	getErr := f.getErr
	content, contentType, ok := f.stored(key)
	f.mu.Unlock()

	if getErr != nil {
		return nil, getErr
	}
	if !ok {
		return nil, storage.ErrNotFound
	}

	start := offset
	if start < 0 {
		start = 0
	}
	if start > int64(len(content)) {
		start = int64(len(content))
	}
	end := int64(len(content))
	if length > 0 && start+length < end {
		end = start + length
	}

	body := &chunkReader{data: content[start:end], chunk: fakeChunkSize}

	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()

	return &storage.Object{
		Info: storage.ObjectInfo{
			Key:         key,
			Size:        int64(len(content)),
			ContentType: contentType,
			ETag:        fakeETag(content),
		},
		Body: body,
	}, nil
}

// Stat returns the stored object's metadata.
func (f *fakeStorage) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	f.mu.Lock()
	statErr := f.statErr
	content, contentType, ok := f.stored(key)
	f.mu.Unlock()

	if statErr != nil {
		return storage.ObjectInfo{}, statErr
	}
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}

	return storage.ObjectInfo{
		Key:         key,
		Size:        int64(len(content)),
		ContentType: contentType,
		ETag:        fakeETag(content),
	}, nil
}

// PresignPut returns a placeholder URL; the upload tests do not presign.
func (f *fakeStorage) PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error) {
	return "https://storage.invalid/" + key, nil
}

// PresignGet returns a placeholder URL; the download tests do not presign.
func (f *fakeStorage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "https://storage.invalid/" + key, nil
}

// Delete records the key and drops the object.
func (f *fakeStorage) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	delete(f.contentTypes, key)
	return nil
}

// fakeETag builds a stable ETag from the object bytes, so the same content
// always answers with the same header value.
func fakeETag(content []byte) string {
	return fmt.Sprintf("\"%08x-%d\"", crc32.ChecksumIEEE(content), len(content))
}

// chunkReader is the object body fakeStorage hands out: it yields at most chunk
// bytes per Read and records how often it was read and whether it was closed, so
// a test can tell a streamed copy apart from one that buffered the object.
type chunkReader struct {
	data   []byte
	offset int
	chunk  int
	reads  int
	closed bool
}

var _ io.ReadCloser = (*chunkReader)(nil)

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}

	r.reads++

	n := len(p)
	if r.chunk > 0 && n > r.chunk {
		n = r.chunk
	}
	if remaining := len(r.data) - r.offset; n > remaining {
		n = remaining
	}
	copy(p, r.data[r.offset:r.offset+n])
	r.offset += n
	return n, nil
}

func (r *chunkReader) Close() error {
	r.closed = true
	return nil
}

// countingReadCloser counts the bytes handed to whoever reads it, so a test can
// observe how much of a request body the handler had consumed at a given moment.
type countingReadCloser struct {
	r io.Reader
	n int64
}

var _ io.ReadCloser = (*countingReadCloser)(nil)

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error {
	if closer, ok := c.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
