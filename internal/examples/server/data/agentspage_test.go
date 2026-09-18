package data

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// registerAgents adds count Agents that have all reported a status.
func registerAgents(t *testing.T, agents *Agents, count int) {
	t.Helper()

	conn := newRecordingConnection()
	for i := range count {
		agent := agents.FindOrCreateAgent(InstanceId(uuid.New()), conn)
		agent.UpdateStatus(&protobufs.AgentToServer{
			SequenceNum: 1,
			AgentDescription: &protobufs.AgentDescription{
				IdentifyingAttributes: []*protobufs.KeyValue{
					{
						Key: "service.name",
						Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{
								StringValue: fmt.Sprintf("agent-%d", i),
							},
						},
					},
				},
			},
		}, &protobufs.ServerToAgent{})
	}
}

func TestGetAgentsPageSplitsTheFleet(t *testing.T) {
	agents := newTestAgents()
	registerAgents(t, agents, 125)

	first := agents.GetAgentsPage(1, 50)
	assert.Len(t, first.Agents, 50)
	assert.Equal(t, 125, first.TotalAgents)
	assert.Equal(t, 3, first.TotalPages)
	assert.Equal(t, 1, first.FirstIndex)
	assert.Equal(t, 50, first.LastIndex)
	assert.False(t, first.HasPrev)
	assert.True(t, first.HasNext)

	last := agents.GetAgentsPage(3, 50)
	assert.Len(t, last.Agents, 25)
	assert.Equal(t, 101, last.FirstIndex)
	assert.Equal(t, 125, last.LastIndex)
	assert.True(t, last.HasPrev)
	assert.False(t, last.HasNext)
}

// Paging is only useful if a given Agent shows up on exactly one page.
func TestGetAgentsPageIsStableAndComplete(t *testing.T) {
	agents := newTestAgents()
	registerAgents(t, agents, 75)

	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		for _, agent := range agents.GetAgentsPage(page, 25).Agents {
			require.False(t, seen[agent.InstanceIdStr], "agent listed on more than one page")
			seen[agent.InstanceIdStr] = true
		}
	}
	assert.Len(t, seen, 75, "every agent must appear on some page")

	// The same request twice returns the same page.
	assert.Equal(t,
		agents.GetAgentsPage(2, 25).Agents[0].InstanceIdStr,
		agents.GetAgentsPage(2, 25).Agents[0].InstanceIdStr,
	)
}

func TestGetAgentsPageClampsItsInput(t *testing.T) {
	agents := newTestAgents()
	registerAgents(t, agents, 10)

	// A page past the end returns the last page, so a bookmarked URL keeps
	// working as the fleet shrinks.
	beyond := agents.GetAgentsPage(99, 5)
	assert.Equal(t, 2, beyond.Page)
	assert.Len(t, beyond.Agents, 5)

	// Nonsense input falls back to the defaults.
	assert.Equal(t, DefaultPageSize, agents.GetAgentsPage(0, 0).PageSize)
	assert.Equal(t, 1, agents.GetAgentsPage(-5, 0).Page)

	// A hand written URL cannot ask for the whole fleet.
	assert.Equal(t, MaxPageSize, agents.GetAgentsPage(1, 100000).PageSize)
}

func TestGetAgentsPageWithNoAgents(t *testing.T) {
	page := newTestAgents().GetAgentsPage(1, 50)

	assert.Empty(t, page.Agents)
	assert.Zero(t, page.TotalAgents)
	assert.Equal(t, 1, page.TotalPages, "an empty list still has one page to render")
	assert.Zero(t, page.FirstIndex)
	assert.False(t, page.HasPrev)
	assert.False(t, page.HasNext)
}

// Only the Agents on the page may be cloned: cloning the whole fleet to render
// one page is what the paging exists to avoid.
func TestGetAgentsPageClonesOnlyThePage(t *testing.T) {
	agents := newTestAgents()
	registerAgents(t, agents, 40)

	page := agents.GetAgentsPage(1, 10)
	require.Len(t, page.Agents, 10)

	live := agents.FindAgent(page.Agents[0].InstanceId)
	require.NotNil(t, live)
	assert.NotSame(t, live, page.Agents[0], "the page must hand out clones, not live agents")
}
