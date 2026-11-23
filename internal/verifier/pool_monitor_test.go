package verifier

import (
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/event"
)

// The pool monitor counts each server’s open and checked-out connections,
// and it records checkouts, checkout failures, and pool clears.
func TestPoolMonitor(t *testing.T) {
	pipeline, err := metrics.Setup(logger.NewDebugLogger(), metrics.Settings{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pipeline.Shutdown(t.Context())) })

	m := newVerifierMetrics(pipeline.MeterProvider())
	pm := newPoolMonitor(dst, func() *verifierMetrics { return m })
	require.NoError(t, m.observePools(pm))

	const host1, host2 = "host1:27017", "host2:27017"

	monitor := pm.driverMonitor()
	for _, evt := range []event.PoolEvent{
		{Type: event.ConnectionCreated, Address: host1},
		{Type: event.ConnectionCreated, Address: host1},
		{Type: event.ConnectionCreated, Address: host2},
		{Type: event.ConnectionCheckedOut, Address: host1, Duration: 2 * time.Millisecond},
		{Type: event.ConnectionCheckedOut, Address: host1, Duration: 4 * time.Millisecond},
		{Type: event.ConnectionCheckedIn, Address: host1},
		{Type: event.ConnectionClosed, Address: host2},
		{Type: event.ConnectionCheckOutFailed, Address: host2, Reason: event.ReasonTimedOut},
		{Type: event.ConnectionPoolCleared, Address: host2},
	} {
		monitor.Event(&evt)
	}

	assert.Equal(
		t,
		map[string]poolCounts{
			host1: {open: 2, inUse: 1},
			host2: {open: 0, inUse: 0},
		},
		pm.getPoolCounts(),
	)

	families := scrapeFamilies(t, pipeline.Handler())

	assert.Equal(
		t,
		map[string]float64{host1: 2, host2: 0},
		valuesByLabel(families["migration_verifier_connections_open"], "address"),
	)
	assert.Equal(
		t,
		map[string]float64{host1: 1, host2: 0},
		valuesByLabel(families["migration_verifier_connections_in_use"], "address"),
	)
	assert.Equal(
		t,
		map[string]uint64{host1: 2},
		histogramCountByLabel(families["migration_verifier_connection_checkout_duration_seconds"], "address"),
	)
	assert.InDelta(
		t,
		0.006,
		histogramSumByLabel(families["migration_verifier_connection_checkout_duration_seconds"], "address")[host1],
		1e-9,
	)
	assert.Equal(
		t,
		1.0,
		sampleValue(
			families["migration_verifier_connection_checkout_failures_total"],
			map[string]string{"cluster": "destination", "address": host2, "reason": "timeout"},
		),
	)
	assert.Equal(
		t,
		map[string]float64{"destination": 1},
		valuesByLabel(families["migration_verifier_connection_pool_clears_total"], "cluster"),
	)
}
