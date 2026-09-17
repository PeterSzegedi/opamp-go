package data

import "context"

// ConfigStore is the external source of truth for Agent configurations, backed
// by S3 in production (see the configstore package).
//
// The Server holds no authoritative config of its own: what it offers to an
// Agent is whatever the store returns for that Agent, and a config change is
// only handed out after the store accepted the write. That keeps the Server
// stateless with regard to configuration, so it can be restarted or scaled out
// without Agents losing or flapping their config.
type ConfigStore interface {
	// Resolve returns the config that applies to the Agent with the given
	// instance id and service.name, together with the key it was loaded from.
	// found is false when the store holds no config for the Agent.
	Resolve(instanceID, serviceName string) (key string, body []byte, found bool)

	// DefaultKeyFor returns the key a config for this Agent should be written
	// to when the Agent does not have one yet.
	DefaultKeyFor(instanceID, serviceName string) string

	// Save durably stores body under key.
	Save(ctx context.Context, key string, body []byte) error

	// Location describes where the configs are stored, for display purposes.
	Location() string
}
