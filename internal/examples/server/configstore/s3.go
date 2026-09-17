package configstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// DefaultContentType is used for config objects written by the Server.
const DefaultContentType = "application/yaml"

// S3Settings configures the S3 backend.
type S3Settings struct {
	// Bucket holding the config files. Required.
	Bucket string
	// Prefix is the "folder" inside the bucket that the config files live in.
	// Keys handled by the Store are relative to it.
	Prefix string
	// Region of the bucket. When empty the region is taken from the usual AWS
	// configuration sources (AWS_REGION, shared config, instance metadata).
	Region string
	// Endpoint overrides the S3 endpoint, for S3 compatible stores such as
	// MinIO or LocalStack. Optional.
	Endpoint string
	// UsePathStyle addresses buckets as <endpoint>/<bucket> instead of
	// <bucket>.<endpoint>. Required by most S3 compatible stores.
	UsePathStyle bool
	// ContentType of the objects written by the Server. Defaults to
	// DefaultContentType.
	ContentType string
	// SSEKMSKeyID enables SSE-KMS with the given key for objects written by the
	// Server. Optional: when empty, the bucket's default encryption applies.
	SSEKMSKeyID string
}

// S3Backend stores the plain config files as objects in an S3 bucket.
//
// The objects are written verbatim, with no wrapping or encoding, so that a
// config can be inspected and edited with any S3 client:
//
//	aws s3 cp s3://my-bucket/otel-configs/default.yaml -
//
// Bucket versioning is recommended: it gives a full history of every config
// change and makes rollbacks a bucket operation rather than a code change.
type S3Backend struct {
	client   *s3.Client
	settings S3Settings
	prefix   string
	location string
}

var _ Backend = (*S3Backend)(nil)

// NewS3Backend creates a backend backed by the given bucket, using the default
// AWS credential chain.
func NewS3Backend(ctx context.Context, settings S3Settings) (*S3Backend, error) {
	if settings.Bucket == "" {
		return nil, errors.New("s3 bucket must be set")
	}
	if settings.ContentType == "" {
		settings.ContentType = DefaultContentType
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	if settings.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(settings.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if settings.Endpoint != "" {
			o.BaseEndpoint = aws.String(settings.Endpoint)
		}
		if settings.UsePathStyle {
			o.UsePathStyle = true
		}
	})

	return NewS3BackendWithClient(client, settings), nil
}

// NewS3BackendWithClient creates a backend that uses an already configured S3
// client, for callers that need full control over the client (custom
// credentials, retries, instrumentation, ...).
func NewS3BackendWithClient(client *s3.Client, settings S3Settings) *S3Backend {
	if settings.ContentType == "" {
		settings.ContentType = DefaultContentType
	}

	prefix := strings.Trim(settings.Prefix, "/")

	location := (&url.URL{Scheme: "s3", Host: settings.Bucket, Path: "/" + prefix}).String()

	return &S3Backend{
		client:   client,
		settings: settings,
		prefix:   prefix,
		location: strings.TrimSuffix(location, "/"),
	}
}

// Location implements Backend.
func (b *S3Backend) Location() string {
	return b.location
}

// List implements Backend. Objects that are not config files (folder markers,
// checksums, docs, ...) are ignored.
func (b *S3Backend) List(ctx context.Context) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(b.settings.Bucket)}
	if b.prefix != "" {
		input.Prefix = aws.String(b.prefix + "/")
	}

	var infos []ObjectInfo

	paginator := s3.NewListObjectsV2Paginator(b.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, obj := range page.Contents {
			objectKey := aws.ToString(obj.Key)

			key, ok := b.storeKey(objectKey)
			if !ok || !HasConfigExtension(key) {
				continue
			}

			infos = append(infos, ObjectInfo{
				Key:        key,
				Version:    normalizeETag(aws.ToString(obj.ETag)),
				ModifiedAt: aws.ToTime(obj.LastModified),
			})
		}
	}

	return infos, nil
}

// Get implements Backend.
func (b *S3Backend) Get(ctx context.Context, key string) (Config, error) {
	if err := ValidateKey(key); err != nil {
		return Config{}, err
	}

	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.settings.Bucket),
		Key:    aws.String(b.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return Config{}, fmt.Errorf("%q: %w", key, ErrNotFound)
		}
		return Config{}, err
	}
	defer out.Body.Close()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return Config{}, fmt.Errorf("reading %q: %w", key, err)
	}

	return Config{
		Key:        key,
		Body:       body,
		Version:    normalizeETag(aws.ToString(out.ETag)),
		ModifiedAt: aws.ToTime(out.LastModified),
	}, nil
}

// Put implements Backend.
func (b *S3Backend) Put(ctx context.Context, key string, body []byte) (Config, error) {
	if err := ValidateKey(key); err != nil {
		return Config{}, err
	}

	input := &s3.PutObjectInput{
		Bucket:      aws.String(b.settings.Bucket),
		Key:         aws.String(b.objectKey(key)),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(b.settings.ContentType),
	}
	if b.settings.SSEKMSKeyID != "" {
		input.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(b.settings.SSEKMSKeyID)
	}

	out, err := b.client.PutObject(ctx, input)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Key:        key,
		Body:       bytes.Clone(body),
		Version:    normalizeETag(aws.ToString(out.ETag)),
		ModifiedAt: time.Now().UTC(),
	}, nil
}

// objectKey maps a store key to the full S3 object key.
func (b *S3Backend) objectKey(key string) string {
	if b.prefix == "" {
		return key
	}

	return path.Join(b.prefix, key)
}

// storeKey maps a full S3 object key back to a store key. It returns false for
// objects outside of the prefix and for folder markers.
func (b *S3Backend) storeKey(objectKey string) (string, bool) {
	key := objectKey
	if b.prefix != "" {
		trimmed, ok := strings.CutPrefix(objectKey, b.prefix+"/")
		if !ok {
			return "", false
		}
		key = trimmed
	}

	if key == "" || strings.HasSuffix(key, "/") {
		return "", false
	}

	return key, true
}

// normalizeETag strips the quotes S3 wraps ETags in.
func normalizeETag(etag string) string {
	return strings.Trim(etag, `"`)
}

func isNotFound(err error) bool {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	var notFound *s3types.NotFound
	return errors.As(err, &notFound)
}
