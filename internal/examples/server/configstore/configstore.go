// Package configstore keeps the plain OpenTelemetry Collector config files that
// the OpAMP Server offers to Agents in an external object store (typically S3)
// and keeps an in-memory cache of them in sync.
//
// The object store is the source of truth. The Server never invents a config on
// its own: every config change made through the admin UI is written to the store
// first and is only then handed to the Agents, and any change made directly in
// the store (by a CI pipeline, GitOps job or a human with the AWS CLI) is picked
// up by the periodic sync and pushed to the affected Agents.
//
// # Layout
//
// Agents are addressed by the component they run and the stack they run in, the
// two attributes they report in their AgentDescription. Individual instances and
// hosts are deliberately not addressable: every instance of a component in a
// stack runs the same config.
//
// Config files are plain YAML documents, stored verbatim so that they can be
// read and edited with any tool:
//
//	<bucket>/<prefix>/<component>/<stack>/config.yaml   config for a component in one stack
//	<bucket>/<prefix>/<component>/config.yaml           config for a component in every stack
//	<bucket>/<prefix>/config.yaml                       fallback config for every other Agent
//
// An Agent gets the config from the first of those folders that holds one, so a
// component/stack file overrides a component wide file, which in turn overrides
// the fallback. Keys used by this package are always relative to the configured
// prefix, which defaults to DefaultPrefix.
//
// Exactly one config file per folder is expected. The Server always writes
// ConfigFile, but a file that a pipeline put there under a different name is
// picked up just as well; if a folder holds several config files, the first one
// in lexicographic order wins.
package configstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Well known locations inside the store.
const (
	// DefaultPrefix is the key under the bucket that the config files live under.
	DefaultPrefix = "otel-collector"
	// ConfigFile is the name of the config file inside a folder. It is the name
	// the Server writes to when it creates a config file.
	ConfigFile = "config.yaml"
)

// ConfigExtensions are the object suffixes recognized as config files. Objects
// under the prefix that do not end with one of these are ignored, so the bucket
// can also hold documentation, checksums or other bookkeeping files.
var ConfigExtensions = []string{".yaml", ".yml"}

// DefaultSyncInterval is how often the store is polled for out-of-band changes.
const DefaultSyncInterval = 30 * time.Second

// ErrNotFound is returned by Backend.Get for a key that does not exist.
var ErrNotFound = errors.New("config not found")

// Config is a single plain config file held by the backend.
type Config struct {
	// Key of the config file, relative to the backend prefix.
	Key string
	// Body is the verbatim content of the config file.
	Body []byte
	// Version is an opaque, backend specific version (the S3 ETag) that changes
	// whenever the content changes. It is used to avoid re-downloading unchanged
	// objects on every sync.
	Version string
	// ModifiedAt is when the config file was last written.
	ModifiedAt time.Time
}

// ObjectInfo is the metadata of a config file, as returned by a listing.
type ObjectInfo struct {
	Key        string
	Version    string
	ModifiedAt time.Time
}

// Backend is the storage that holds the config files. It is deliberately small
// so that S3, an S3 compatible store (MinIO, LocalStack) or an in-memory fake
// can all be plugged into the same Store.
type Backend interface {
	// List returns the metadata of every config file in the backend.
	List(ctx context.Context) ([]ObjectInfo, error)
	// Get returns the config file stored under key, or ErrNotFound.
	Get(ctx context.Context, key string) (Config, error)
	// Put stores body under key and returns the stored config.
	Put(ctx context.Context, key string, body []byte) (Config, error)
	// Location is a human readable description of where configs live, such as
	// "s3://my-bucket/otel-collector".
	Location() string
}

// Options configures a Store.
type Options struct {
	// SyncInterval is how often the backend is polled for changes made outside
	// of this Server. Defaults to DefaultSyncInterval.
	SyncInterval time.Duration
	// Logger receives sync progress and errors. Defaults to the standard logger.
	Logger *log.Logger
}

// Store is a cached, periodically synchronized view of a Backend.
//
// Reads (Resolve) are served from the cache so that they never block an Agent's
// status update on a network round trip, while writes (Save) go straight to the
// backend and only update the cache once the backend accepted them.
type Store struct {
	backend      Backend
	syncInterval time.Duration
	logger       *log.Logger

	mux     sync.RWMutex
	configs map[string]Config
	// folders maps a folder to the config file it holds, so that an Agent can be
	// resolved to a config without scanning every key.
	folders map[string]string
	// loaded is set after the first successful sync, so that the initial load is
	// not reported as a series of changes.
	loaded bool

	onChangeMux sync.Mutex
	onChange    []func()

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}
}

// New creates a Store on top of backend. Call Start to load the configs and to
// begin watching the backend for changes.
func New(backend Backend, opts Options) *Store {
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = DefaultSyncInterval
	}
	if opts.Logger == nil {
		opts.Logger = log.New(
			log.Default().Writer(),
			"[CONFIGSTORE] ",
			log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds,
		)
	}

	return &Store{
		backend:      backend,
		syncInterval: opts.SyncInterval,
		logger:       opts.Logger,
		configs:      map[string]Config{},
		folders:      map[string]string{},
		done:         make(chan struct{}),
	}
}

// Start performs the initial synchronization and then keeps syncing in the
// background until Stop is called or ctx is cancelled. It fails if the initial
// synchronization fails, so that a misconfigured or unreachable backend is
// reported at startup instead of silently handing out empty configs.
func (s *Store) Start(ctx context.Context) error {
	if _, err := s.Sync(ctx); err != nil {
		return fmt.Errorf("initial sync from %s failed: %w", s.backend.Location(), err)
	}

	s.logger.Printf("Loaded %d config(s) from %s", len(s.Keys()), s.backend.Location())

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	go s.syncLoop(loopCtx)

	return nil
}

// Stop ends the background synchronization and waits for it to finish.
func (s *Store) Stop() {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
			<-s.done
		} else {
			close(s.done)
		}
	})
}

func (s *Store) syncLoop(ctx context.Context) {
	defer close(s.done)

	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := s.Sync(ctx)
			if err != nil {
				// Keep serving the configs we already have. The next tick retries.
				s.logger.Printf("Sync from %s failed, keeping the last known configs: %v",
					s.backend.Location(), err)
				continue
			}
			if changed {
				s.notifyChanged()
			}
		}
	}
}

// OnChange registers fn to be called after a background sync picked up a change
// in the backend. fn is called from the sync goroutine, outside of the Store
// lock, so it is free to call back into the Store.
func (s *Store) OnChange(fn func()) {
	s.onChangeMux.Lock()
	defer s.onChangeMux.Unlock()
	s.onChange = append(s.onChange, fn)
}

func (s *Store) notifyChanged() {
	s.onChangeMux.Lock()
	watchers := slices.Clone(s.onChange)
	s.onChangeMux.Unlock()

	for _, fn := range watchers {
		fn()
	}
}

// Sync refreshes the cache from the backend and reports whether anything
// changed. Objects whose version is unchanged are not downloaded again.
func (s *Store) Sync(ctx context.Context) (bool, error) {
	infos, err := s.backend.List(ctx)
	if err != nil {
		return false, fmt.Errorf("listing configs: %w", err)
	}

	s.mux.RLock()
	cached := maps.Clone(s.configs)
	isInitialLoad := !s.loaded
	s.mux.RUnlock()

	next := make(map[string]Config, len(infos))
	changed := false

	for _, info := range infos {
		if prev, ok := cached[info.Key]; ok && prev.Version != "" && prev.Version == info.Version {
			// Unchanged since the last sync, reuse the cached body.
			next[info.Key] = prev
			continue
		}

		cfg, err := s.backend.Get(ctx, info.Key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Deleted between the listing and the fetch. Treat it as deleted.
				continue
			}
			return false, fmt.Errorf("fetching config %q: %w", info.Key, err)
		}

		next[info.Key] = cfg

		if prev, ok := cached[info.Key]; !ok || !bytes.Equal(prev.Body, cfg.Body) {
			changed = true
			if !isInitialLoad {
				s.logger.Printf("Config %q changed in %s", info.Key, s.backend.Location())
			}
		}
	}

	for key := range cached {
		if _, ok := next[key]; !ok {
			changed = true
			s.logger.Printf("Config %q was removed from %s", key, s.backend.Location())
		}
	}

	s.mux.Lock()
	s.configs = next
	s.reindexLocked()
	s.loaded = true
	s.mux.Unlock()

	return changed && !isInitialLoad, nil
}

// Resolve returns the config that applies to the Agent running the given
// component in the given stack, looking up the candidate folders from the most
// to the least specific one. found is false when the store holds no config for
// the Agent at all.
func (s *Store) Resolve(component, stack string) (key string, body []byte, found bool) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	for _, folder := range CandidateFolders(component, stack) {
		candidate, ok := s.folders[folder]
		if !ok {
			continue
		}

		cfg, ok := s.configs[candidate]
		if !ok {
			continue
		}

		return cfg.Key, bytes.Clone(cfg.Body), true
	}

	return "", nil, false
}

// DefaultKeyFor returns the key a config should be written to when the Agent
// does not have one yet: the config file of its own component and stack.
func (s *Store) DefaultKeyFor(component, stack string) string {
	return CandidateKeys(component, stack)[0]
}

// Save writes body to key in the backend and, once the backend accepted it,
// updates the cache. The write is what makes the change durable, everything
// else (the UI, the Agents) is derived from it.
func (s *Store) Save(ctx context.Context, key string, body []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	cfg, err := s.backend.Put(ctx, key, body)
	if err != nil {
		return fmt.Errorf("storing config %q in %s: %w", key, s.backend.Location(), err)
	}

	s.mux.Lock()
	s.configs[cfg.Key] = cfg
	s.reindexLocked()
	s.mux.Unlock()

	s.logger.Printf("Stored config %q in %s (%d bytes)", cfg.Key, s.backend.Location(), len(body))

	return nil
}

// Get returns a cached config by key.
func (s *Store) Get(key string) (Config, bool) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	cfg, ok := s.configs[key]
	if !ok {
		return Config{}, false
	}
	cfg.Body = bytes.Clone(cfg.Body)

	return cfg, true
}

// Keys returns the sorted keys of all cached configs.
func (s *Store) Keys() []string {
	s.mux.RLock()
	defer s.mux.RUnlock()

	keys := make([]string, 0, len(s.configs))
	for key := range s.configs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// Location describes where the configs are stored, for display purposes.
func (s *Store) Location() string {
	return s.backend.Location()
}

// CandidateKeys returns the keys that may hold the config of an Agent running
// the given component in the given stack, from the most to the least specific
// one. The first key is where a config for that Agent is created.
func CandidateKeys(component, stack string) []string {
	folders := CandidateFolders(component, stack)

	keys := make([]string, 0, len(folders))
	for _, folder := range folders {
		keys = append(keys, path.Join(folder, ConfigFile))
	}

	return keys
}

// CandidateFolders returns the folders that may hold the config of an Agent
// running the given component in the given stack, from the most to the least
// specific one. The last folder is always the root of the prefix, which holds
// the fallback config. Attribute values that cannot produce a safe key segment
// are skipped.
func CandidateFolders(component, stack string) []string {
	folders := make([]string, 0, 3)

	if c := sanitizeSegment(component); c != "" {
		if s := sanitizeSegment(stack); s != "" {
			folders = append(folders, path.Join(c, s))
		}
		folders = append(folders, c)
	}

	// The root of the prefix, holding the config for every other Agent.
	return append(folders, "")
}

// reindexLocked rebuilds the folder to config file index. Folders that hold more
// than one config file resolve to the first key in lexicographic order, so that
// the config an Agent gets never depends on the order of a listing. The caller
// must hold the write lock.
func (s *Store) reindexLocked() {
	keys := make([]string, 0, len(s.configs))
	for key := range s.configs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	s.folders = make(map[string]string, len(keys))
	for _, key := range keys {
		folder := folderOf(key)
		if _, ok := s.folders[folder]; !ok {
			s.folders[folder] = key
		}
	}
}

// folderOf returns the folder a key lives in, "" for a key at the root of the
// prefix.
func folderOf(key string) string {
	folder := path.Dir(key)
	if folder == "." || folder == "/" {
		return ""
	}

	return folder
}

// ValidateKey checks that key is a safe, relative key of a config file. It is
// applied to every key that reaches the backend, including the ones typed into
// the admin UI, so that a config cannot be written outside of the prefix.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("config key must not be empty")
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("config key %q must be relative to the store prefix", key)
	case strings.HasSuffix(key, "/"):
		return fmt.Errorf("config key %q must name a file, not a folder", key)
	case strings.ContainsAny(key, "\\\n\r"):
		return fmt.Errorf("config key %q contains invalid characters", key)
	case hasParentSegment(key):
		return fmt.Errorf("config key %q must not escape the store prefix", key)
	case path.Clean(key) != key:
		return fmt.Errorf("config key %q must be a clean relative path", key)
	case !HasConfigExtension(key):
		return fmt.Errorf("config key %q must end with one of %s",
			key, strings.Join(ConfigExtensions, ", "))
	}

	return nil
}

// HasConfigExtension reports whether key names a config file.
func HasConfigExtension(key string) bool {
	for _, ext := range ConfigExtensions {
		if strings.HasSuffix(key, ext) {
			return true
		}
	}

	return false
}

// hasParentSegment reports whether the key walks up the hierarchy. path.Clean
// keeps leading ".." segments, so they have to be rejected explicitly.
func hasParentSegment(key string) bool {
	for _, segment := range strings.Split(key, "/") {
		if segment == ".." {
			return true
		}
	}

	return false
}

// sanitizeSegment turns an arbitrary attribute value, such as a component or a
// stack name, into a single safe key segment. It returns an empty string for
// values that cannot produce a meaningful segment.
func sanitizeSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	hasAlphanumeric := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasAlphanumeric = true
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	if !hasAlphanumeric {
		// Nothing identifying is left, e.g. ".." or "///". Such a value must not
		// become a key of its own.
		return ""
	}

	// Avoid keys that would name a hidden or relative path.
	return strings.Trim(b.String(), ".")
}
