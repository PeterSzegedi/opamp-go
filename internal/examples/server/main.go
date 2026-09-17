package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/open-telemetry/opamp-go/internal/examples/server/configstore"
	"github.com/open-telemetry/opamp-go/internal/examples/server/data"
	"github.com/open-telemetry/opamp-go/internal/examples/server/opampsrv"
	"github.com/open-telemetry/opamp-go/internal/examples/server/uisrv"
)

var logger = log.New(log.Default().Writer(), "[MAIN] ", log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds)

// s3Flags are the settings of the S3 backed config store. The store holds the
// plain OTel config files that are handed out to the Agents and is the source
// of truth for them: see the configstore package for the layout of the bucket.
type s3Flags struct {
	bucket       string
	prefix       string
	region       string
	endpoint     string
	usePathStyle bool
	kmsKeyID     string
	syncInterval time.Duration
}

func main() {
	var emitMetrics bool
	flag.BoolVar(&emitMetrics, "emit-metrics", false, "Emit metrics to stdout.")

	var noTLS bool
	flag.BoolVar(&noTLS, "no-tls", false, "Serve the OpAMP endpoint without TLS, accepting plaintext (ws://) connections. Useful when testing OpAMP clients that do not support TLS yet.")

	var s3 s3Flags
	flag.StringVar(&s3.bucket, "s3-bucket", envOr("OPAMP_S3_BUCKET", ""),
		"S3 bucket holding the agent config files. When empty, configs are kept in memory only and are lost on restart.")
	flag.StringVar(&s3.prefix, "s3-prefix", envOr("OPAMP_S3_PREFIX", configstore.DefaultPrefix),
		"Key inside the S3 bucket that the config files live under.")
	flag.StringVar(&s3.region, "s3-region", envOr("OPAMP_S3_REGION", ""),
		"Region of the S3 bucket. Defaults to the region from the usual AWS configuration sources.")
	flag.StringVar(&s3.endpoint, "s3-endpoint", envOr("OPAMP_S3_ENDPOINT", ""),
		"Override the S3 endpoint, for S3 compatible stores such as MinIO or LocalStack.")
	flag.BoolVar(&s3.usePathStyle, "s3-path-style", envBoolOr("OPAMP_S3_PATH_STYLE", false),
		"Use path style bucket addressing (<endpoint>/<bucket>). Required by most S3 compatible stores.")
	flag.StringVar(&s3.kmsKeyID, "s3-kms-key-id", envOr("OPAMP_S3_KMS_KEY_ID", ""),
		"KMS key to encrypt config files written by the Server with. Defaults to the bucket's default encryption.")
	flag.DurationVar(&s3.syncInterval, "s3-sync-interval", envDurationOr("OPAMP_S3_SYNC_INTERVAL", configstore.DefaultSyncInterval),
		"How often the Server polls the bucket for config changes made outside of the Server.")

	flag.Parse()

	curDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	logger.Println("OpAMP Server starting...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	configStore, err := startConfigStore(ctx, s3)
	if err != nil {
		// Do not start handing out configs we cannot store or refresh.
		logger.Fatalf("Cannot start the config store: %v", err)
	}

	uisrv.Start(curDir)
	opampSrv := opampsrv.NewServer(&data.AllAgents, emitMetrics)
	opampSrv.Start(noTLS)

	logger.Println("OpAMP Server running...")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt

	logger.Println("OpAMP Server shutting down...")
	uisrv.Shutdown()
	opampSrv.Stop()
	if configStore != nil {
		configStore.Stop()
	}
}

// startConfigStore creates the config store, loads the configs it already holds
// and starts watching it for changes. It returns nil when no bucket is
// configured, in which case configs are kept in memory only.
func startConfigStore(ctx context.Context, s3 s3Flags) (*configstore.Store, error) {
	if s3.bucket == "" {
		logger.Println("No S3 bucket configured (-s3-bucket), agent configs are kept in memory only.")
		return nil, nil
	}

	backend, err := configstore.NewS3Backend(ctx, configstore.S3Settings{
		Bucket:       s3.bucket,
		Prefix:       s3.prefix,
		Region:       s3.region,
		Endpoint:     s3.endpoint,
		UsePathStyle: s3.usePathStyle,
		SSEKMSKeyID:  s3.kmsKeyID,
	})
	if err != nil {
		return nil, err
	}

	store := configstore.New(backend, configstore.Options{SyncInterval: s3.syncInterval})

	// Agents are configured from the store, and every change picked up from the
	// store is pushed to the Agents it applies to.
	data.AllAgents.SetConfigStore(store)
	store.OnChange(data.AllAgents.ReloadConfigsFromStore)

	if err := store.Start(ctx); err != nil {
		return nil, err
	}

	logger.Printf("Agent configs are served from %s (syncing every %s)", store.Location(), s3.syncInterval)

	return store, nil
}

func envOr(name, defaultValue string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}

	return defaultValue
}

func envBoolOr(name string, defaultValue bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		logger.Printf("Ignoring invalid boolean value %q in %s: %v", value, name, err)
		return defaultValue
	}

	return parsed
}

func envDurationOr(name string, defaultValue time.Duration) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		logger.Printf("Ignoring invalid duration %q in %s: %v", value, name, err)
		return defaultValue
	}

	return parsed
}
