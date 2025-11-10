package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type changeReader interface {
	getWhichCluster() whichCluster
	getReadChannel() <-chan changeEventBatch
	getError() *util.Eventual[error]
	getStartTimestamp() bson.Timestamp
	getEventsPerSecond() option.Option[float64]
	getLag() option.Option[time.Duration]
	getBufferSaturation() float64
	setWritesOff(bson.Timestamp)
	setPersistorError(error)
	start(context.Context) error
	done() <-chan struct{}
	persistChangeStreamResumeToken(context.Context, bson.Raw) error
	isRunning() bool
	String() string
}
