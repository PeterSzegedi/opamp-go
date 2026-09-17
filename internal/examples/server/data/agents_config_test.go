package data

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-telemetry/opamp-go/internal/examples/server/configstore"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
)

// recordingConnection captures the messages the Server sends to an Agent. It is
// a pointer type so that it stays comparable and can be used as a map key.
type recordingConnection struct {
	mux      sync.Mutex
	messages []*protobufs.ServerToAgent
	sent     chan struct{}
}

func newRecordingConnection() *recordingConnection {
	return &recordingConnection{sent: make(chan struct{}, 16)}
}

func (c *recordingConnection) Connection() net.Conn { return nil }

func (c *recordingConnection) Send(_ context.Context, message *protobufs.ServerToAgent) error {
	c.mux.Lock()
	c.messages = append(c.messages, message)
	c.mux.Unlock()

	c.sent <- struct{}{}

	return nil
}

func (c *recordingConnection) Disconnect() error { return nil }

// awaitRemoteConfig waits for the next config the Server pushes to the Agent.
func (c *recordingConnection) awaitRemoteConfig(t *testing.T) string {
	t.Helper()

	select {
	case <-c.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a config to be sent to the agent")
	}

	c.mux.Lock()
	defer c.mux.Unlock()

	last := c.messages[len(c.messages)-1]
	require.NotNil(t, last.RemoteConfig)

	return string(last.RemoteConfig.Config.ConfigMap[""].Body)
}

func (c *recordingConnection) messageCount() int {
	c.mux.Lock()
	defer c.mux.Unlock()

	return len(c.messages)
}

func newTestAgents() *Agents {
	return &Agents{
		agentsById:  map[InstanceId]*Agent{},
		connections: map[types.Connection]map[InstanceId]bool{},
	}
}

func newTestStore(t *testing.T, backend configstore.Backend) *configstore.Store {
	t.Helper()

	store := configstore.New(backend, configstore.Options{
		SyncInterval: 10 * time.Millisecond,
		Logger:       log.New(io.Discard, "", 0),
	})
	require.NoError(t, store.Start(context.Background()))
	t.Cleanup(store.Stop)

	return store
}

// connectAgent registers an Agent and reports its first status, which is what
// triggers the initial config lookup in the store.
func connectAgent(t *testing.T, agents *Agents, conn *recordingConnection, serviceName string) (*Agent, *protobufs.ServerToAgent) {
	t.Helper()

	instanceId := InstanceId(uuid.New())
	agent := agents.FindOrCreateAgent(instanceId, conn)

	response := &protobufs.ServerToAgent{}
	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum:  1,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{
				{
					Key:   "service.name",
					Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: serviceName}},
				},
			},
		},
	}, response)

	return agent, response
}

func TestAgentIsConfiguredFromTheStoreOnConnect(t *testing.T) {
	ctx := context.Background()
	backend := configstore.NewMemoryBackend()
	_, err := backend.Put(ctx, configstore.DefaultKey, []byte("default config"))
	require.NoError(t, err)
	_, err = backend.Put(ctx, "services/billing.yaml", []byte("billing config"))
	require.NoError(t, err)

	agents := newTestAgents()
	agents.SetConfigStore(newTestStore(t, backend))

	billing, response := connectAgent(t, agents, newRecordingConnection(), "billing")
	require.NotNil(t, response.RemoteConfig)
	assert.Equal(t, "billing config", string(response.RemoteConfig.Config.ConfigMap[""].Body))
	assert.Equal(t, "services/billing.yaml", billing.CloneReadonly().ConfigKey)

	// An agent of another service gets the default config.
	shipping, response := connectAgent(t, agents, newRecordingConnection(), "shipping")
	require.NotNil(t, response.RemoteConfig)
	assert.Equal(t, "default config", string(response.RemoteConfig.Config.ConfigMap[""].Body))
	assert.Equal(t, configstore.DefaultKey, shipping.CloneReadonly().ConfigKey)
}

func TestAgentWithoutConfigInTheStoreGetsAKeyToCreate(t *testing.T) {
	agents := newTestAgents()
	agents.SetConfigStore(newTestStore(t, configstore.NewMemoryBackend()))

	agent, response := connectAgent(t, agents, newRecordingConnection(), "billing")

	clone := agent.CloneReadonly()
	assert.Empty(t, clone.CustomInstanceConfig)
	assert.Equal(t, "instances/"+clone.InstanceIdStr+".yaml", clone.ConfigKey,
		"the UI must know where a config for this agent would belong")
	assert.NotNil(t, response.RemoteConfig, "the agent is still offered the (empty) config it resolves to")
}

func TestSaveConfigForAgentStoresBeforeSending(t *testing.T) {
	ctx := context.Background()
	backend := configstore.NewMemoryBackend()
	_, err := backend.Put(ctx, configstore.DefaultKey, []byte("v1"))
	require.NoError(t, err)

	agents := newTestAgents()
	agents.SetConfigStore(newTestStore(t, backend))

	conn := newRecordingConnection()
	agent, _ := connectAgent(t, agents, conn, "billing")

	notify := make(chan struct{}, 1)
	require.NoError(t, agents.SaveConfigForAgent(ctx, agent.InstanceId, configstore.DefaultKey, []byte("v2"), notify))

	// The config must be in the store...
	stored, err := backend.Get(ctx, configstore.DefaultKey)
	require.NoError(t, err)
	assert.Equal(t, "v2", string(stored.Body))

	// ...and the agent must have been given the stored config.
	assert.Equal(t, "v2", conn.awaitRemoteConfig(t))
}

func TestSaveConfigForAgentAppliesToEveryAgentThatResolvesToTheKey(t *testing.T) {
	ctx := context.Background()
	backend := configstore.NewMemoryBackend()
	_, err := backend.Put(ctx, configstore.DefaultKey, []byte("v1"))
	require.NoError(t, err)

	agents := newTestAgents()
	agents.SetConfigStore(newTestStore(t, backend))

	firstConn, secondConn := newRecordingConnection(), newRecordingConnection()
	agent, _ := connectAgent(t, agents, firstConn, "billing")
	connectAgent(t, agents, secondConn, "shipping")

	require.NoError(t, agents.SaveConfigForAgent(ctx, agent.InstanceId, configstore.DefaultKey, []byte("v2"), nil))

	assert.Equal(t, "v2", firstConn.awaitRemoteConfig(t))
	assert.Equal(t, "v2", secondConn.awaitRemoteConfig(t),
		"a change to a shared config file must reach every agent using it")
}

func TestSaveConfigForAgentDoesNotSendWhenTheStoreRejectsTheWrite(t *testing.T) {
	agents := newTestAgents()
	agents.SetConfigStore(newTestStore(t, configstore.NewMemoryBackend()))

	conn := newRecordingConnection()
	agent, _ := connectAgent(t, agents, conn, "billing")

	err := agents.SaveConfigForAgent(context.Background(), agent.InstanceId, "../escape.yaml", []byte("v2"), nil)
	require.Error(t, err)

	assert.Zero(t, conn.messageCount(), "nothing may be sent to the agent if the config was not stored")
	assert.Empty(t, agent.CloneReadonly().CustomInstanceConfig)
}

func TestOutOfBandChangeInTheStoreReachesTheAgent(t *testing.T) {
	ctx := context.Background()
	backend := configstore.NewMemoryBackend()
	_, err := backend.Put(ctx, configstore.DefaultKey, []byte("v1"))
	require.NoError(t, err)

	agents := newTestAgents()
	store := newTestStore(t, backend)
	agents.SetConfigStore(store)
	store.OnChange(agents.ReloadConfigsFromStore)

	conn := newRecordingConnection()
	connectAgent(t, agents, conn, "billing")

	// Somebody uploads a new config straight to the bucket.
	_, err = backend.Put(ctx, configstore.DefaultKey, []byte("v2"))
	require.NoError(t, err)

	assert.Equal(t, "v2", conn.awaitRemoteConfig(t))
}

func TestConfigRemovedFromTheStoreIsNotWipedOnTheAgent(t *testing.T) {
	ctx := context.Background()
	backend := configstore.NewMemoryBackend()
	_, err := backend.Put(ctx, configstore.DefaultKey, []byte("v1"))
	require.NoError(t, err)

	agents := newTestAgents()
	store := newTestStore(t, backend)
	agents.SetConfigStore(store)

	conn := newRecordingConnection()
	agent, _ := connectAgent(t, agents, conn, "billing")

	backend.Delete(configstore.DefaultKey)
	_, err = store.Sync(ctx)
	require.NoError(t, err)

	agents.ReloadConfigsFromStore()

	clone := agent.CloneReadonly()
	assert.Equal(t, "v1", clone.CustomInstanceConfig,
		"a deleted config file must not wipe the config the agent is running")
	assert.Zero(t, conn.messageCount(), "no config change may be pushed for a deleted config file")
}

func TestAgentsWithoutConfigStoreKeepConfigsInMemory(t *testing.T) {
	agents := newTestAgents()

	conn := newRecordingConnection()
	agent, _ := connectAgent(t, agents, conn, "billing")

	notify := make(chan struct{}, 1)
	require.NoError(t, agents.SaveConfigForAgent(context.Background(), agent.InstanceId, "", []byte("in memory"), notify))

	assert.Equal(t, "in memory", conn.awaitRemoteConfig(t))
	assert.Equal(t, "in memory", agent.CloneReadonly().CustomInstanceConfig)
}
