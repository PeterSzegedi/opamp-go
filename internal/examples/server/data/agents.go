package data

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/google/uuid"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/protobufshelpers"
	"github.com/open-telemetry/opamp-go/server/types"
)

type Agents struct {
	mux         sync.RWMutex
	agentsById  map[InstanceId]*Agent
	connections map[types.Connection]map[InstanceId]bool

	// Source of truth for the Agents' configs. May be nil, in which case configs
	// are only kept in memory and are lost when the Server restarts.
	configStore ConfigStore
}

var logger = log.New(log.Default().Writer(), "[AGENTS] ", log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds)

// RemoveConnection removes the connection all Agent instances associated with the
// connection.
func (agents *Agents) RemoveConnection(conn types.Connection) {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	for instanceId := range agents.connections[conn] {
		delete(agents.agentsById, instanceId)
	}
	delete(agents.connections, conn)
}

func (agents *Agents) SendCustomMessageToAgent(
	agentId InstanceId,
	customMsg *protobufs.ServerToAgent,
) {
	agent := agents.FindAgent(agentId)
	if agent != nil {
		agent.SendCustomMessage(customMsg)
	}
}

func (agents *Agents) SetCustomConfigForAgent(
	agentId InstanceId,
	config *protobufs.AgentConfigMap,
	notifyNextStatusUpdate chan<- struct{},
) {
	agent := agents.FindAgent(agentId)
	if agent != nil {
		agent.SetCustomConfig(config, notifyNextStatusUpdate)
	}
}

// SetConfigStore makes store the source of truth for the Agents' configs. It
// must be called before the OpAMP Server starts accepting connections.
func (agents *Agents) SetConfigStore(store ConfigStore) {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	agents.configStore = store
	for _, agent := range agents.agentsById {
		agent.configStore = store
	}
}

// ConfigStore returns the configured source of truth for configs, or nil.
func (agents *Agents) ConfigStore() ConfigStore {
	agents.mux.RLock()
	defer agents.mux.RUnlock()

	return agents.configStore
}

// SaveConfigForAgent writes a config to the config store and then hands the
// stored config to every Agent it applies to, which is normally more than the
// Agent the change was made for: a config file applies to every instance of a
// component in a stack, and a config stored under a shared key (a component wide
// file or the fallback config) applies to all Agents that resolve to that key.
//
// The config is written to the store first and is only offered to the Agents
// once the store accepted it, so that the Agents can never run a config that is
// not in the store. If the store rejects the write, nothing is sent to any
// Agent and the error is returned.
//
// notifyNextStatusUpdate is notified once the Agent identified by agentId
// reported back after the change. It must have a buffer size of at least 1.
func (agents *Agents) SaveConfigForAgent(
	ctx context.Context,
	agentId InstanceId,
	configKey string,
	body []byte,
	notifyNextStatusUpdate chan<- struct{},
) error {
	store := agents.ConfigStore()
	if store == nil {
		// No external store configured, keep the config in memory only.
		agents.SetCustomConfigForAgent(
			agentId,
			&protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigObject{
					"": {Body: body},
				},
			},
			notifyNextStatusUpdate,
		)
		return nil
	}

	if configKey == "" {
		// The caller did not say where to store the config, use the file this
		// Agent's config comes from (or would be created at).
		agent := agents.FindAgent(agentId)
		if agent == nil {
			return fmt.Errorf("agent %s is not connected, cannot tell where to store its config",
				uuid.UUID(agentId))
		}
		configKey = agent.storeConfigKey()
	}

	if err := store.Save(ctx, configKey, body); err != nil {
		return err
	}

	agents.refreshConfigsFromStore(&agentId, notifyNextStatusUpdate)

	return nil
}

// ReloadConfigsFromStore re-reads the config of every Agent from the config
// store and pushes the ones that changed. It is called after the store picked
// up a change that was made outside of this Server, for example by a pipeline
// that uploaded a new config file to the bucket.
func (agents *Agents) ReloadConfigsFromStore() {
	agents.refreshConfigsFromStore(nil, nil)
}

func (agents *Agents) refreshConfigsFromStore(
	notifyAgentId *InstanceId,
	notifyNextStatusUpdate chan<- struct{},
) {
	// Take a snapshot of the Agents so that we do not hold the Agents lock while
	// sending configs out.
	agents.mux.RLock()
	snapshot := make([]*Agent, 0, len(agents.agentsById))
	for _, agent := range agents.agentsById {
		snapshot = append(snapshot, agent)
	}
	agents.mux.RUnlock()

	notified := false
	for _, agent := range snapshot {
		var notify chan<- struct{}
		if notifyAgentId != nil && agent.InstanceId == *notifyAgentId {
			notify = notifyNextStatusUpdate
			notified = true
		}

		agent.RefreshConfigFromStore(notify)
	}

	if notifyNextStatusUpdate != nil && !notified {
		// The Agent disconnected while the config was being stored. The config is
		// safely in the store and will be offered when the Agent comes back, so
		// release the waiter instead of making it wait for a timeout.
		select {
		case notifyNextStatusUpdate <- struct{}{}:
		default:
		}
	}
}

func isEqualAgentDescr(d1, d2 *protobufs.AgentDescription) bool {
	if d1 == d2 {
		return true
	}
	if d1 == nil || d2 == nil {
		return false
	}
	return isEqualAttrs(d1.IdentifyingAttributes, d2.IdentifyingAttributes) &&
		isEqualAttrs(d1.NonIdentifyingAttributes, d2.NonIdentifyingAttributes)
}

func isEqualAttrs(attrs1, attrs2 []*protobufs.KeyValue) bool {
	if len(attrs1) != len(attrs2) {
		return false
	}
	for i, a1 := range attrs1 {
		a2 := attrs2[i]
		if !protobufshelpers.IsEqualKeyValue(a1, a2) {
			return false
		}
	}
	return true
}

func (agents *Agents) FindAgent(agentId InstanceId) *Agent {
	agents.mux.RLock()
	defer agents.mux.RUnlock()
	return agents.agentsById[agentId]
}

func (agents *Agents) FindOrCreateAgent(agentId InstanceId, conn types.Connection) *Agent {
	agents.mux.Lock()
	defer agents.mux.Unlock()

	// Ensure the Agent is in the agentsById map.
	agent := agents.agentsById[agentId]
	if agent == nil {
		agent = NewAgent(agentId, conn)
		agent.configStore = agents.configStore
		agents.agentsById[agentId] = agent

		// Ensure the Agent's instance id is associated with the connection.
		if agents.connections[conn] == nil {
			agents.connections[conn] = map[InstanceId]bool{}
		}
		agents.connections[conn][agentId] = true
	}

	return agent
}

func (agents *Agents) GetAgentReadonlyClone(agentId InstanceId) *Agent {
	agent := agents.FindAgent(agentId)
	if agent == nil {
		return nil
	}

	// Return a clone to allow safe access after returning.
	return agent.CloneReadonly()
}

func (a *Agents) OfferAgentConnectionSettings(
	id InstanceId,
	offers *protobufs.ConnectionSettingsOffers,
) {
	if len(offers.Hash) == 0 {
		offers.Hash = toHash(offers)
	}

	a.mux.Lock()
	defer a.mux.Unlock()

	agent, ok := a.agentsById[id]
	if ok {
		agent.OfferConnectionSettings(offers)
		logger.Printf("Client connection settings offers sent to %s (hash=%x)\n", id, offers.Hash)
	} else {
		logger.Printf("Agent %s not found\n", id)
	}
}

var AllAgents = Agents{
	agentsById:  map[InstanceId]*Agent{},
	connections: map[types.Connection]map[InstanceId]bool{},
}

// toHash computes a sha256 hash from fields within ConnectionSettingsOffers
func toHash(c *protobufs.ConnectionSettingsOffers) []byte {
	hasher := sha256.New()
	if c.Opamp != nil {
		hashEndpoint(hasher, c.Opamp)
	}
	if c.OwnMetrics != nil {
		hashEndpoint(hasher, c.OwnMetrics)
	}
	if c.OwnTraces != nil {
		hashEndpoint(hasher, c.OwnTraces)
	}
	if c.OwnLogs != nil {
		hashEndpoint(hasher, c.OwnLogs)
	}
	for name, endpoint := range c.OtherConnections {
		hasher.Write([]byte(name))
		hashEndpoint(hasher, endpoint)
	}

	return hasher.Sum(nil)
}

// endpoint is an incomplete interface to assist with turning an offered connection into a hash.
type endpoint interface {
	GetDestinationEndpoint() string
	GetHeaders() *protobufs.Headers
	GetCertificate() *protobufs.TLSCertificate
	GetTls() *protobufs.TLSConnectionSettings
}

// hashEndpoint writes some shared attributes of the passed enpoint to the passed writer.
func hashEndpoint(w io.Writer, e endpoint) {
	_, err := w.Write([]byte(e.GetDestinationEndpoint()))
	if err != nil {
		panic(err)
	}
	if headers := e.GetHeaders(); headers != nil {
		for _, header := range headers.Headers {
			_, err := w.Write([]byte(header.Key + header.Value))
			if err != nil {
				panic(err)
			}
		}
	}
	if cert := e.GetCertificate(); cert != nil {
		_, err := w.Write(cert.Cert)
		if err != nil {
			panic(err)
		}
		_, err = w.Write(cert.PrivateKey)
		if err != nil {
			panic(err)
		}
		_, err = w.Write(cert.CaCert)
		if err != nil {
			panic(err)
		}
	}
	if tlsSettings := e.GetTls(); tlsSettings != nil {
		_, err := w.Write([]byte(tlsSettings.CaPemContents))
		if err != nil {
			panic(err)
		}
	}
}
