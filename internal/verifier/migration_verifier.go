package verifier

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	_ "net/http/pprof"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"golang.org/x/exp/maps"
)

// ReadConcernSetting describes the verifier’s handling of read
// concern.
type ReadConcernSetting string

const (
	//TODO: add comments for each of these so the warnings will stop :)
	Missing           = "Missing"
	Failed            = "Failed"
	Mismatch          = "Mismatch"
	ClusterTarget     = "dstClient"
	ClusterSource     = "srcClient"
	SrcNamespaceField = "query_filter.namespace"
	DstNamespaceField = "query_filter.to"
	NumWorkers        = 10
	Idle              = "idle"
	Check             = "check"
	Recheck           = "recheck"

	// ReadConcernMajority means to force majority read concern.
	// This is generally desirable to ensure consistency.
	ReadConcernMajority ReadConcernSetting = "forceMajority"

	// ReadConcernIgnore means to use connection-default read concerns.
	// This setting’s main purpose is to allow local read concern,
	// which is important for deployments where majority read concern
	// is unworkable.
	ReadConcernIgnore ReadConcernSetting = "ignore"

	configDBName  = "config"
	collsCollName = "collections"

	DefaultFailureDisplaySize = 20

	okSymbol    = "\u2705" // white heavy check mark
	infoSymbol  = "\u24d8" // circled Latin small letter I
	notOkSymbol = "\u2757" // heavy exclamation mark symbol
)

var timeFormat = time.RFC3339

// Verifier is the main state for the migration verifier
type Verifier struct {
	writesOff          bool
	lastGeneration     bool
	running            bool
	generation         int
	phase              string
	port               int
	metaURI            string
	metaClient         *mongo.Client
	srcClient          *mongo.Client
	dstClient          *mongo.Client
	srcBuildInfo       *util.BuildInfo
	dstBuildInfo       *util.BuildInfo
	numWorkers         int
	failureDisplaySize int64

	eventRecorder *EventRecorder

	// Used only with generation 0 to defer the first
	// progress report until after we’ve finished partitioning
	// every collection.
	gen0PendingCollectionTasks atomic.Int32

	generationStartTime        time.Time
	generationPauseDelayMillis time.Duration
	workerSleepDelayMillis     time.Duration
	ignoreBSONFieldOrder       bool
	verifyAll                  bool
	startClean                 bool

	// This would seem more ideal as uint64, but changing it would
	// trigger several other similar type changes, and that’s not really
	// worthwhile for now.
	partitionSizeInBytes int64

	readPreference *readpref.ReadPref

	logger *logger.Logger
	writer io.Writer

	srcNamespaces []string
	dstNamespaces []string
	nsMap         map[string]string
	metaDBName    string
	srcStartAtTs  *primitive.Timestamp

	mux                         sync.RWMutex
	changeStreamRunning         bool
	changeStreamWritesOffTsChan chan primitive.Timestamp
	changeStreamErrChan         chan error
	changeStreamDoneChan        chan struct{}
	lastChangeEventTime         *primitive.Timestamp
	writesOffTimestamp          *primitive.Timestamp

	readConcernSetting ReadConcernSetting

	// A user-defined $match-compatible document-level query filter.
	// The filter is applied to all namespaces in both initial checking and iterative checking.
	// The verifier only checks documents within the filter.
	globalFilter map[string]any

	pprofInterval time.Duration

	verificationStatusCheckInterval time.Duration
}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	TotalTasks            int `json:"totalTasks"`
	AddedTasks            int `json:"addedTasks"`
	ProcessingTasks       int `json:"processingTasks"`
	FailedTasks           int `json:"failedTasks"`
	CompletedTasks        int `json:"completedTasks"`
	MetadataMismatchTasks int `json:"metadataMismatchTasks"`
}

// VerificationResult holds the Verification Results.
type VerificationResult struct {

	// This field gets used differently depending on whether this result
	// came from a document comparison or something else. If it’s from a
	// document comparison, it *MUST* be a document ID, not a
	// documentmap.MapKey, because we query on this to populate verification
	// tasks for rechecking after a document mismatch. Thus, in sharded
	// clusters with duplicate document IDs in the same collection, multiple
	// VerificationResult instances might share the same ID. That’s OK,
	// though; it’ll just make the recheck include all docs with that ID,
	// regardless of which ones actually need the recheck.
	ID interface{}

	Field     interface{}
	Details   interface{}
	Cluster   interface{}
	NameSpace interface{}
	// The data size of the largest of the mismatched objects.
	// Note this is not persisted; it is used only to ensure recheck tasks
	// don't get too large.
	dataSize int
}

// VerifierSettings is NewVerifier’s argument.
type VerifierSettings struct {
	ReadConcernSetting
}

// NewVerifier creates a new Verifier
func NewVerifier(settings VerifierSettings) *Verifier {
	readConcern := settings.ReadConcernSetting
	if readConcern == "" {
		readConcern = ReadConcernMajority
	}

	return &Verifier{
		phase:                       Idle,
		numWorkers:                  NumWorkers,
		readPreference:              readpref.Primary(),
		partitionSizeInBytes:        400 * 1024 * 1024,
		failureDisplaySize:          DefaultFailureDisplaySize,
		changeStreamWritesOffTsChan: make(chan primitive.Timestamp),
		changeStreamErrChan:         make(chan error),
		changeStreamDoneChan:        make(chan struct{}),
		readConcernSetting:          readConcern,

		// This will get recreated once gen0 starts, but we want it
		// here in case the change streams gets an event before then.
		eventRecorder: NewEventRecorder(),

		verificationStatusCheckInterval: 15 * time.Second,
	}
}

// ConfigureReadConcern
func (verifier *Verifier) ConfigureReadConcern(setting ReadConcernSetting) {
	verifier.readConcernSetting = setting
}

func (verifier *Verifier) getClientOpts(uri string) *options.ClientOptions {
	appName := "Migration Verifier"
	opts := &options.ClientOptions{
		AppName: &appName,
	}
	opts.ApplyURI(uri)
	opts.SetWriteConcern(writeconcern.Majority())

	verifier.doIfForceReadConcernMajority(func() {
		opts.SetReadConcern(readconcern.Majority())
	})

	return opts
}

func (verifier *Verifier) SetFailureDisplaySize(size int64) {
	verifier.failureDisplaySize = size
}

func (verifier *Verifier) WritesOff(ctx context.Context) error {
	verifier.logger.Debug().
		Msg("WritesOff called.")

	verifier.mux.Lock()
	verifier.writesOff = true

	if verifier.writesOffTimestamp == nil {
		verifier.logger.Debug().Msg("Change stream still running. Signalling that writes are done.")

		finalTs, err := GetNewClusterTime(
			ctx,
			verifier.logger,
			verifier.srcClient,
		)

		if err != nil {
			return errors.Wrapf(err, "failed to fetch source's cluster time")
		}

		verifier.writesOffTimestamp = &finalTs

		verifier.mux.Unlock()

		// This has to happen outside the lock because the change stream
		// might be inserting docs into the recheck queue, which happens
		// under the lock.
		select {
		case verifier.changeStreamWritesOffTsChan <- finalTs:
		case err := <-verifier.changeStreamErrChan:
			return errors.Wrap(err, "tried to send writes-off timestamp to change stream, but change stream already failed")
		}
	} else {
		verifier.mux.Unlock()
	}

	return nil
}

func (verifier *Verifier) WritesOn(ctx context.Context) {
	verifier.mux.Lock()
	verifier.writesOff = false
	verifier.mux.Unlock()
}

func (verifier *Verifier) GetLogger() *logger.Logger {
	return verifier.logger
}

func (verifier *Verifier) SetMetaURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.metaClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return err
	}

	verifier.metaURI = uri

	return err
}

func (verifier *Verifier) AddMetaIndexes(ctx context.Context) error {
	model := mongo.IndexModel{Keys: bson.M{"generation": 1}}
	_, err := verifier.verificationTaskCollection().Indexes().CreateOne(ctx, model)
	if err != nil {
		return err
	}
	model = mongo.IndexModel{Keys: bson.D{{"_id.generation", 1}}}
	_, err = verifier.verificationDatabase().Collection(recheckQueue).Indexes().CreateOne(ctx, model)
	return err
}

func (verifier *Verifier) SetSrcURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.srcClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to source %#q", uri)
	}

	buildInfo, err := util.GetBuildInfo(ctx, verifier.srcClient)
	if err != nil {
		return errors.Wrap(err, "failed to read source build info")
	}

	verifier.srcBuildInfo = &buildInfo
	return nil
}

func (verifier *Verifier) SetDstURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.dstClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to destination %#q", uri)
	}

	buildInfo, err := util.GetBuildInfo(ctx, verifier.dstClient)
	if err != nil {
		return errors.Wrap(err, "failed to read destination build info")
	}

	verifier.dstBuildInfo = &buildInfo
	return nil
}

func (verifier *Verifier) SetServerPort(port int) {
	verifier.port = port
}

func (verifier *Verifier) SetNumWorkers(arg int) {
	verifier.numWorkers = arg
}

func (verifier *Verifier) SetGenerationPauseDelayMillis(arg time.Duration) {
	verifier.generationPauseDelayMillis = arg
}

func (verifier *Verifier) SetWorkerSleepDelayMillis(arg time.Duration) {
	verifier.workerSleepDelayMillis = arg
}

// SetPartitionSizeMB sets the verifier’s maximum partition size in MiB.
func (verifier *Verifier) SetPartitionSizeMB(partitionSizeMB uint32) {
	verifier.partitionSizeInBytes = int64(partitionSizeMB) * 1024 * 1024
}

func (verifier *Verifier) SetLogger(logPath string) {
	verifier.logger, verifier.writer = getLoggerAndWriter(logPath)
}

func (verifier *Verifier) SetSrcNamespaces(arg []string) {
	verifier.srcNamespaces = arg
}

func (verifier *Verifier) SetDstNamespaces(arg []string) {
	verifier.dstNamespaces = arg
}

func (verifier *Verifier) SetNamespaceMap() {
	verifier.nsMap = make(map[string]string)
	if len(verifier.dstNamespaces) == 0 {
		return
	}
	for i, ns := range verifier.srcNamespaces {
		verifier.nsMap[ns] = verifier.dstNamespaces[i]
	}
}

func (verifier *Verifier) SetMetaDBName(arg string) {
	verifier.metaDBName = arg
}

func (verifier *Verifier) SetIgnoreBSONFieldOrder(arg bool) {
	verifier.ignoreBSONFieldOrder = arg
}

func (verifier *Verifier) SetVerifyAll(arg bool) {
	verifier.verifyAll = arg
}

func (verifier *Verifier) SetStartClean(arg bool) {
	verifier.startClean = arg
}

func (verifier *Verifier) SetReadPreference(arg string) error {
	mode, err := readpref.ModeFromString(arg)
	if err != nil {
		return err
	}
	verifier.readPreference, err = readpref.New(mode)
	return err
}

func (verifier *Verifier) SetPprofInterval(arg string) error {
	if arg == "" {
		return nil
	}

	interval, err := time.ParseDuration(arg)
	if err != nil {
		return err
	}

	verifier.pprofInterval = interval
	return nil
}

// DocumentStats gets various stats (TODO clarify)
func DocumentStats(ctx context.Context, client *mongo.Client, namespaces []string) {

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Doc Count", "Database", "Collection"})

	for _, n := range namespaces {
		db, coll := SplitNamespace(n)
		if db != "" {
			s, _ := client.Database(db).Collection(coll).EstimatedDocumentCount(ctx)
			table.Append([]string{strconv.FormatInt(s, 10), db, coll})
		}
	}
	table.Render()
	fmt.Println()
}

func (verifier *Verifier) getGeneration() (generation int, lastGeneration bool) {
	verifier.mux.RLock()
	generation, lastGeneration = verifier.generation, verifier.lastGeneration
	verifier.mux.RUnlock()
	return
}

func (verifier *Verifier) getGenerationWhileLocked() (int, bool) {

	// As long as no other goroutine has locked the mux this will
	// usefully panic if the caller neglected the lock.
	wasUnlocked := verifier.mux.TryRLock()
	if wasUnlocked {
		verifier.mux.RUnlock()
		panic("getGenerationWhileLocked() while unlocked")
	}

	return verifier.generation, verifier.lastGeneration
}

func (verifier *Verifier) maybeAppendGlobalFilterToPredicates(predicates bson.A) bson.A {
	if len(verifier.globalFilter) == 0 {
		verifier.logger.Debug().Msg("No filter to append; globalFilter is nil")
		return predicates
	}
	verifier.logger.Debug().Str("filter", fmt.Sprintf("%v", verifier.globalFilter)).Msg("Appending filter to find query")
	return append(predicates, verifier.globalFilter)
}

func (verifier *Verifier) getDocumentsCursor(ctx context.Context, collection *mongo.Collection, buildInfo *util.BuildInfo,
	startAtTs *primitive.Timestamp, task *VerificationTask) (*mongo.Cursor, error) {
	var findOptions bson.D
	runCommandOptions := options.RunCmd()
	var andPredicates bson.A

	if len(task.Ids) > 0 {
		andPredicates = append(andPredicates, bson.D{{"_id", bson.M{"$in": task.Ids}}})
		andPredicates = verifier.maybeAppendGlobalFilterToPredicates(andPredicates)
		findOptions = bson.D{
			bson.E{"filter", bson.D{{"$and", andPredicates}}},
		}
	} else {
		findOptions = task.QueryFilter.Partition.GetFindOptions(buildInfo, verifier.maybeAppendGlobalFilterToPredicates(andPredicates))
	}
	if verifier.readPreference.Mode() != readpref.PrimaryMode {
		runCommandOptions = runCommandOptions.SetReadPreference(verifier.readPreference)
		if startAtTs != nil {

			// We never want to read before the change stream start time,
			// or for the last generation, the change stream end time.
			findOptions = append(
				findOptions,
				bson.E{"readConcern", bson.D{
					{"afterClusterTime", *startAtTs},
				}},
			)
		}
	}
	findCmd := append(bson.D{{"find", collection.Name()}}, findOptions...)
	verifier.logger.Debug().
		Interface("task", task.PrimaryKey).
		Str("findCmd", fmt.Sprintf("%s", findCmd)).
		Str("options", fmt.Sprintf("%v", *runCommandOptions)).
		Msg("getDocuments findCmd.")

	return collection.Database().RunCommandCursor(ctx, findCmd, runCommandOptions)
}

func mismatchResultsToVerificationResults(mismatch *MismatchDetails, srcClientDoc, dstClientDoc bson.Raw, namespace string, id interface{}, fieldPrefix string) (results []VerificationResult) {
	for _, field := range mismatch.missingFieldOnSrc {
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   Missing,
			Cluster:   ClusterSource,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}

	for _, field := range mismatch.missingFieldOnDst {
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   Missing,
			Cluster:   ClusterTarget,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}

	for _, field := range mismatch.fieldContentsDiffer {
		srcClientValue := srcClientDoc.Lookup(field)
		dstClientValue := dstClientDoc.Lookup(field)
		details := Mismatch + fmt.Sprintf(" : Document %s failed comparison on field %s between srcClient (Type: %s) and dstClient (Type: %s)", id, fieldPrefix+field, srcClientValue.Type, dstClientValue.Type)
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   details,
			Cluster:   ClusterTarget,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}
	return
}

func (verifier *Verifier) compareOneDocument(srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		return nil, nil
	}
	//verifier.logger.Info().Msg("Byte comparison failed for id %s, falling back to field comparison", id)

	if verifier.ignoreBSONFieldOrder {
		mismatch, err := BsonUnorderedCompareRawDocumentWithDetails(srcClientDoc, dstClientDoc)
		if err != nil {
			return nil, err
		}
		if mismatch == nil {
			return nil, nil
		}
		results := mismatchResultsToVerificationResults(mismatch, srcClientDoc, dstClientDoc, namespace, srcClientDoc.Lookup("_id"), "" /* fieldPrefix */)
		return results, nil
	}
	dataSize := len(srcClientDoc)
	if dataSize < len(dstClientDoc) {
		dataSize = len(dstClientDoc)
	}

	// If we're respecting field order we have just done a binary compare so don't know the mismatching fields.
	return []VerificationResult{{
		ID:        srcClientDoc.Lookup("_id"),
		Details:   Mismatch,
		Cluster:   ClusterTarget,
		NameSpace: namespace,
		dataSize:  dataSize,
	}}, nil
}

func (verifier *Verifier) ProcessVerifyTask(ctx context.Context, workerNum int, task *VerificationTask) error {
	start := time.Now()

	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Interface("task", task.PrimaryKey).
		Msg("Processing document comparison task.")

	defer func() {
		elapsed := time.Since(start)

		verifier.logger.Debug().
			Int("workerNum", workerNum).
			Interface("task", task.PrimaryKey).
			Stringer("timeElapsed", elapsed).
			Msg("Finished document comparison task.")
	}()

	problems, docsCount, bytesCount, err := verifier.FetchAndCompareDocuments(
		ctx,
		task,
	)

	if err != nil {
		return errors.Wrapf(
			err,
			"worker %d failed to process document comparison task %s (namespace: %#q)",
			workerNum,
			task.PrimaryKey,
			task.QueryFilter.Namespace,
		)
	}

	task.SourceDocumentCount = docsCount
	task.SourceByteCount = bytesCount

	if len(problems) == 0 {
		task.Status = verificationTaskCompleted
	} else {
		task.Status = verificationTaskFailed
		// We know we won't change lastGeneration while verification tasks are running, so no mutex needed here.
		if verifier.lastGeneration {
			verifier.logger.Error().
				Int("workerNum", workerNum).
				Interface("task", task.PrimaryKey).
				Str("namespace", task.QueryFilter.Namespace).
				Int("mismatchesCount", len(problems)).
				Msg("Document(s) mismatched, and this is the final generation.")
		} else {
			verifier.logger.Debug().
				Int("workerNum", workerNum).
				Interface("task", task.PrimaryKey).
				Str("namespace", task.QueryFilter.Namespace).
				Int("mismatchesCount", len(problems)).
				Msg("Document comparison task failed, but it may pass in the next generation.")

			var mismatches []VerificationResult
			var missingIds []interface{}
			var dataSizes []int

			// This stores all IDs for the next generation to check.
			// Its length should equal len(mismatches) + len(missingIds).
			var idsToRecheck []interface{}

			for _, mismatch := range problems {
				idsToRecheck = append(idsToRecheck, mismatch.ID)
				dataSizes = append(dataSizes, mismatch.dataSize)

				if mismatch.Details == Missing {
					missingIds = append(missingIds, mismatch.ID)
				} else {
					mismatches = append(mismatches, mismatch)
				}
			}

			// Update ids of the failed task so that only mismatches and
			// missing are reported. Matching documents are thus hidden
			// from the progress report.
			task.Ids = missingIds
			task.FailedDocs = mismatches

			// Create a task for the next generation to recheck the
			// mismatched & missing docs.
			err := verifier.InsertFailedCompareRecheckDocs(ctx, task.QueryFilter.Namespace, idsToRecheck, dataSizes)
			if err != nil {
				return errors.Wrapf(
					err,
					"failed to enqueue %d recheck(s) of mismatched documents",
					len(idsToRecheck),
				)
			}
		}
	}

	return errors.Wrapf(
		verifier.UpdateVerificationTask(ctx, task),
		"failed to persist task %s's new status (%#q)",
		task.PrimaryKey,
		task.Status,
	)
}

func (verifier *Verifier) logChunkInfo(ctx context.Context, namespaceAndUUID *uuidutil.NamespaceAndUUID) {
	// Only log full chunk info in debug mode
	debugMsg := verifier.logger.Debug()
	if !debugMsg.Enabled() {
		return
	}
	debugMsg.Discard()

	uuid := namespaceAndUUID.UUID
	namespace := namespaceAndUUID.DBName + "." + namespaceAndUUID.CollName
	configChunkColl := verifier.srcClientDatabase("config").Collection("chunks")
	cursor, err := configChunkColl.Find(ctx, bson.D{{"uuid", uuid}})
	if err != nil {
		verifier.logger.Error().Msgf("Unable to read chunk info for %s: %v", namespace, err)
		return
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var resultMap bson.M
		if err = cursor.Decode(&resultMap); err != nil {
			verifier.logger.Error().Msgf("Error decoding chunk info for %s: %v", namespace, err)
			return
		}
		verifier.logger.Debug().Msgf(" Chunk of %s on %v, range %v to %v", namespace, resultMap["shard"],
			resultMap["min"], resultMap["max"])
	}
	if err = cursor.Err(); err != nil {
		verifier.logger.Error().Msgf("Error reading chunk info for %s: %v", namespace, err)
	}
}

func (verifier *Verifier) getShardingInfo(ctx context.Context, namespaceAndUUID *uuidutil.NamespaceAndUUID) ([]string, error) {
	uuid := namespaceAndUUID.UUID
	namespace := namespaceAndUUID.DBName + "." + namespaceAndUUID.CollName
	configCollectionsColl := verifier.srcClientDatabase(configDBName).Collection(collsCollName)
	cursor, err := configCollectionsColl.Find(ctx, bson.D{{"uuid", uuid}})
	if err != nil {
		return nil, fmt.Errorf("Failed to read sharding info for %s: %v", namespace, err)
	}
	defer cursor.Close(ctx)
	collectionSharded := false

	shardKeys := []string{}

	for cursor.Next(ctx) {
		collectionSharded = true
		var result struct {
			Key bson.M
		}

		if err = cursor.Decode(&result); err != nil {
			return nil, fmt.Errorf("Failed to decode sharding info for %s: %v", namespace, err)
		}

		verifier.logger.Debug().Msgf("Collection %s is sharded with shard key %v", namespace, result.Key)

		shardKeys = maps.Keys(result.Key)
		sort.Strings(shardKeys)
	}
	if err = cursor.Err(); err != nil {
		verifier.logger.Error().Msgf("Error reading sharding info for %s: %v", namespace, err)
	}
	if collectionSharded {
		verifier.logChunkInfo(ctx, namespaceAndUUID)
	}

	return shardKeys, nil
}

// partitionAndInspectNamespace does a few preliminary tasks for the
// given namespace:
//  1. Devise partition boundaries.
//  2. Fetch shard keys.
//  3. Fetch the size: # of docs, and # of bytes.
func (verifier *Verifier) partitionAndInspectNamespace(ctx context.Context, namespace string) ([]*partitions.Partition, []string, types.DocumentCount, types.ByteCount, error) {
	retryer := retry.New(retry.DefaultDurationLimit).SetRetryOnUUIDNotSupported()
	dbName, collName := SplitNamespace(namespace)
	namespaceAndUUID, err := uuidutil.GetCollectionNamespaceAndUUID(ctx, verifier.logger, retryer,
		verifier.srcClientDatabase(dbName), collName)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	shardKeys, err := verifier.getShardingInfo(ctx, namespaceAndUUID)
	if err != nil {
		return nil, nil, 0, 0, err
	}

	// The partitioner doles out ranges to replicators; we don't use that functionality so we just pass
	// one "replicator".
	replicator1 := partitions.Replicator{ID: "verifier"}
	replicators := []partitions.Replicator{replicator1}
	partitionList, srcDocs, srcBytes, err := partitions.PartitionCollectionWithSize(
		ctx, namespaceAndUUID, retryer, verifier.srcClient, replicators, verifier.logger, verifier.partitionSizeInBytes, verifier.globalFilter)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// TODO: Test the empty collection (which returns no partitions)
	if len(partitionList) == 0 {
		partitionList = []*partitions.Partition{{
			Key: partitions.PartitionKey{
				SourceUUID:  namespaceAndUUID.UUID,
				MongosyncID: "verifier"},
			Ns: &partitions.Namespace{
				DB:   namespaceAndUUID.DBName,
				Coll: namespaceAndUUID.CollName}}}
	}
	// Use "open" partitions, otherwise out-of-range keys on the destination might be missed
	partitionList[0].Key.Lower = primitive.MinKey{}
	partitionList[len(partitionList)-1].Upper = primitive.MaxKey{}
	debugLog := verifier.logger.Debug()
	if debugLog.Enabled() {
		debugLog.Msgf("Partitions (%d):", len(partitionList))
		for i, partition := range partitionList {
			verifier.logger.Debug().Msgf("Partition %d: %+v -> %+v", i, partition.Key.Lower, partition.Upper)
		}
	}
	rand.Shuffle(len(partitionList), func(i, j int) {
		tmp := partitionList[i]
		partitionList[i] = partitionList[j]
		partitionList[j] = tmp
	})
	debugLog = verifier.logger.Debug()
	if debugLog.Enabled() {
		debugLog.Msgf("Shuffled Partitions (%d):", len(partitionList))
		for i, partition := range partitionList {
			verifier.logger.Debug().Msgf("Shuffled Partition %d: %+v -> %+v", i, partition.Key.Lower, partition.Upper)
		}
	}

	return partitionList, shardKeys, srcDocs, srcBytes, nil
}

// Returns a slice of VerificationResults with the differences, and a boolean indicating whether or
// not the collection data can be safely verified.
func (verifier *Verifier) compareCollectionSpecifications(
	srcNs, dstNs string,
	srcSpecOpt, dstSpecOpt option.Option[util.CollectionSpec],
) ([]VerificationResult, bool, error) {
	srcSpec, hasSrcSpec := srcSpecOpt.Get()
	dstSpec, hasDstSpec := dstSpecOpt.Get()

	if !hasSrcSpec {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterSource,
			Details:   Missing}}, false, nil
	}
	if !hasDstSpec {
		return []VerificationResult{{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Details:   Missing}}, false, nil
	}
	if srcSpec.Type != dstSpec.Type {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterTarget,
			Field:     "Type",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Type, dstSpec.Type)}}, false, nil
		// If the types differ, the rest is not important.
	}
	var results []VerificationResult
	if srcSpec.Info.ReadOnly != dstSpec.Info.ReadOnly {
		results = append(results, VerificationResult{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Field:     "ReadOnly",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Info.ReadOnly, dstSpec.Info.ReadOnly)})
	}
	if !bytes.Equal(srcSpec.Options, dstSpec.Options) {
		mismatchDetails, err := BsonUnorderedCompareRawDocumentWithDetails(srcSpec.Options, dstSpec.Options)
		if err != nil {
			return nil, false, errors.Wrapf(
				err,
				"failed to compare namespace %#q's specifications",
				srcNs,
			)
		}
		if mismatchDetails == nil {
			results = append(results, VerificationResult{
				NameSpace: dstNs,
				Cluster:   ClusterTarget,
				Field:     "Options (Field Order Only)",
				Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Options, dstSpec.Options)})
		} else {
			results = append(results, mismatchResultsToVerificationResults(mismatchDetails, srcSpec.Options, dstSpec.Options, srcNs, nil /* id */, "Options.")...)
		}
	}

	// Don't compare view data; they have no data of their own.
	canCompareData := srcSpec.Type != "view"
	// Do not compare data between capped and uncapped collections because the partitioning is different.
	canCompareData = canCompareData && srcSpec.Options.Lookup("capped").Equal(dstSpec.Options.Lookup("capped"))

	return results, canCompareData, nil
}

func (verifier *Verifier) doIndexSpecsMatch(ctx context.Context, srcSpec bson.Raw, dstSpec bson.Raw) (bool, error) {
	// If the byte buffers match, then we’re done.
	if bytes.Equal(srcSpec, dstSpec) {
		return true, nil
	}

	var fieldsToRemove = []string{
		// v4.4 stopped adding “ns” to index fields.
		"ns",

		// v4.2+ ignores this field.
		"background",
	}

	// Next check to see if the only differences are type differences.
	// (We can safely use $documents here since this is against the metadata
	// cluster, which we can require to be v5+.)
	db := verifier.metaClient.Database(verifier.metaDBName)
	cursor, err := db.Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$documents", []bson.D{
				{
					{"spec", bson.D{{"$literal", srcSpec}}},
					{"dstSpec", bson.D{{"$literal", dstSpec}}},
				},
			}}},

			{{"$unset", lo.Reduce(
				fieldsToRemove,
				func(cur []string, field string, _ int) []string {
					return append(cur, "spec."+field, "dstSpec."+field)
				},
				[]string{},
			)}},

			// Now check to be sure that those specs match.
			{{"$match", bson.D{
				{"$expr", bson.D{
					{"$eq", mslices.Of("$spec", "$dstSpec")},
				}},
			}}},
		},
	)

	if err != nil {
		return false, errors.Wrap(err, "failed to check index specification match in metadata")
	}
	var docs []bson.Raw
	err = cursor.All(ctx, &docs)
	if err != nil {
		return false, errors.Wrap(err, "failed to parse index specification match’s result")
	}

	count := len(docs)

	switch count {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}

	return false, errors.Errorf("weirdly received %d matching index docs (should be 0 or 1)", count)
}

func (verifier *Verifier) ProcessCollectionVerificationTask(
	ctx context.Context,
	workerNum int,
	task *VerificationTask,
) error {
	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Interface("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Msg("Processing collection.")

	err := verifier.verifyMetadataAndPartitionCollection(ctx, workerNum, task)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to process collection %#q for task %s",
			task.QueryFilter.Namespace,
			task.PrimaryKey,
		)
	}

	return errors.Wrapf(
		verifier.UpdateVerificationTask(ctx, task),
		"failed to update verification task %s's status",
		task.PrimaryKey,
	)
}

func getIndexesMap(
	ctx context.Context,
	coll *mongo.Collection,
) (map[string]bson.Raw, error) {

	var specs []bson.Raw
	specsMap := map[string]bson.Raw{}

	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to read %#q’s indexes",
			FullName(coll),
		)
	}
	err = cursor.All(ctx, &specs)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to parse %#q’s indexes",
			FullName(coll),
		)
	}

	for _, spec := range specs {
		var name string
		has, err := mbson.RawLookup(spec, &name, "name")

		if !has {
			return nil, errors.Errorf(
				"%#q has an unnamed index (%+v)",
				FullName(coll),
				spec,
			)
		}

		if err != nil {
			return nil, errors.Wrapf(
				err,
				"failed to extract %#q from %#q's index specification (%+v)",
				"name",
				FullName(coll),
				spec,
			)
		}

		specsMap[name] = spec
	}

	return specsMap, nil
}

func (verifier *Verifier) verifyIndexes(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
	srcIdIndexSpec, dstIdIndexSpec bson.Raw,
) ([]VerificationResult, error) {

	srcMap, err := getIndexesMap(ctx, srcColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on source",
			FullName(srcColl),
		)
	}

	if srcIdIndexSpec != nil {
		srcMap["_id"] = srcIdIndexSpec
	}

	dstMap, err := getIndexesMap(ctx, dstColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on destination",
			FullName(dstColl),
		)
	}

	if dstIdIndexSpec != nil {
		dstMap["_id"] = dstIdIndexSpec
	}

	var results []VerificationResult
	srcMapUsed := map[string]bool{}

	for indexName, dstSpec := range dstMap {
		srcSpec, exists := srcMap[indexName]
		if exists {
			srcMapUsed[indexName] = true
			theyMatch, err := verifier.doIndexSpecsMatch(ctx, srcSpec, dstSpec)
			if err != nil {
				return nil, errors.Wrapf(
					err,
					"failed to check whether %#q's source & desstination %#q indexes match",
					FullName(srcColl),
					indexName,
				)
			}

			if !theyMatch {
				results = append(results, VerificationResult{
					NameSpace: FullName(dstColl),
					Cluster:   ClusterTarget,
					ID:        indexName,
					Details:   Mismatch + fmt.Sprintf(": src: %v, dst: %v", srcSpec, dstSpec),
				})
			}
		} else {
			results = append(results, VerificationResult{
				ID:        indexName,
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: FullName(srcColl),
			})
		}
	}

	// Find any index specs which existed in the source cluster but not the target cluster.
	for indexName := range srcMap {
		if !srcMapUsed[indexName] {
			results = append(results, VerificationResult{
				ID:        indexName,
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: FullName(dstColl)})
		}
	}
	return results, nil
}

func (verifier *Verifier) verifyMetadataAndPartitionCollection(
	ctx context.Context,
	workerNum int,
	task *VerificationTask,
) error {
	srcColl := verifier.srcClientCollection(task)
	dstColl := verifier.dstClientCollection(task)
	srcNs := FullName(srcColl)
	dstNs := FullName(dstColl)

	srcSpecOpt, err := util.GetCollectionSpecIfExists(ctx, srcColl)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to fetch %#q's specification on source",
			FullName(srcColl),
		)
	}

	dstSpecOpt, err := util.GetCollectionSpecIfExists(ctx, dstColl)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to fetch %#q's specification on destination",
			FullName(srcColl),
		)
	}

	insertFailedCollection := func() error {
		_, err := verifier.InsertFailedCollectionVerificationTask(ctx, srcNs)
		return errors.Wrapf(
			err,
			"failed to persist metadata mismatch for collection %#q",
			srcNs,
		)
	}

	srcSpec, hasSrcSpec := srcSpecOpt.Get()
	dstSpec, hasDstSpec := dstSpecOpt.Get()

	if !hasDstSpec {
		if !hasSrcSpec {
			verifier.logger.Info().
				Int("workerNum", workerNum).
				Str("srcNamespace", srcNs).
				Str("dstNamespace", dstNs).
				Msg("Collection not present on either cluster.")

			// This counts as success.
			task.Status = verificationTaskCompleted
			return nil
		}

		task.Status = verificationTaskFailed
		// Fall through here; comparing the collection specifications will produce the correct
		// failure output.
	}
	specificationProblems, verifyData, err := verifier.compareCollectionSpecifications(srcNs, dstNs, srcSpecOpt, dstSpecOpt)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to compare collection %#q's specifications",
			srcNs,
		)
	}
	if specificationProblems != nil {
		err := insertFailedCollection()
		if err != nil {
			return err
		}

		task.FailedDocs = specificationProblems
		if !verifyData {
			task.Status = verificationTaskFailed
			return nil
		}
		task.Status = verificationTaskMetadataMismatch
	}
	if !verifyData {
		// If the metadata mismatched and we're not checking the actual data, that's a complete failure.
		if task.Status == verificationTaskMetadataMismatch {
			task.Status = verificationTaskFailed
		} else {
			task.Status = verificationTaskCompleted
		}
		return nil
	}

	indexProblems, err := verifier.verifyIndexes(ctx, srcColl, dstColl, srcSpec.IDIndex, dstSpec.IDIndex)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to compare namespace %#q's indexes",
			srcNs,
		)
	}
	if indexProblems != nil {
		if specificationProblems == nil {
			// don't insert a failed collection unless we did not insert one above
			err = insertFailedCollection()
			if err != nil {
				return err
			}
		}
		task.FailedDocs = append(task.FailedDocs, indexProblems...)
		task.Status = verificationTaskMetadataMismatch
	}

	// We’ve confirmed that the collection metadata (including indices)
	// matches between soruce & destination. Now we can partition the collection.

	if task.Generation == 0 {
		partitions, shardKeys, docsCount, bytesCount, err := verifier.partitionAndInspectNamespace(ctx, srcNs)
		if err != nil {
			return errors.Wrapf(
				err,
				"failed to partition collection %#q",
				srcNs,
			)
		}
		verifier.logger.Debug().
			Int("workerNum", workerNum).
			Str("namespace", srcNs).
			Int("partitionsCount", len(partitions)).
			Msg("Divided collection into partitions.")

		task.SourceDocumentCount = docsCount
		task.SourceByteCount = bytesCount

		for _, partition := range partitions {
			_, err := verifier.InsertPartitionVerificationTask(ctx, partition, shardKeys, dstNs)
			if err != nil {
				return errors.Wrapf(
					err,
					"failed to insert a partition task for namespace %#q",
					srcNs,
				)
			}
		}
	}

	if task.Status == verificationTaskProcessing {
		task.Status = verificationTaskCompleted
	}

	return nil
}

func (verifier *Verifier) GetVerificationStatus(ctx context.Context) (*VerificationStatus, error) {
	taskCollection := verifier.verificationTaskCollection()
	generation, _ := verifier.getGeneration()

	aggregation := []bson.M{
		{
			"$match": bson.M{
				"type":       bson.M{"$ne": "primary"},
				"generation": generation,
			},
		},
		{
			"$group": bson.M{
				"_id":   "$status",
				"count": bson.M{"$sum": 1},
			},
		},
	}
	cursor, err := taskCollection.Aggregate(ctx, aggregation)
	if err != nil {
		return nil, err
	}
	var results []bson.Raw
	err = cursor.All(ctx, &results)
	if err != nil {
		return nil, err
	}

	verificationStatus := VerificationStatus{}

	for _, result := range results {
		status := result.Lookup("_id").String()
		// Status is returned with quotes around it so remove those
		status = status[1 : len(status)-1]
		count := int(result.Lookup("count").Int32())
		verificationStatus.TotalTasks += int(count)
		switch verificationTaskStatus(status) {
		case verificationTaskAdded:
			verificationStatus.AddedTasks = count
		case verificationTaskProcessing:
			verificationStatus.ProcessingTasks = count
		case verificationTaskFailed:
			verificationStatus.FailedTasks = count
		case verificationTaskMetadataMismatch:
			verificationStatus.MetadataMismatchTasks = count
		case verificationTaskCompleted:
			verificationStatus.CompletedTasks = count
		default:
			verifier.logger.Info().Msgf("Unknown task status %s", status)
		}
	}

	return &verificationStatus, nil
}

func (verifier *Verifier) doIfForceReadConcernMajority(f func()) {
	switch verifier.readConcernSetting {
	case ReadConcernMajority:
		f()
	case ReadConcernIgnore:
		// Do nothing.
	default:
		// invariant
		panic("Unknown read concern setting: " + verifier.readConcernSetting)
	}
}

func (verifier *Verifier) verificationDatabase() *mongo.Database {
	db := verifier.metaClient.Database(verifier.metaDBName)
	if db.WriteConcern().W != "majority" {
		verifier.logger.Fatal().Msgf("Verification metadata is not using write concern majority: %+v", db.WriteConcern())
	}

	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Verification metadata is not using read concern majority: %+v", db.ReadConcern())
		}
	})

	return db
}

func (verifier *Verifier) verificationTaskCollection() *mongo.Collection {
	return verifier.verificationDatabase().Collection(verificationTasksCollection)
}

func (verifier *Verifier) srcClientDatabase(dbName string) *mongo.Database {
	db := verifier.srcClient.Database(dbName)
	// No need to check the write concern because we do not write to the source database.
	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
		}
	})
	return db
}

func (verifier *Verifier) dstClientDatabase(dbName string) *mongo.Database {
	db := verifier.dstClient.Database(dbName)
	// No need to check the write concern because we do not write to the target database.
	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
		}
	})
	return db
}

func (verifier *Verifier) srcClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		dbName, collName := SplitNamespace(task.QueryFilter.Namespace)
		return verifier.srcClientDatabase(dbName).Collection(collName)
	}
	return nil
}

func (verifier *Verifier) dstClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		if task.QueryFilter.To != "" {
			return verifier.dstClientCollectionByNameSpace(task.QueryFilter.To)
		} else {
			return verifier.dstClientCollectionByNameSpace(task.QueryFilter.Namespace)
		}
	}
	return nil
}

func (verifier *Verifier) dstClientCollectionByNameSpace(namespace string) *mongo.Collection {
	dbName, collName := SplitNamespace(namespace)
	return verifier.dstClientDatabase(dbName).Collection(collName)
}

func (verifier *Verifier) StartServer() error {
	server := NewWebServer(verifier.port, verifier, verifier.logger)
	return server.Run(context.Background())
}

func (verifier *Verifier) GetProgress(ctx context.Context) (Progress, error) {
	status, err := verifier.GetVerificationStatus(ctx)
	if err != nil {
		return Progress{Error: err}, err
	}
	return Progress{
		Phase:      verifier.phase,
		Generation: verifier.generation,
		Status:     status,
	}, nil
}

// Returned boolean indicates that namespaces are cached, and
// whatever needs them can proceed.
func (verifier *Verifier) ensureNamespaces(ctx context.Context) bool {

	// cache namespace
	if len(verifier.srcNamespaces) == 0 {
		namespaces, err := verifier.getNamespaces(ctx, SrcNamespaceField)
		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to learn source namespaces")
			return false
		}

		verifier.srcNamespaces = namespaces

		// if still no namespace, nothing to print!
		if len(verifier.srcNamespaces) == 0 {
			verifier.logger.Info().Msg("Source contains no namespaces.")
			return false
		}
	}

	if len(verifier.dstNamespaces) == 0 {
		namespaces, err := verifier.getNamespaces(ctx, DstNamespaceField)

		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to learn destination namespaces")
			return false
		}

		verifier.dstNamespaces = namespaces

		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
	}

	return true
}

const (
	dividerChar  = "\u2501" // horizontal dash
	dividerWidth = 76
)

func startReport() *strings.Builder {
	strBuilder := &strings.Builder{}

	timestampAndSpace := time.Now().Format(timeFormat) + " "
	divider := strings.Repeat(dividerChar, dividerWidth-len(timestampAndSpace))

	strBuilder.WriteString(fmt.Sprintf(
		"\n%s%s\n\n", timestampAndSpace, divider,
	))

	return strBuilder
}

func (verifier *Verifier) PrintVerificationSummary(ctx context.Context, genstatus GenerationStatus) {
	if !verifier.ensureNamespaces(ctx) {
		return
	}

	strBuilder := startReport()

	generation, _ := verifier.getGeneration()

	var header string
	switch genstatus {
	case Gen0MetadataAnalysisComplete:
		header = fmt.Sprintf("Metadata Analysis Complete (Generation #%d)", generation)
	case GenerationInProgress:
		header = fmt.Sprintf("Progress Report (Generation #%d)", generation)
	case GenerationComplete:
		header = fmt.Sprintf("End of Generation #%d", generation)
	default:
		panic("Bad generation status: " + genstatus)
	}

	strBuilder.WriteString(header + "\n\n")

	strBuilder.WriteString(fmt.Sprintf(
		"Generation time elapsed: %s\n",
		reportutils.DurationToHMS(time.Since(verifier.generationStartTime)),
	))

	metadataMismatches, anyCollsIncomplete, err := verifier.reportCollectionMetadataMismatches(ctx, strBuilder)
	if err != nil {
		verifier.logger.Err(err).Msgf("Failed to report collection metadata mismatches")
		return
	}

	var hasTasks bool
	switch genstatus {
	case Gen0MetadataAnalysisComplete:
		fallthrough
	case GenerationInProgress:
		hasTasks, err = verifier.printNamespaceStatistics(ctx, strBuilder)
	case GenerationComplete:
		hasTasks, err = verifier.printEndOfGenerationStatistics(ctx, strBuilder)
	default:
		panic("Bad generation status: " + genstatus)
	}

	if err != nil {
		verifier.logger.Err(err).Msgf("Failed to report per-namespace statistics")
	}

	verifier.printChangeEventStatistics(strBuilder)

	var statusLine string

	if hasTasks {
		docMismatches, anyPartitionsIncomplete, err := verifier.reportDocumentMismatches(ctx, strBuilder)
		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to report document mismatches")
			return
		}

		if metadataMismatches || docMismatches {
			verifier.printMismatchInvestigationNotes(strBuilder)

			statusLine = fmt.Sprintf(notOkSymbol + " Mismatches found.")
		} else if anyCollsIncomplete || anyPartitionsIncomplete {
			statusLine = fmt.Sprintf(infoSymbol + " No mismatches found yet, but verification is still in progress.")
		} else {
			statusLine = fmt.Sprintf(okSymbol + " No mismatches found. Source & destination completely match!")
		}
	} else {
		switch genstatus {
		case GenerationInProgress:
			statusLine = "This generation has nothing to compare."
		case GenerationComplete:
			statusLine = "This generation had nothing to compare."
		default:
			panic("Bad generation status: " + genstatus)
		}

		statusLine = okSymbol + " " + statusLine
	}

	strBuilder.WriteString("\n" + statusLine + "\n")

	verifier.writeStringBuilder(strBuilder)
}

func (verifier *Verifier) writeStringBuilder(builder *strings.Builder) {
	_, err := verifier.writer.Write([]byte(builder.String()))
	if err != nil {
		panic("Failed to write to string builder: " + err.Error())
	}
}

func (verifier *Verifier) getNamespaces(ctx context.Context, fieldName string) ([]string, error) {
	var namespaces []string
	ret, err := verifier.verificationTaskCollection().Distinct(ctx, fieldName, bson.D{})
	if err != nil {
		return nil, err
	}
	for _, v := range ret {
		namespaces = append(namespaces, v.(string))
	}
	return namespaces, nil
}
