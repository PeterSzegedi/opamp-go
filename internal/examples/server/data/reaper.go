package data

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/open-telemetry/opamp-go/server/types"
)

// DefaultReapAfter is how long an Agent may be unavailable or unhealthy before
// the reaper forgets it.
const DefaultReapAfter = 24 * time.Hour

// Bounds of the derived scan interval. Scanning is cheap (it walks the Agents
// without cloning them), so it is done far more often than ReapAfter in order
// to keep the overshoot small, but never so often that it is pointless.
const (
	minReapScanInterval = time.Minute
	maxReapScanInterval = time.Hour
)

// maxReapLogSamples caps how many reaped Agents are named in the log line, so
// that reaping a large fleet does not turn into a log flood.
const maxReapLogSamples = 5

// Reaper periodically removes Agents that the Server has no reason to keep
// tracking: ones that have gone silent, and ones that have been reporting
// themselves unhealthy for a long time.
//
// It matters for a Server that stays up for a long time in front of a fleet
// whose machines are replaced often. When a machine disappears without closing
// its OpAMP connection, which is the normal case for an abruptly terminated
// cloud instance or a severed network path, the Server does not find out: the
// WebSocket read loop only ends once the OS TCP stack gives up, which can take
// hours. Until then the Agent stays in the map, holds on to its config and its
// last reported status, and is still walked on every config change, where a
// write to its dead socket can stall the rollout for the Agents behind it.
//
// Reaping an Agent also closes its connection, which is what actually releases
// the socket and the goroutine reading from it. An Agent that turns out to be
// alive after all simply registers again on its next message, so reaping is
// safe to do slightly too eagerly.
type Reaper struct {
	agents       *Agents
	reapAfter    time.Duration
	scanInterval time.Duration
	logger       *log.Logger
	now          func() time.Time

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}
}

// ReaperOptions configures a Reaper.
type ReaperOptions struct {
	// ReapAfter is how long an Agent may be unavailable or unhealthy before it
	// is removed. Zero or negative disables reaping. Defaults to
	// DefaultReapAfter.
	ReapAfter time.Duration
	// ScanInterval is how often the Agents are checked. When zero it is derived
	// from ReapAfter.
	ScanInterval time.Duration
	// Logger receives one line per reap pass that removed something. Defaults to
	// the standard logger.
	Logger *log.Logger

	// now is overridden in tests.
	now func() time.Time
}

// NewReaper creates a Reaper for agents. Call Start to begin reaping.
func NewReaper(agents *Agents, opts ReaperOptions) *Reaper {
	if opts.ReapAfter == 0 {
		opts.ReapAfter = DefaultReapAfter
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = scanIntervalFor(opts.ReapAfter)
	}
	if opts.Logger == nil {
		opts.Logger = log.New(
			log.Default().Writer(),
			"[REAPER] ",
			log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds,
		)
	}
	if opts.now == nil {
		opts.now = func() time.Time { return time.Now().UTC() }
	}

	return &Reaper{
		agents:       agents,
		reapAfter:    opts.ReapAfter,
		scanInterval: opts.ScanInterval,
		logger:       opts.Logger,
		now:          opts.now,
		done:         make(chan struct{}),
	}
}

// Enabled reports whether the Reaper does anything.
func (r *Reaper) Enabled() bool {
	return r.reapAfter > 0
}

// ReapAfter is how long an Agent may be unavailable or unhealthy before it is
// removed.
func (r *Reaper) ReapAfter() time.Duration {
	return r.reapAfter
}

// ScanInterval is how often the Agents are checked.
func (r *Reaper) ScanInterval() time.Duration {
	return r.scanInterval
}

// Start begins reaping in the background until Stop is called or ctx is
// cancelled. It does nothing when reaping is disabled.
func (r *Reaper) Start(ctx context.Context) {
	if !r.Enabled() {
		close(r.done)
		return
	}

	loopCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	go r.loop(loopCtx)
}

// Stop ends the background reaping and waits for it to finish. It is safe to
// call on a Reaper that was never started.
func (r *Reaper) Stop() {
	r.stopOnce.Do(func() {
		if r.cancel == nil {
			// Either never started, or started while disabled. Nothing is
			// running, so there is nothing to wait for.
			return
		}

		r.cancel()
		<-r.done
	})
}

func (r *Reaper) loop(ctx context.Context) {
	defer close(r.done)

	ticker := time.NewTicker(r.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Reap()
		}
	}
}

// Reap runs a single pass and returns how many Agents were removed. It is
// exported so that it can be triggered directly, mostly from tests.
func (r *Reaper) Reap() int {
	if !r.Enabled() {
		return 0
	}

	reaped, remaining := r.agents.ReapAgents(r.now(), r.reapAfter)
	if len(reaped) == 0 {
		return 0
	}

	r.logger.Printf("Reaped %d agent(s) unavailable or unhealthy for more than %s (%d remaining)%s",
		len(reaped), r.reapAfter, remaining, formatReapSamples(reaped))

	return len(reaped)
}

// ReapedAgent describes an Agent that the reaper removed.
type ReapedAgent struct {
	InstanceIdStr string
	// Reason is why the Agent was reaped, for the log.
	Reason string
}

func formatReapSamples(reaped []ReapedAgent) string {
	if len(reaped) == 0 {
		return ""
	}

	samples := reaped
	suffix := ""
	if len(samples) > maxReapLogSamples {
		samples = samples[:maxReapLogSamples]
		suffix = ", ..."
	}

	parts := make([]string, 0, len(samples))
	for _, agent := range samples {
		parts = append(parts, fmt.Sprintf("%s (%s)", agent.InstanceIdStr, agent.Reason))
	}

	return ": " + strings.Join(parts, ", ") + suffix
}

// scanIntervalFor derives how often to scan from how long an Agent may be
// stale, so that ReapAfter stays the only setting that has to be configured.
func scanIntervalFor(reapAfter time.Duration) time.Duration {
	interval := reapAfter / 12
	if interval < minReapScanInterval {
		interval = minReapScanInterval
	}
	if interval > maxReapScanInterval {
		interval = maxReapScanInterval
	}

	return interval
}

// reapReason reports why the Agent should be reaped at time now, or "" when it
// should be kept.
func (agent *Agent) reapReason(now time.Time, reapAfter time.Duration) string {
	agent.mux.RLock()
	defer agent.mux.RUnlock()

	// An Agent that connected but never reported is aged from when it connected,
	// so that it cannot look infinitely stale.
	lastSeen := agent.LastSeenAt
	if lastSeen.Before(agent.connectedAt) {
		lastSeen = agent.connectedAt
	}

	if !lastSeen.IsZero() && now.Sub(lastSeen) > reapAfter {
		return fmt.Sprintf("not seen for %s", now.Sub(lastSeen).Truncate(time.Second))
	}

	if !agent.unhealthySince.IsZero() && now.Sub(agent.unhealthySince) > reapAfter {
		return fmt.Sprintf("unhealthy for %s", now.Sub(agent.unhealthySince).Truncate(time.Second))
	}

	return ""
}

// ReapAgents removes every Agent that has been unavailable or unhealthy for
// longer than reapAfter, and disconnects the connections left without any
// Agent. It returns what was reaped and how many Agents are left.
//
// The Agents are evaluated without holding the Agents lock, so that a large
// fleet is not blocked from connecting and disconnecting while the scan runs.
// An Agent that reports in between being picked and being removed is reaped
// anyway and registers again on its next message.
func (agents *Agents) ReapAgents(now time.Time, reapAfter time.Duration) (reaped []ReapedAgent, remaining int) {
	if reapAfter <= 0 {
		return nil, agents.count()
	}

	// Take the pointers only. Cloning here is what would make reaping as
	// expensive as rendering the whole agent list.
	agents.mux.RLock()
	candidates := make([]*Agent, 0, len(agents.agentsById))
	for _, agent := range agents.agentsById {
		candidates = append(candidates, agent)
	}
	agents.mux.RUnlock()

	type doomed struct {
		agent  *Agent
		reason string
	}

	var toReap []doomed
	for _, agent := range candidates {
		if reason := agent.reapReason(now, reapAfter); reason != "" {
			toReap = append(toReap, doomed{agent: agent, reason: reason})
		}
	}

	if len(toReap) == 0 {
		return nil, agents.count()
	}

	// Connections that lost their last Agent, to be closed once the lock is
	// released: Disconnect can block, and it makes the Agent's read loop return,
	// which calls back into RemoveConnection.
	var orphaned []types.Connection

	agents.mux.Lock()
	for _, d := range toReap {
		// The Agent may have been replaced by a reconnect since it was picked.
		if current, ok := agents.agentsById[d.agent.InstanceId]; !ok || current != d.agent {
			continue
		}

		delete(agents.agentsById, d.agent.InstanceId)
		reaped = append(reaped, ReapedAgent{
			InstanceIdStr: d.agent.InstanceIdStr,
			Reason:        d.reason,
		})

		conn := d.agent.conn
		if conn == nil {
			continue
		}
		if onConn, ok := agents.connections[conn]; ok {
			delete(onConn, d.agent.InstanceId)
			if len(onConn) == 0 {
				delete(agents.connections, conn)
				orphaned = append(orphaned, conn)
			}
		}
	}
	remaining = len(agents.agentsById)
	agents.mux.Unlock()

	for _, conn := range orphaned {
		// Best effort: the connection is very likely already dead, which is why
		// its Agent was reaped in the first place.
		_ = conn.Disconnect()
	}

	return reaped, remaining
}

func (agents *Agents) count() int {
	agents.mux.RLock()
	defer agents.mux.RUnlock()

	return len(agents.agentsById)
}
