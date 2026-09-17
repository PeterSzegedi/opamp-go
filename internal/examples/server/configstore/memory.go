package configstore

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryBackend is an in-memory Backend. It is used by the tests and lets the
// example Server run with the same code paths as the S3 backend when no bucket
// is configured, at the cost of losing the configs on restart.
type MemoryBackend struct {
	mux     sync.RWMutex
	objects map[string]Config
	version uint64
}

var _ Backend = (*MemoryBackend)(nil)

// NewMemoryBackend creates an empty in-memory backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{objects: map[string]Config{}}
}

// List implements Backend.
func (b *MemoryBackend) List(_ context.Context) ([]ObjectInfo, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	infos := make([]ObjectInfo, 0, len(b.objects))
	for _, cfg := range b.objects {
		infos = append(infos, ObjectInfo{
			Key:        cfg.Key,
			Version:    cfg.Version,
			ModifiedAt: cfg.ModifiedAt,
		})
	}

	return infos, nil
}

// Get implements Backend.
func (b *MemoryBackend) Get(_ context.Context, key string) (Config, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	cfg, ok := b.objects[key]
	if !ok {
		return Config{}, fmt.Errorf("%q: %w", key, ErrNotFound)
	}
	cfg.Body = bytes.Clone(cfg.Body)

	return cfg, nil
}

// Put implements Backend.
func (b *MemoryBackend) Put(_ context.Context, key string, body []byte) (Config, error) {
	if err := ValidateKey(key); err != nil {
		return Config{}, err
	}

	b.mux.Lock()
	defer b.mux.Unlock()

	b.version++
	cfg := Config{
		Key:        key,
		Body:       bytes.Clone(body),
		Version:    fmt.Sprintf("v%d", b.version),
		ModifiedAt: time.Now().UTC(),
	}
	b.objects[key] = cfg

	return cfg, nil
}

// Delete removes a config, simulating an out-of-band deletion.
func (b *MemoryBackend) Delete(key string) {
	b.mux.Lock()
	defer b.mux.Unlock()

	delete(b.objects, key)
}

// Location implements Backend.
func (b *MemoryBackend) Location() string {
	return "memory://configs"
}
