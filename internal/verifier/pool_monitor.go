package verifier

import (
	"context"
	"maps"
	"sync"

	"go.mongodb.org/mongo-driver/v2/event"
)

// poolCounts are a server’s connection counts.
type poolCounts struct {
	open  int64
	inUse int64
}

// poolMonitor turns one cluster’s connection pool events into metrics. Every
// client that connects to the cluster can share it.
//
// It keeps its own connection counts, rather than recording them to up/down
// counters, so that the counts include connections that were opened before
// the verifier’s metrics were set up.
type poolMonitor struct {
	cluster    whichCluster
	getMetrics func() *verifierMetrics

	mu     sync.Mutex
	counts map[string]poolCounts
}

var _ poolGaugeSource = &poolMonitor{}

func newPoolMonitor(cluster whichCluster, getMetrics func() *verifierMetrics) *poolMonitor {
	return &poolMonitor{
		cluster:    cluster,
		getMetrics: getMetrics,
		counts:     map[string]poolCounts{},
	}
}

// driverMonitor returns a driver monitor that sends events to pm.
func (pm *poolMonitor) driverMonitor() *event.PoolMonitor {
	return &event.PoolMonitor{Event: pm.handle}
}

func (pm *poolMonitor) getWhichCluster() whichCluster {
	return pm.cluster
}

func (pm *poolMonitor) getPoolCounts() map[string]poolCounts {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	return maps.Clone(pm.counts)
}

// handle receives a pool event. The driver calls it synchronously, often while
// an operation waits, so it must stay cheap.
func (pm *poolMonitor) handle(evt *event.PoolEvent) {
	// The driver’s events carry no context.
	ctx := context.Background()

	switch evt.Type {
	case event.ConnectionCreated:
		pm.update(evt.Address, func(c *poolCounts) { c.open++ })
	case event.ConnectionClosed:
		pm.update(evt.Address, func(c *poolCounts) { c.open-- })
	case event.ConnectionCheckedOut:
		pm.update(evt.Address, func(c *poolCounts) { c.inUse++ })
		pm.getMetrics().recordConnectionCheckout(ctx, pm.cluster, evt.Address, evt.Duration)
	case event.ConnectionCheckedIn:
		pm.update(evt.Address, func(c *poolCounts) { c.inUse-- })
	case event.ConnectionCheckOutFailed:
		pm.getMetrics().recordConnectionCheckoutFailure(ctx, pm.cluster, evt.Address, evt.Reason)
	case event.ConnectionPoolCleared:
		pm.getMetrics().recordConnectionPoolClear(ctx, pm.cluster, evt.Address)
	}
}

func (pm *poolMonitor) update(address string, change func(*poolCounts)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	counts := pm.counts[address]
	change(&counts)
	pm.counts[address] = counts
}
