package data

import (
	"bytes"
	"slices"
)

// Paging defaults for the agent list. The list is paged because cloning every
// Agent to render one page does not survive a large fleet: each clone carries a
// deep copy of the Agent's last reported status, including its effective
// config, so a page that clones all of them allocates in proportion to the
// whole fleet on every single page load.
const (
	// DefaultPageSize is how many Agents are listed when the request does not
	// ask for a specific size.
	DefaultPageSize = 50
	// MaxPageSize caps what a request may ask for, so that a hand written URL
	// cannot bring back the unbounded behaviour.
	MaxPageSize = 500
)

// AgentsPage is one page of the agent list, together with everything the UI
// needs to render the paging controls.
type AgentsPage struct {
	// Agents are readonly clones of the Agents on this page, ordered by
	// instance id. Only the Agents on the page are cloned.
	Agents []*Agent

	// Page is the 1 based number of this page.
	Page int
	// PageSize is how many Agents fit on a page.
	PageSize int
	// TotalAgents is how many Agents the Server currently tracks.
	TotalAgents int
	// TotalPages is at least 1, even when there are no Agents at all.
	TotalPages int

	// FirstIndex and LastIndex are the 1 based positions of the first and the
	// last Agent on this page. Both are 0 when the page is empty.
	FirstIndex int
	LastIndex  int

	HasPrev  bool
	HasNext  bool
	PrevPage int
	NextPage int
}

// GetAgentsPage returns one page of the agent list. page is 1 based; a page
// beyond the end returns the last page, so that a bookmarked URL keeps working
// as the fleet shrinks.
//
// Agents are ordered by instance id. That is an arbitrary order, but it is
// stable and can be established without touching any Agent's lock, which
// matters when there are tens of thousands of them.
func (agents *Agents) GetAgentsPage(page, pageSize int) *AgentsPage {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	if page <= 0 {
		page = 1
	}

	// Collect the pointers under the lock, but clone only the page below.
	agents.mux.RLock()
	all := make([]*Agent, 0, len(agents.agentsById))
	for _, agent := range agents.agentsById {
		all = append(all, agent)
	}
	agents.mux.RUnlock()

	slices.SortFunc(all, func(a, b *Agent) int {
		return bytes.Compare(a.InstanceId[:], b.InstanceId[:])
	})

	total := len(all)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	end := min(start+pageSize, total)

	result := &AgentsPage{
		Page:        page,
		PageSize:    pageSize,
		TotalAgents: total,
		TotalPages:  totalPages,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
		PrevPage:    page - 1,
		NextPage:    page + 1,
	}

	if start >= end {
		return result
	}

	result.FirstIndex = start + 1
	result.LastIndex = end
	result.Agents = make([]*Agent, 0, end-start)
	for _, agent := range all[start:end] {
		result.Agents = append(result.Agents, agent.CloneReadonly())
	}

	return result
}
