package verifier

import (
	"context"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"go.mongodb.org/mongo-driver/v2/event"
)

// getMoreCursorTypeKey is the context key that marks a context’s getMores
// as a change stream’s.
type getMoreCursorTypeKey struct{}

// withChangeStreamGetMores marks the getMores that ctx runs as a change
// stream’s. Unmarked getMores are counted as a collection cursor’s.
func withChangeStreamGetMores(ctx context.Context) context.Context {
	return context.WithValue(ctx, getMoreCursorTypeKey{}, metricCursorTypeChangeStream)
}

// newGetMoreMonitor returns a command monitor that records each successful
// getMore’s latency for the given cluster label.
//
// It sets only Succeeded: with a Started callback, the driver would copy
// every command it sends, including every write’s documents. A Succeeded
// event carries the reply, which names the cursor’s namespace, so nothing
// more is needed.
func (verifier *Verifier) newGetMoreMonitor(cluster string) *event.CommandMonitor {
	return &event.CommandMonitor{
		Succeeded: func(ctx context.Context, evt *event.CommandSucceededEvent) {
			if evt.CommandName != "getMore" {
				return
			}

			if ctx.Value(getMoreCursorTypeKey{}) == metricCursorTypeChangeStream {
				verifier.getMetrics().recordChangeStreamGetMore(ctx, cluster, evt.Duration)

				return
			}

			verifier.getMetrics().recordCollectionGetMore(
				ctx,
				cluster,
				getMoreNamespace(evt),
				evt.Duration,
			)
		},
	}
}

// getMoreNamespace returns a getMore reply’s cursor namespace, or an empty
// string if the reply lacks one.
func getMoreNamespace(evt *event.CommandSucceededEvent) string {
	ns, err := bsontools.RawLookup[string](evt.Reply, "cursor", "ns")
	if err != nil {
		return ""
	}

	return ns
}
