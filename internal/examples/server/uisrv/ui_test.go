package uisrv

import (
	"bytes"
	"context"
	"html/template"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-telemetry/opamp-go/internal/examples/html"
	"github.com/open-telemetry/opamp-go/internal/examples/server/data"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// fakeConnection is a types.Connection that does nothing. It must be comparable
// because Agents stores connections as map keys.
type fakeConnection struct{ id int }

func (fakeConnection) Connection() net.Conn { return nil }

func (fakeConnection) Send(context.Context, *protobufs.ServerToAgent) error { return nil }

func (fakeConnection) Disconnect() error { return nil }

// fakeConfigStore is a data.ConfigStore that only reports its location, which is
// all the agent page needs.
type fakeConfigStore struct{ location string }

func (s fakeConfigStore) Resolve(string, string) (string, []byte, bool) { return "", nil, false }

func (s fakeConfigStore) DefaultKeyFor(instanceID, _ string) string {
	return "instances/" + instanceID + ".yaml"
}

func (s fakeConfigStore) Save(context.Context, string, []byte) error { return nil }

func (s fakeConfigStore) Location() string { return s.location }

func renderAgentTemplate(t *testing.T, agent *data.Agent) string {
	t.Helper()

	tmpl, err := template.ParseFS(html.HtmlFS, "html/*")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, tmpl.Lookup("agent.html").Execute(&out, agent))

	return out.String()
}

// newTestAgent registers an Agent that reported a minimal status, so that the
// agent page can be rendered for it.
func newTestAgent(t *testing.T, store data.ConfigStore) *data.Agent {
	t.Helper()

	agents := &data.AllAgents
	agents.SetConfigStore(store)

	conn := fakeConnection{id: 1}
	t.Cleanup(func() {
		agents.RemoveConnection(conn)
		agents.SetConfigStore(nil)
	})

	agent := agents.FindOrCreateAgent(data.InstanceId(uuid.New()), conn)
	agent.Status = &protobufs.AgentToServer{AgentDescription: &protobufs.AgentDescription{}}

	return agent
}

// Without a config store the page keeps offering the in-memory config editor.
func TestAgentPageRendersWithoutConfigStore(t *testing.T) {
	agent := newTestAgent(t, nil)
	agent.CustomInstanceConfig = "in memory config"

	page := renderAgentTemplate(t, agent)

	assert.Contains(t, page, "Additional Configuration")
	assert.Contains(t, page, "in memory config")
	assert.NotContains(t, page, `name="configkey"`)
}

// With a config store the page shows which file in the store is being edited.
func TestAgentPageRendersConfigStoreFile(t *testing.T) {
	agent := newTestAgent(t, fakeConfigStore{location: "s3://bucket/otel-configs"})
	agent.CustomInstanceConfig = "stored config"
	agent.ConfigKey = "services/billing.yaml"

	page := renderAgentTemplate(t, agent)

	assert.Contains(t, page, "s3://bucket/otel-configs")
	assert.Contains(t, page, "services/billing.yaml")
	assert.Contains(t, page, "stored config")
	assert.Contains(t, page, `name="configkey"`)
}
