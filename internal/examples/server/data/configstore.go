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
//
// Agents are addressed by the component they run and the stack they run in, not
// by instance or host: every instance of a component in a stack gets the same
// config.
type ConfigStore interface {
	// Resolve returns the config that applies to the Agents running the given
	// component in the given stack, together with the key it was loaded from.
	// found is false when the store holds no config for them.
	Resolve(component, stack string) (key string, body []byte, found bool)

	// DefaultKeyFor returns the key a config for this component and stack should
	// be written to when they do not have one yet.
	DefaultKeyFor(component, stack string) string

	// Save durably stores body under key.
	Save(ctx context.Context, key string, body []byte) error

	// Location describes where the configs are stored, for display purposes.
	Location() string
}
