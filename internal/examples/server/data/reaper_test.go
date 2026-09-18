package data

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// disconnectableConnection records whether the reaper closed it.
type disconnectableConnection struct {
	*recordingConnection
	disconnected chan struct{}
}

func newDisconnectableConnection() *disconnectableConnection {
	return &disconnectableConnection{
		recordingConnection: newRecordingConnection(),
		disconnected:        make(chan struct{}, 1),
	}
}

func (c *disconnectableConnection) Disconnect() error {
	select {
	case c.disconnected <- struct{}{}:
	default:
	}

	return nil
}

func (c *disconnectableConnection) wasDisconnected() bool {
	select {
	case <-c.disconnected:
		return true
	default:
		return false
	}
}

func newTestReaper(agents *Agents, reapAfter time.Duration, now func() time.Time) *Reaper {
	return NewReaper(agents, ReaperOptions{
		ReapAfter: reapAfter,
		Logger:    log.New(io.Discard, "", 0),
		now:       now,
	})
}

// reportHealth makes the Agent report a health status, as an Agent with the
// ReportsHealth capability would.
func reportHealth(agent *Agent, healthy bool) {
	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum:  2,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth),
		Health:       &protobufs.ComponentHealth{Healthy: healthy},
	}, &protobufs.ServerToAgent{})
}

func TestReaperKeepsAgentsThatAreStillReporting(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	// Only a minute has passed since the Agent reported.
	now := agent.CloneReadonly().LastSeenAt.Add(time.Minute)
	reaped := newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap()

	assert.Zero(t, reaped)
	assert.NotNil(t, agents.FindAgent(agent.InstanceId))
	assert.False(t, conn.wasDisconnected())
}

func TestReaperRemovesAgentsThatWentSilent(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	// The Agent has not been heard from for longer than the threshold, which is
	// what an abruptly terminated machine looks like to the Server.
	now := agent.CloneReadonly().LastSeenAt.Add(25 * time.Hour)
	reaped := newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap()

	assert.Equal(t, 1, reaped)
	assert.Nil(t, agents.FindAgent(agent.InstanceId), "the agent must be forgotten")
	assert.True(t, conn.wasDisconnected(), "the dead connection must be closed, not just forgotten")
}

func TestReaperRemovesAgentsThatStayUnhealthy(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	reportHealth(agent, false)
	unhealthySince := agent.CloneReadonly().unhealthySince
	require.False(t, unhealthySince.IsZero())

	// Still reporting, but unhealthy the whole time.
	now := unhealthySince.Add(25 * time.Hour)
	agent.LastSeenAt = now

	reaped := newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap()

	assert.Equal(t, 1, reaped)
	assert.Nil(t, agents.FindAgent(agent.InstanceId))
}

func TestReaperForgetsUnhealthinessAfterRecovery(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	reportHealth(agent, false)
	require.False(t, agent.CloneReadonly().unhealthySince.IsZero())

	reportHealth(agent, true)
	assert.True(t, agent.CloneReadonly().unhealthySince.IsZero(),
		"recovering must reset the unhealthy timer")

	now := agent.CloneReadonly().LastSeenAt.Add(time.Hour)
	assert.Zero(t, newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap())
}

// An Agent without the ReportsHealth capability shows up as "Unknown" health.
// That must not be treated as unhealthy, or every such Agent would be reaped.
func TestReaperKeepsAgentsThatReportNoHealth(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	require.Equal(t, "Unknown", agent.HealthStatus())

	now := agent.CloneReadonly().LastSeenAt.Add(time.Hour)
	assert.Zero(t, newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap())
	assert.NotNil(t, agents.FindAgent(agent.InstanceId))
}

// A connection that still carries another Agent must stay open.
func TestReaperKeepsConnectionsThatStillHaveAgents(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()

	stale, _ := connectAgent(t, agents, conn, "billing", "prod")
	fresh := agents.FindOrCreateAgent(InstanceId(uuid.New()), conn)
	fresh.UpdateStatus(&protobufs.AgentToServer{SequenceNum: 1}, &protobufs.ServerToAgent{})

	// Age only the first Agent past the threshold.
	staleSeenAt := stale.CloneReadonly().LastSeenAt
	now := staleSeenAt.Add(25 * time.Hour)
	fresh.LastSeenAt = now

	reaped := newTestReaper(agents, 24*time.Hour, func() time.Time { return now }).Reap()

	assert.Equal(t, 1, reaped)
	assert.Nil(t, agents.FindAgent(stale.InstanceId))
	assert.NotNil(t, agents.FindAgent(fresh.InstanceId))
	assert.False(t, conn.wasDisconnected(), "the connection still has a live agent")
}

// An Agent that connected but never reported is aged from when it connected, so
// that a zero LastSeenAt does not make it look infinitely stale.
func TestReaperAgesSilentAgentsFromConnectTime(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent := agents.FindOrCreateAgent(InstanceId(uuid.New()), conn)

	require.True(t, agent.CloneReadonly().LastSeenAt.IsZero())

	justConnected := agent.CloneReadonly().connectedAt.Add(time.Minute)
	assert.Zero(t, newTestReaper(agents, 24*time.Hour, func() time.Time { return justConnected }).Reap())

	longGone := agent.CloneReadonly().connectedAt.Add(25 * time.Hour)
	assert.Equal(t, 1, newTestReaper(agents, 24*time.Hour, func() time.Time { return longGone }).Reap())
}

func TestReaperDisabled(t *testing.T) {
	agents := newTestAgents()
	conn := newDisconnectableConnection()
	agent, _ := connectAgent(t, agents, conn, "billing", "prod")

	now := agent.CloneReadonly().LastSeenAt.Add(1000 * time.Hour)
	reaper := newTestReaper(agents, -1, func() time.Time { return now })

	assert.False(t, reaper.Enabled())
	assert.Zero(t, reaper.Reap())
	assert.NotNil(t, agents.FindAgent(agent.InstanceId))
}

func TestReaperDefaultsAndDerivedScanInterval(t *testing.T) {
	reaper := NewReaper(newTestAgents(), ReaperOptions{Logger: log.New(io.Discard, "", 0)})

	assert.Equal(t, DefaultReapAfter, reaper.ReapAfter())
	assert.True(t, reaper.Enabled())

	// 24h/12 = 2h, clamped to the maximum.
	assert.Equal(t, maxReapScanInterval, reaper.ScanInterval())

	// A short threshold scans more often, but never below the minimum.
	short := NewReaper(newTestAgents(), ReaperOptions{
		ReapAfter: 10 * time.Second,
		Logger:    log.New(io.Discard, "", 0),
	})
	assert.Equal(t, minReapScanInterval, short.ScanInterval())

	medium := NewReaper(newTestAgents(), ReaperOptions{
		ReapAfter: 6 * time.Hour,
		Logger:    log.New(io.Discard, "", 0),
	})
	assert.Equal(t, 30*time.Minute, medium.ScanInterval())
}

func TestReaperStartStopIsSafeWhenDisabled(t *testing.T) {
	reaper := newTestReaper(newTestAgents(), -time.Second, time.Now)
	reaper.Start(context.Background())
	reaper.Stop()
	reaper.Stop() // idempotent
}

// Stopping a Reaper that was never started must not block.
func TestReaperStopWithoutStart(t *testing.T) {
	newTestReaper(newTestAgents(), 24*time.Hour, time.Now).Stop()
}

func TestReaperStartStopWhenEnabled(t *testing.T) {
	reaper := newTestReaper(newTestAgents(), 24*time.Hour, time.Now)
	reaper.Start(context.Background())
	reaper.Stop()
	reaper.Stop() // idempotent
}
