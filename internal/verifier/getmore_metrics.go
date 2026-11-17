package verifier

import (
	"context"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"go.mongodb.org/mongo-driver/v2/event"
)

// changeStreamNamespace is the cursor namespace of a cluster-wide aggregation,
// such as the change streams that the verifier opens with mongo.Client.Watch.
const changeStreamNamespace = "admin.$cmd.aggregate"

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

			ns := getMoreNamespace(evt)

			if ns == changeStreamNamespace {
				verifier.getMetrics().recordChangeStreamGetMore(ctx, cluster, evt.Duration)

				return
			}

			verifier.getMetrics().recordCollectionGetMore(ctx, cluster, ns, evt.Duration)
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
