// Package files holds the metadata of stored files.
package files

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when no file has the requested id.
var ErrNotFound = errors.New("file not found")

// File is the metadata of one stored object. Key is the object-store key and is
// deliberately not part of the JSON the API returns.
type File struct {
	ID          string    `json:"id"`
	Key         string    `json:"-"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	SHA256      string    `json:"sha256,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Store keeps the file metadata. The feature tickets use Add, Get, List and
// Delete; Replace lets the hash ticket fill in a value after the upload.
type Store interface {
	Add(File) error
	Get(id string) (File, bool)
	List() []File
	Replace(File) error
	Delete(id string) error
}

// Memory is an in-memory, concurrency-safe Store.
type Memory struct {
	mu    sync.RWMutex
	files map[string]File
}

// NewMemory returns an empty metadata store.
func NewMemory() *Memory {
	return &Memory{files: make(map[string]File)}
}

// NewID returns a new RFC 4122 version 4 UUID for a file id.
func NewID() string {
	return uuid.NewString()
}

func (m *Memory) Add(file File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.files[file.ID]; exists {
		return errors.New("file already exists")
	}
	m.files[file.ID] = file
	return nil
}

func (m *Memory) Get(id string) (File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	file, ok := m.files[id]
	return file, ok
}

// List returns the files newest first, so a listing page has a stable order.
func (m *Memory) List() []File {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]File, 0, len(m.files))
	for _, file := range m.files {
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (m *Memory) Replace(file File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.files[file.ID]; !exists {
		return ErrNotFound
	}
	m.files[file.ID] = file
	return nil
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.files[id]; !exists {
		return ErrNotFound
	}
	delete(m.files, id)
	return nil
}
