package verifier

import (
	"context"
	b64 "encoding/base64"
	"fmt"
	"strings"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Specify states
const (
	Unprocessed = "unprocessed"
	Upserted    = "upserted"
	Deleted     = "deleted"
	NotFound    = "not found"
)

// Specify error codes
const (
	ErrorGetClusterStatsSummary = 522601
	ErrorTrackedDirtyBytes
	ErrorReplicationDelayed

	ErrorInsertTask
	ErrorUpdateParentTask
	ErrorUpdateTask
)

// SplitNamespace returns db, collection
func SplitNamespace(namespace string) (string, string) {
	dot := strings.Index(namespace, ".")
	if dot < 0 {
		return namespace, ""
	}
	return namespace[:dot], namespace[dot+1:]
}

// Returns full name of collection including database name
func FullName(collection *mongo.Collection) string {
	return collection.Database().Name() + "." + collection.Name()
}

// Namespace represents a db and coll.
type Namespace struct {
	// The database and collection name of the namespace being copied.
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

func (ns *Namespace) String() string {
	return fmt.Sprintf("{ db: %s, coll: %s }", ns.DB, ns.Coll)
}

func (ns *Namespace) FullName() string {
	return ns.DB + "." + ns.Coll
}

// NewNamespace returns a new Namespace struct with the given parameters.
func NewNamespace(db, coll string) *Namespace {
	return &Namespace{
		DB:   db,
		Coll: coll,
	}
}

// Refetch contains the data necessary to track a refretch
type Refetch struct {
	ID            interface{} `bson:"id"`
	SrcNamespace  string      `bson:"srcNamespace"`
	DestNamespace string      `bson:"destNamespace"`
	Status        string
}

// TaskError contains error Code and Message
type TaskError struct {
	Code    int
	Message string
}

func (e TaskError) Error() string {
	return e.Message
}

// QueryFilter stores namespace and partition info
type QueryFilter struct {
	Partition *partitions.Partition `bson:"partition"`
	Namespace string                `json:"namespace" bson:"namespace"`
	To        string                `json:"to,omitempty" bson:"to,omitempty"`
}

func RawToString(b bson.RawValue) string {
	return b64.StdEncoding.EncodeToString([]byte{byte(b.Type)}) + "#" +
		b64.StdEncoding.EncodeToString(b.Value)
}

// GetLastOpTimeAndSyncShardClusterTime retrieves the last operation time on the source. If maxClusterTime is provided,
// the `AppendOplogNote` will only be performed on the shards that has a $clusterTime lower than the provided value.
// This function is exposed for testing purposes.
// This is slightly modified from the code in mongosync to use *mongo.Client instead of the
// mongosync util.Client.
func GetLastOpTimeAndSyncShardClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	retryer retry.Retryer,
	client *mongo.Client,
	retryOnLockFailed bool,
) (*primitive.Timestamp, error) {
	// 'AppendOplogNote' will perform a no-op write on the source cluster. When run against
	// sharded clusters, this command will perform the no-op write on all shards. In receiving
	// the shard responses, the driver notes the highest $clusterTime amongst all of them.
	var response bson.Raw
	appendOplogNoteCmd := bson.D{
		primitive.E{Key: "appendOplogNote", Value: 1},
		primitive.E{Key: "data", Value: primitive.E{Key: "migration-verifier", Value: "last op fetching"}}}

	if retryOnLockFailed {
		retryer = retryer.WithErrorCodes(util.LockFailed)
	}
	err := retryer.RunForTransientErrorsOnly(ctx, logger, func(ri *retry.Info) error {
		ri.Log(logger.Logger,
			"appendOplogNote",
			"source",
			"",
			"",
			fmt.Sprintf("Running appendOplogNote command. %v", appendOplogNoteCmd))
		ret := client.Database("admin").RunCommand(ctx, appendOplogNoteCmd)
		var err error
		if response, err = ret.DecodeBytes(); err != nil {
			return err
		}

		return nil
	})

	// When we issue a maxClusterTime lower than any shard's current $cluster_time, we will receive an StaleClusterTime error.
	// The command will essentially be a noop on that particular shard.
	// Since mongos will broadcast the command to all the shards, this error doesn't affect correctness.
	if err != nil && !util.IsStaleClusterTimeError(err) {
		return nil, errors.Wrap(err,
			"failed to issue appendOplogNote command on source cluster")
	}

	// Get the `operationTime` from the response and return it.
	rawOperationTime, err := response.LookupErr("operationTime")
	if err != nil {
		return nil, errors.Wrap(err,
			"failed to get operationTime from source cluster's appendOplogNote response")
	}

	t, i := rawOperationTime.Timestamp()
	return &primitive.Timestamp{T: t, I: i}, nil
}
