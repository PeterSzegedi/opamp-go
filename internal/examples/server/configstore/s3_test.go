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
		{name: "with prefix", prefix: "otel-configs", key: "default.yaml", objectKey: "otel-configs/default.yaml"},
		{name: "nested prefix", prefix: "a/b", key: "instances/1.yaml", objectKey: "a/b/instances/1.yaml"},
		{name: "prefix with slashes", prefix: "/otel-configs/", key: "default.yaml", objectKey: "otel-configs/default.yaml"},
		{name: "no prefix", prefix: "", key: "default.yaml", objectKey: "default.yaml"},
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
	backend := NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket", Prefix: "otel-configs"})

	ignored := []string{
		"other-prefix/default.yaml",
		"otel-configs/",
		"otel-configsX/default.yaml",
	}
	for _, objectKey := range ignored {
		_, ok := backend.storeKey(objectKey)
		assert.False(t, ok, objectKey)
	}
}

func TestS3Location(t *testing.T) {
	assert.Equal(t, "s3://bucket/otel-configs",
		NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket", Prefix: "otel-configs"}).Location())

	assert.Equal(t, "s3://bucket",
		NewS3BackendWithClient(nil, S3Settings{Bucket: "bucket"}).Location())
}

func TestNormalizeETag(t *testing.T) {
	assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", normalizeETag(`"d41d8cd98f00b204e9800998ecf8427e"`))
	assert.Equal(t, "", normalizeETag(""))
}
