package configstore

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testOptions() Options {
	return Options{
		SyncInterval: 10 * time.Millisecond,
		Logger:       log.New(io.Discard, "", 0),
	}
}

func newTestStore(t *testing.T, backend Backend) *Store {
	t.Helper()

	store := New(backend, testOptions())
	require.NoError(t, store.Start(context.Background()))
	t.Cleanup(store.Stop)

	return store
}

func TestResolvePrefersMostSpecificConfig(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	store := New(backend, testOptions())

	require.NoError(t, store.Save(ctx, ConfigFile, []byte("fallback")))

	key, body, found := store.Resolve("billing", "prod")
	require.True(t, found)
	assert.Equal(t, ConfigFile, key)
	assert.Equal(t, "fallback", string(body))

	require.NoError(t, store.Save(ctx, "billing/config.yaml", []byte("component")))

	key, body, found = store.Resolve("billing", "prod")
	require.True(t, found)
	assert.Equal(t, "billing/config.yaml", key)
	assert.Equal(t, "component", string(body))

	// Another component still falls back to the config at the root.
	key, _, found = store.Resolve("shipping", "prod")
	require.True(t, found)
	assert.Equal(t, ConfigFile, key)

	require.NoError(t, store.Save(ctx, "billing/prod/config.yaml", []byte("component in stack")))

	key, body, found = store.Resolve("billing", "prod")
	require.True(t, found)
	assert.Equal(t, "billing/prod/config.yaml", key)
	assert.Equal(t, "component in stack", string(body))

	// The same component in another stack is unaffected.
	key, body, found = store.Resolve("billing", "staging")
	require.True(t, found)
	assert.Equal(t, "billing/config.yaml", key)
	assert.Equal(t, "component", string(body))
}

// A config file is picked up whatever it is called, so a pipeline is free to
// name the single file in a folder after the component, the release, ...
func TestResolveFindsTheConfigFileInTheFolder(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	_, err := backend.Put(ctx, "billing/prod/otelcol.yaml", []byte("named config"))
	require.NoError(t, err)

	store := newTestStore(t, backend)

	key, body, found := store.Resolve("billing", "prod")
	require.True(t, found)
	assert.Equal(t, "billing/prod/otelcol.yaml", key)
	assert.Equal(t, "named config", string(body))

	// A folder is expected to hold a single config file. If it holds more, the
	// resolution must still be stable rather than depend on the map order.
	_, err = backend.Put(ctx, "billing/prod/a-otelcol.yaml", []byte("first in order"))
	require.NoError(t, err)
	_, err = store.Sync(ctx)
	require.NoError(t, err)

	for range 5 {
		key, _, found = store.Resolve("billing", "prod")
		require.True(t, found)
		assert.Equal(t, "billing/prod/a-otelcol.yaml", key)
	}
}

func TestResolveReportsMissingConfig(t *testing.T) {
	store := New(NewMemoryBackend(), testOptions())

	key, body, found := store.Resolve("billing", "prod")
	assert.False(t, found)
	assert.Empty(t, key)
	assert.Nil(t, body)

	// The Agent has no config yet, but we know where it would belong.
	assert.Equal(t, "billing/prod/config.yaml", store.DefaultKeyFor("billing", "prod"))
}

func TestCandidateKeysSanitizesNames(t *testing.T) {
	assert.Equal(t,
		[]string{"my_component_1/prod-eu/config.yaml", "my_component_1/config.yaml", ConfigFile},
		CandidateKeys("my/component 1", "prod-eu"),
	)

	// An agent that does not report a stack shares the config of its component.
	assert.Equal(t,
		[]string{"billing/config.yaml", ConfigFile},
		CandidateKeys("billing", ""),
	)

	// Names that cannot produce a safe key segment are skipped.
	assert.Equal(t, []string{ConfigFile}, CandidateKeys("../..", "prod"))
}

func TestStartLoadsExistingConfigs(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	_, err := backend.Put(ctx, ConfigFile, []byte("fallback"))
	require.NoError(t, err)
	_, err = backend.Put(ctx, "billing/prod/config.yaml", []byte("component in stack"))
	require.NoError(t, err)

	store := newTestStore(t, backend)

	assert.Equal(t, []string{"billing/prod/config.yaml", ConfigFile}, store.Keys())
}

func TestStartFailsWhenBackendIsUnreachable(t *testing.T) {
	store := New(&failingBackend{err: errors.New("no such bucket")}, testOptions())

	err := store.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such bucket")
}

func TestSyncNotifiesAboutOutOfBandChanges(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	_, err := backend.Put(ctx, ConfigFile, []byte("v1"))
	require.NoError(t, err)

	store := New(backend, testOptions())

	changes := make(chan struct{}, 10)
	store.OnChange(func() { changes <- struct{}{} })

	require.NoError(t, store.Start(ctx))
	t.Cleanup(store.Stop)

	// Nothing changed yet, so no notification is expected.
	select {
	case <-changes:
		t.Fatal("unexpected change notification")
	case <-time.After(50 * time.Millisecond):
	}

	// A change made directly in the backend must be picked up and announced.
	_, err = backend.Put(ctx, ConfigFile, []byte("v2"))
	require.NoError(t, err)

	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the change notification")
	}

	_, body, found := store.Resolve("billing", "prod")
	require.True(t, found)
	assert.Equal(t, "v2", string(body))

	// Deletions are announced as well.
	backend.Delete(ConfigFile)

	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the deletion notification")
	}

	_, _, found = store.Resolve("billing", "prod")
	assert.False(t, found)
}

func TestSyncDoesNotRefetchUnchangedConfigs(t *testing.T) {
	ctx := context.Background()
	backend := &countingBackend{Backend: NewMemoryBackend()}
	_, err := backend.Put(ctx, ConfigFile, []byte("v1"))
	require.NoError(t, err)

	store := New(backend, testOptions())
	_, err = store.Sync(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, backend.gets())

	changed, err := store.Sync(ctx)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, backend.gets(), "unchanged config must not be downloaded again")

	_, err = backend.Put(ctx, ConfigFile, []byte("v2"))
	require.NoError(t, err)

	changed, err = store.Sync(ctx)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 2, backend.gets())
}

func TestSyncFailureKeepsTheLastKnownConfigs(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryBackend()
	_, err := memory.Put(ctx, ConfigFile, []byte("v1"))
	require.NoError(t, err)

	backend := &failingBackend{Backend: memory}
	store := New(backend, testOptions())
	require.NoError(t, store.Start(ctx))
	t.Cleanup(store.Stop)

	backend.setError(errors.New("s3 is having a bad day"))

	_, err = store.Sync(ctx)
	require.Error(t, err)

	_, body, found := store.Resolve("billing", "prod")
	require.True(t, found, "a failed sync must not drop the configs we already have")
	assert.Equal(t, "v1", string(body))
}

func TestSaveRejectsUnsafeKeys(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	store := New(backend, testOptions())

	require.Error(t, store.Save(ctx, "../outside.yaml", []byte("x")))
	require.Error(t, store.Save(ctx, "/absolute.yaml", []byte("x")))
	require.Error(t, store.Save(ctx, "not-a-config.json", []byte("x")))

	assert.Empty(t, store.Keys(), "nothing must be written for a rejected key")
}

func TestSaveWritesThroughToTheBackend(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()
	store := New(backend, testOptions())

	require.NoError(t, store.Save(ctx, ConfigFile, []byte("v1")))

	stored, err := backend.Get(ctx, ConfigFile)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(stored.Body))

	cached, ok := store.Get(ConfigFile)
	require.True(t, ok)
	assert.Equal(t, "v1", string(cached.Body))
	assert.Equal(t, stored.Version, cached.Version)
}

func TestSaveFailureIsReported(t *testing.T) {
	backend := &failingBackend{Backend: NewMemoryBackend()}
	backend.setError(errors.New("access denied"))

	store := New(backend, testOptions())

	err := store.Save(context.Background(), ConfigFile, []byte("v1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access denied")

	_, ok := store.Get(ConfigFile)
	assert.False(t, ok, "a rejected write must not end up in the cache")
}

func TestValidateKey(t *testing.T) {
	valid := []string{
		ConfigFile,
		"config.yml",
		"billing/config.yaml",
		"billing/prod/config.yaml",
		"billing/prod/otelcol.yaml",
	}
	for _, key := range valid {
		assert.NoError(t, ValidateKey(key), key)
	}

	invalid := []string{
		"",
		"/config.yaml",
		"billing/",
		"../config.yaml",
		"billing/../../config.yaml",
		"./config.yaml",
		"billing//config.yaml",
		"billing\\config.yaml",
		"config.json",
		"config.yaml\nx-amz-meta: evil",
	}
	for _, key := range invalid {
		assert.Error(t, ValidateKey(key), key)
	}
}

// countingBackend counts the Get calls made against the wrapped backend.
type countingBackend struct {
	Backend

	mux      sync.Mutex
	getCalls int
}

func (b *countingBackend) Get(ctx context.Context, key string) (Config, error) {
	b.mux.Lock()
	b.getCalls++
	b.mux.Unlock()

	return b.Backend.Get(ctx, key)
}

func (b *countingBackend) gets() int {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.getCalls
}

// failingBackend fails every operation with a configurable error.
type failingBackend struct {
	Backend

	mux sync.Mutex
	err error
}

func (b *failingBackend) setError(err error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	b.err = err
}

func (b *failingBackend) failure() error {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.err
}

func (b *failingBackend) List(ctx context.Context) ([]ObjectInfo, error) {
	if err := b.failure(); err != nil {
		return nil, err
	}

	return b.Backend.List(ctx)
}

func (b *failingBackend) Get(ctx context.Context, key string) (Config, error) {
	if err := b.failure(); err != nil {
		return Config{}, err
	}

	return b.Backend.Get(ctx, key)
}

func (b *failingBackend) Put(ctx context.Context, key string, body []byte) (Config, error) {
	if err := b.failure(); err != nil {
		return Config{}, err
	}

	return b.Backend.Put(ctx, key, body)
}

func (b *failingBackend) Location() string {
	if b.Backend == nil {
		return "failing://backend"
	}

	return b.Backend.Location()
}
