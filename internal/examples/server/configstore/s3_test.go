package configstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestS3KeyMapping(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		key       string
		objectKey string
	}{
		{name: "with prefix", prefix: "otel-collector", key: "config.yaml", objectKey: "otel-collector/config.yaml"},
		{name: "nested prefix", prefix: "a/b", key: "billing/prod/config.yaml", objectKey: "a/b/billing/prod/config.yaml"},
		{name: "prefix with slashes", prefix: "/otel-collector/", key: "config.yaml", objectKey: "otel-collector/config.yaml"},
		{name: "empty prefix falls back to the default", prefix: "", key: "config.yaml", objectKey: DefaultPrefix + "/config.yaml"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket", Prefix: test.prefix})

			assert.Equal(t, test.objectKey, backend.objectKey(test.key))

			key, ok := backend.storeKey(test.objectKey)
			assert.True(t, ok)
			assert.Equal(t, test.key, key)
		})
	}
}

func TestS3StoreKeyIgnoresUnrelatedObjects(t *testing.T) {
	backend := NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket", Prefix: "otel-collector"})

	ignored := []string{
		"other-prefix/config.yaml",
		"otel-collector/",
		"otel-collectorX/config.yaml",
	}
	for _, objectKey := range ignored {
		_, ok := backend.storeKey(objectKey)
		assert.False(t, ok, objectKey)
	}
}

func TestS3Location(t *testing.T) {
	assert.Equal(t, "s3://bucket/otel-collector",
		NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket", Prefix: "otel-collector"}).Location())

	assert.Equal(t, "s3://bucket/"+DefaultPrefix,
		NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket"}).Location())
}

func TestNormalizeETag(t *testing.T) {
	assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", normalizeETag(`"d41d8cd98f00b204e9800998ecf8427e"`))
	assert.Equal(t, "", normalizeETag(""))
}
