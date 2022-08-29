package verifier

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	//TODO: add comments for each of these so the warnings will stop :)
	Missing           = "Missing"
	Mismatch          = "Mismatch"
	ClusterTarget     = "dstClient"
	ClusterSource     = "srcClient"
	SrcNamespaceField = "query_filter.namespace"
	DstNamespaceField = "query_filter.to"
	NumWorkers        = 10
	refetch           = "TODO_CHANGE_ME_REFETCH"
)

// Verifier is the main state for the migration verifier
type Verifier struct {
	metaClient *mongo.Client
	srcClient  *mongo.Client
	dstClient  *mongo.Client
	numWorkers int

	comparisonRetryDelayMillis time.Duration
	workerSleepDelayMillis     time.Duration
	ignoreBSONFieldOrder       bool

	logger *zerolog.Logger

	srcNamespaces []string
	dstNamespaces []string
	metaDBName    string
}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	totalTasks      int
	addedTasks      int
	processingTasks int
	failedTasks     int
	completedTasks  int
	retryTasks      int
}

// VerificationResult holds the Verification Results
type VerificationResult struct {
	ID        interface{}
	Field     interface{}
	Type      interface{}
	Details   interface{}
	Cluster   interface{}
	NameSpace interface{}
}

// NewVerifier creates a new Verifier
func NewVerifier() *Verifier {
	return &Verifier{
		numWorkers: NumWorkers,
	}
}

func (verifier *Verifier) getClientOpts(uri string) *options.ClientOptions {
	appName := "Migration Verifier"
	opts := &options.ClientOptions{
		AppName: &appName,
	}
	opts.ApplyURI(uri)
	return opts
}

func (verifier *Verifier) GetLogger() *zerolog.Logger {
	return verifier.logger
}

func (verifier *Verifier) SetMetaURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.metaClient, err = mongo.Connect(ctx, opts)
	return err
}

func (verifier *Verifier) SetSrcURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.srcClient, err = mongo.Connect(ctx, opts)
	return err
}

func (verifier *Verifier) SetDstURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.dstClient, err = mongo.Connect(ctx, opts)
	return err
}

func (verifier *Verifier) SetNumWorkers(arg int) {
	verifier.numWorkers = arg
}

func (verifier *Verifier) SetComparisonRetryDelayMillis(arg time.Duration) {
	verifier.comparisonRetryDelayMillis = arg
}

func (verifier *Verifier) SetWorkerSleepDelayMillis(arg time.Duration) {
	verifier.workerSleepDelayMillis = arg
}

func (verifier *Verifier) SetLogger(logPath string) (*os.File, *bufio.Writer, error) {
	var file *os.File
	var writer *bufio.Writer = nil
	if logPath == "stderr" {
		l := zerolog.New(os.Stderr).With().Timestamp().Logger()
		verifier.logger = &l
		return file, writer, nil
	}
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		mkdirErr := os.MkdirAll(path.Dir(logPath), 0770)
		if mkdirErr != nil {
			return nil, nil, mkdirErr
		}
	}
	file, err := os.Create(logPath)
	if err != nil {
		return nil, nil, err
	}
	writer = bufio.NewWriter(file)
	l := zerolog.New(writer).With().Timestamp().Logger()
	verifier.logger = &l
	return file, writer, nil
}

func (verifier *Verifier) SetSrcNamespaces(arg []string) {
	verifier.srcNamespaces = arg
}

func (verifier *Verifier) SetDstNamespaces(arg []string) {
	verifier.dstNamespaces = arg
}

func (verifier *Verifier) SetMetaDBName(arg string) {
	verifier.metaDBName = arg
}

func (verifier *Verifier) SetIgnoreBSONFieldOrder(arg bool) {
	verifier.ignoreBSONFieldOrder = arg
}

// DocumentStats gets various status (TODO clarify)
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

func (verifier *Verifier) getDocuments(collection *mongo.Collection, task *VerificationTask) (map[interface{}]bson.Raw, error) {
	var findOptions bson.D
	if len(task.FailedIDs) > 0 {
		filter := bson.D{
			bson.E{
				Key:   "_id",
				Value: bson.M{"$in": task.FailedIDs},
			},
		}
		findOptions = bson.D{
			bson.E{"filter", filter},
		}
	} else if len(task.Ids) > 0 {
		filter := bson.D{
			bson.E{
				Key:   "_id",
				Value: bson.M{"$in": task.Ids},
			},
		}
		findOptions = bson.D{
			bson.E{"filter", filter},
		}
	} else {
		findOptions = task.QueryFilter.Partition.GetFindOptions()
	}
	findCmd := append(bson.D{{"find", collection.Name()}}, findOptions...)
	verifier.logger.Debug().Msgf("getDocuments findCmd: %s", findCmd)

	ctx := context.Background()

	cursor, err := collection.Database().RunCommandCursor(ctx, findCmd)
	if err != nil {
		return nil, err
	}
	documentMap := make(map[interface{}]bson.Raw)
	for cursor.Next(ctx) {
		var rawDoc bson.Raw
		err := cursor.Decode(&rawDoc)
		if err != nil {
			return nil, err
		}
		id := rawDoc.Lookup("_id").String()

		documentMap[id] = rawDoc
	}

	return documentMap, nil
}

func (verifier *Verifier) FetchAndCompareDocuments(task *VerificationTask) ([]VerificationResult, error) {
	srcClientMap, dstClientMap, err := verifier.fetchDocuments(task)
	if err != nil {
		return nil, err
	}
	return verifier.compareDocuments(srcClientMap, dstClientMap, task.QueryFilter.Namespace)
}

// This is split out to allow unit testing of fetching separate from comparison.
func (verifier *Verifier) fetchDocuments(task *VerificationTask) (map[interface{}]bson.Raw, map[interface{}]bson.Raw, error) {
	srcClientMap, err := verifier.getDocuments(verifier.srcClientCollection(task), task)
	if err != nil {
		return nil, nil, err
	}

	var dstClientMap map[interface{}]bson.Raw
	if task.QueryFilter.To != "" {
		dstClientMap, err = verifier.getDocuments(verifier.dstClientCollectionByNameSpace(task.QueryFilter.To), task)
	} else {
		dstClientMap, err = verifier.getDocuments(verifier.dstClientCollection(task), task)
	}

	if err != nil {
		return nil, nil, err
	}
	return srcClientMap, dstClientMap, nil
}

func (verifier *Verifier) compareDocuments(srcClientMap, dstClientMap map[interface{}]bson.Raw, namespace string) ([]VerificationResult, error) {
	var mismatchedIds []VerificationResult
	for id, srcClientDoc := range srcClientMap {
		dstClientDoc, ok := dstClientMap[id]
		if !ok {
			//verifier.logger.Info().Msg("Document %+v missing on dstClient!", id)
			mismatchedIds = append(mismatchedIds, VerificationResult{
				ID:        srcClientDoc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: namespace,
			})
			continue
		}

		misMatch, err := verifier.compareOneDocument(srcClientDoc, dstClientDoc, namespace)
		if len(misMatch) > 0 || err != nil {
			mismatchedIds = append(mismatchedIds, misMatch...)
		}
	}

	if len(srcClientMap) != len(dstClientMap) {
		for id, dstClientDoc := range dstClientMap {
			_, ok := srcClientMap[id]
			if !ok {
				//verifier.logger.Info().Msg("Document %+v missing on srcClient!", id)
				mismatchedIds = append(mismatchedIds, VerificationResult{
					ID:        dstClientDoc.Lookup("_id"),
					Details:   Missing,
					Cluster:   ClusterSource,
					NameSpace: namespace,
				})
			}
		}
	}

	return mismatchedIds, nil
}

func (verifier *Verifier) compareOneDocument(srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		return nil, nil
	}
	//verifier.logger.Info().Msg("Byte comparison failed for id %s, falling back to field comparison", id)

	id := srcClientDoc.Lookup("_id")
	if verifier.ignoreBSONFieldOrder {
		mismatch, err := BsonUnorderedCompareRawDocumentWithDetails(srcClientDoc, dstClientDoc)
		if err != nil {
			return nil, err
		}
		if mismatch == nil {
			return nil, nil
		}
		var results []VerificationResult
		for _, field := range mismatch.missingFieldOnSrc {
			results = append(results, VerificationResult{ID: id, Field: field, Details: Missing, Cluster: ClusterSource, NameSpace: namespace})
		}

		for _, field := range mismatch.missingFieldOnDst {
			results = append(results, VerificationResult{ID: id, Field: field, Details: Missing, Cluster: ClusterTarget, NameSpace: namespace})
		}

		for _, field := range mismatch.fieldContentsDiffer {
			srcClientValue := srcClientDoc.Lookup(field)
			dstClientValue := dstClientDoc.Lookup(field)
			details := fmt.Sprintf("Document %s failed comparison on field %s between srcClient (Type: %s) and dstClient (Type: %s)", id, field, srcClientValue.Type, dstClientValue.Type)
			results = append(results, VerificationResult{ID: id, Field: field, Details: Mismatch + " : " + details, Cluster: ClusterTarget, NameSpace: namespace})
		}
		return results, nil
	}
	// If we're respecting field order we have just done a binary compare so don't know the mismatching fields.
	return []VerificationResult{{
		ID:        dstClientDoc.Lookup("_id"),
		Details:   Mismatch,
		Cluster:   ClusterTarget,
		NameSpace: namespace,
	}}, nil
}

func (verifier *Verifier) ProcessVerifyTask(workerNum int, task *VerificationTask) {

	var mismatchIDs []interface{}

	mismatches, err := verifier.FetchAndCompareDocuments(task)
	for _, v := range mismatches {
		mismatchIDs = append(mismatchIDs, v.ID)
	}

	task.Attempts++
	if len(mismatches) > 0 {
		task.FailedDocs = mismatches
	}
	if err != nil {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Error comparing docs: %+v", workerNum, err)
	} else if len(mismatches) == 0 {
		if len(task.FailedIDs) > 0 {
			verifier.logger.Info().Msgf("Previously failed document IDs now match! Marking task as complete! Document IDs: %+v", task.FailedIDs)
		}
		task.Status = verificationTaskCompleted
	} else if task.Attempts < verificationTaskMaxRetries {
		task.Status = verificationTasksRetry
		task.FailedIDs = mismatchIDs
		task.RetryAfter = time.Now().Add(verifier.comparisonRetryDelayMillis * time.Millisecond)
		verifier.logger.Error().Msgf("[Worker %d] Verification Task %+v failed attempt %d/%d, retrying", workerNum, task.PrimaryKey, task.Attempts, verificationTaskMaxRetries)
	} else {
		task.Status = verificationTaskFailed
		task.FailedIDs = mismatchIDs
		verifier.logger.Error().Msgf("[Worker %d] Verification Task %+v out of retries, failing", workerNum, task.PrimaryKey)
		verifier.AddRefetchTask(task)
	}

	err = verifier.UpdateVerificationTask(task)
	if err != nil {
		verifier.logger.Error().Msgf("Failed updating verification status: %v", err)
	}
}

func (verifier *Verifier) getCollectionPartitions(ctx context.Context, namespace string) ([]*partitions.Partition, error) {
	retryer := retry.New(retry.DefaultDurationLimit)
	logger := logger.NewLogger(verifier.logger, logger.DefaultLogWriter)
	dbName, collName := SplitNamespace(namespace)
	namespaceAndUUID, err := uuidutil.GetCollectionNamespaceAndUUID(ctx, logger, retryer,
		verifier.srcClient.Database(dbName), collName)
	if err != nil {
		return nil, err
	}
	// The partitioner doles out ranges to replicators; we don't use that functionality so we just pass
	// one "replicator".
	replicator1 := partitions.Replicator{ID: "verifier"}
	replicators := []partitions.Replicator{replicator1}
	partitionList, err := partitions.PartitionCollection(ctx, namespaceAndUUID, retryer, verifier.srcClient, replicators, logger)
	if err != nil {
		return nil, err
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
	return partitionList, nil
}

func (verifier *Verifier) ProcessCollectionVerificationTask(ctx context.Context, workerNum int, task *VerificationTask) {
	// TODO handle metadata and indexes.
	srcNs := task.QueryFilter.Namespace
	dstNs := task.QueryFilter.To
	partitions, err := verifier.getCollectionPartitions(ctx, srcNs)
	if err != nil {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Error partitioning collection: %+v", workerNum, err)
	} else {
		task.Status = verificationTaskCompleted
		verifier.logger.Info().Msgf("[Worker %d] split collection info %d partitions", workerNum, len(partitions))
		for _, partition := range partitions {
			_, err := InsertPartitionVerificationTask(partition, dstNs, verifier.verificationTaskCollection())
			if err != nil {
				task.Status = verificationTaskFailed
				verifier.logger.Error().Msgf("[Worker %d] Error inserting verifier tasks: %+v", workerNum, err)
			}
		}
	}

	err = verifier.UpdateVerificationTask(task)
	if err != nil {
		verifier.logger.Error().Msgf("Failed updating verification status: %v", err)
	}
}
func (verifier *Verifier) AddRefetchTask(task *VerificationTask) {
	srcNamespace := task.QueryFilter.Namespace
	dstNamespace := srcNamespace
	if task.QueryFilter.To != "" {
		dstNamespace = task.QueryFilter.To
	}
	for _, id := range task.FailedIDs {
		model := Refetch{ID: id, SrcNamespace: srcNamespace, DestNamespace: dstNamespace, Status: Unprocessed}
		_, err := verifier.refetchCollection().InsertOne(context.Background(), model)
		if err != nil {
			verifier.logger.Error().Msgf("Error saving refetch document for id %s - %+v", id, err)
			// TODO: see if we need this commented out message
			// } else {
			// verifier.logger.Info().Msg("Saved refetch document for id %s", id)
		}
	}
}

func (verifier *Verifier) Work(workerNum int, ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	verifier.logger.Info().Msgf("[Worker %d] Started", workerNum)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			task, err := verifier.FindNextVerifyTaskAndUpdate()
			if errors.Is(err, mongo.ErrNoDocuments) {
				verifier.logger.Info().Msgf("[Worker %d] No tasks found, sleeping...", workerNum)
				time.Sleep(verifier.workerSleepDelayMillis * time.Millisecond)
				continue
			} else if err != nil {
				panic(err)
			}
			if task.Type == verificationTaskVerifyCollection {
				verifier.ProcessCollectionVerificationTask(ctx, workerNum, task)
			} else {
				verifier.ProcessVerifyTask(workerNum, task)
			}
		}
	}
}

func (verifier *Verifier) GetVerificationStatus() (*VerificationStatus, error) {
	ctx := context.Background()
	verificationStatus := VerificationStatus{}
	taskCollection := verifier.verificationTaskCollection()

	aggregation := []bson.M{
		{
			"$match": bson.M{
				"type": bson.M{"$ne": "primary"},
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

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Status", "Count"})

	for _, result := range results {
		status := result.Lookup("_id").String()
		// Status is returned with quotes around it so remove those
		status = status[1 : len(status)-1]
		count := int(result.Lookup("count").Int32())
		verificationStatus.totalTasks += int(count)
		switch status {
		case verificationTaskAdded:
			verificationStatus.addedTasks = count
		case verificationTaskProcessing:
			verificationStatus.processingTasks = count
		case verificationTaskFailed:
			verificationStatus.failedTasks = count
		case verificationTaskCompleted:
			verificationStatus.completedTasks = count
		case verificationTasksRetry:
			verificationStatus.retryTasks = count
		default:
			verifier.logger.Info().Msgf("Unknown task status %s", status)
		}

		table.Append([]string{status, strconv.Itoa(count)})
	}
	fmt.Println("\nVerify Tasks:")
	table.Render()
	fmt.Println()

	return &verificationStatus, nil
}

func (verifier *Verifier) verificationTaskCollection() *mongo.Collection {
	return verifier.metaClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
}

func (verifier *Verifier) verificationRangeCollection() *mongo.Collection {
	return verifier.metaClient.Database(verifier.metaDBName).Collection(verificationRangeCollection)
}

func (verifier *Verifier) refetchCollection() *mongo.Collection {
	return verifier.metaClient.Database(verifier.metaDBName).Collection(refetch)
}

func (verifier *Verifier) srcClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		dbName, collName := SplitNamespace(task.QueryFilter.Namespace)
		return verifier.srcClient.Database(dbName).Collection(collName)
	}
	return nil
}

func (verifier *Verifier) dstClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		dbName, collName := SplitNamespace(task.QueryFilter.Namespace)
		return verifier.dstClient.Database(dbName).Collection(collName)
	}
	return nil
}

func (verifier *Verifier) dstClientCollectionByNameSpace(namespace string) *mongo.Collection {
	dbName, collName := SplitNamespace(namespace)
	return verifier.dstClient.Database(dbName).Collection(collName)
}

func (verifier *Verifier) Verify() error {
	var err error

	// Log out the verification status when initially booting up so it's easy to see the current state
	verificationStatus, err := verifier.GetVerificationStatus()
	if err != nil {
		verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
	} else {
		verifier.logger.Info().Msgf("Initial verification status: %+v", verificationStatus)
	}

	err = verifier.CreateInitialTasks()
	if err != nil {
		return err
	}

	verifier.logger.Info().Msgf("Starting %d verification workers", verifier.numWorkers)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	for i := 0; i < verifier.numWorkers; i++ {
		wg.Add(1)
		go verifier.Work(i, ctx, &wg)
		time.Sleep(10 * time.Millisecond)
	}

	waitForTaskCreation := 0
	for {

		verificationStatus, err := verifier.GetVerificationStatus()
		if err != nil {
			verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
		}

		if waitForTaskCreation%2 == 0 {
			verifier.PrintVerificationSummary(ctx)
		}

		//wait for task to be created, if none of the tasks found.
		if verificationStatus.addedTasks > 0 || verificationStatus.processingTasks > 0 || verificationStatus.retryTasks > 0 {
			waitForTaskCreation++
			time.Sleep(15 * time.Second)
		} else {
			verifier.PrintVerificationSummary(ctx)
			verifier.logger.Info().Msg("Verification tasks complete")
			cancel()
			wg.Wait()
			break
		}
	}
	return nil
}

func (verifier *Verifier) CreateInitialTasks() error {
	// If we don't know the src namespaces, we're definitely not the primary task.
	if len(verifier.srcNamespaces) == 0 {
		return nil
	}
	if len(verifier.dstNamespaces) == 0 {
		verifier.dstNamespaces = verifier.srcNamespaces
	}
	if len(verifier.srcNamespaces) != len(verifier.dstNamespaces) {
		err := errors.Errorf("Different number of source and destination namespaces")
		verifier.logger.Error().Msgf("%s", err)
		return err
	}
	isPrimary, err := verifier.CheckIsPrimary()
	if err != nil {
		return err
	}
	if !isPrimary {
		return nil
	}
	for i, src := range verifier.srcNamespaces {
		dst := verifier.dstNamespaces[i]
		verifier.logger.Info().Msgf("Adding task for %s -> %s", src, dst)
		_, err := InsertCollectionVerificationTask(src, dst, verifier.verificationTaskCollection())
		if err != nil {
			verifier.logger.Error().Msgf("Failed to insert collection verification task: %s", err)
			return err
		}
	}

	err = verifier.UpdatePrimaryTaskComplete()
	if err != nil {
		return err
	}
	return nil
}

func FetchTasks(ctx context.Context, coll *mongo.Collection) []VerificationTask {

	var failedTasks []VerificationTask
	status := []string{verificationTasksRetry, verificationTaskFailed}
	cur, err := coll.Find(ctx, bson.D{bson.E{Key: "status", Value: bson.M{"$in": status}}})
	if err != nil {
		return failedTasks
	}

	err = cur.All(ctx, &failedTasks)
	if err != nil {
		return failedTasks
	}

	return failedTasks
}

func (verifier *Verifier) PrintVerificationSummary(ctx context.Context) {

	// cache namespace
	if len(verifier.srcNamespaces) == 0 {
		verifier.srcNamespaces = verifier.getNamespaces(ctx, SrcNamespaceField)
		// if still no namespace, nothing to print!
		if len(verifier.srcNamespaces) == 0 {
			verifier.logger.Info().Msg("Unable to find the namespace to display DB stats.")
			return
		}
	}

	if len(verifier.dstNamespaces) == 0 {
		verifier.dstNamespaces = verifier.getNamespaces(ctx, DstNamespaceField)
		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Src Count", "Src DB", "Src Coll", "Dst Count", "Dst DB", "Dst Coll"})

	table2 := tablewriter.NewWriter(os.Stdout)
	table2.SetHeader([]string{"Src Count", "Src DB", "Src Coll", "Dst Count", "Dst DB", "Dst Coll"})

	diffCounts := 0
	matchingCounts := 0

	for i, n := range verifier.srcNamespaces {
		srcDb, srcColl := SplitNamespace(n)
		if srcDb != "" {
			srcEst, _ := verifier.srcClient.Database(srcDb).Collection(srcColl).EstimatedDocumentCount(ctx)

			n2 := verifier.dstNamespaces[i]
			dstDb, dstColl := SplitNamespace(n2)
			if dstDb != "" {
				dstEst, _ := verifier.dstClient.Database(dstDb).Collection(dstColl).EstimatedDocumentCount(ctx)

				table.Append([]string{strconv.FormatInt(srcEst, 10), srcDb, srcColl, strconv.FormatInt(dstEst, 10), dstDb, dstColl})
				if srcEst != dstEst {
					table2.Append([]string{strconv.FormatInt(srcEst, 10), srcDb, srcColl, strconv.FormatInt(dstEst, 10), dstDb, dstColl})
					diffCounts++
				} else {
					matchingCounts++
				}
			}
		}
	}

	fmt.Printf("Collections with matching counts: %d\n\n", matchingCounts)

	if matchingCounts <= 25 {
		fmt.Println("\nSource / Target Comparison (All collections):")
		table.Render()
		fmt.Print("\n")
	}

	if diffCounts == 0 {
		fmt.Print("********** ALL COLLECTION COUNTS MATCH ! **********\n\n")
	} else {
		fmt.Println("\nSource / Target Comparison (Differing counts only):")
		table2.Render()
		fmt.Print("Differences in counts may be due to query filters\n\n\n")
	}

	failedTasks := FetchTasks(ctx, verifier.verificationTaskCollection())
	if len(failedTasks) == 0 {
		// Nothing to print
		return
	}

	table = tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Cluster", "Type", "Field", "Namespace", "Details"})

	for _, v := range failedTasks {
		for _, f := range v.FailedDocs {
			table.Append([]string{fmt.Sprintf("%v", f.ID), fmt.Sprintf("%v", f.Cluster), fmt.Sprintf("%v", f.Type), fmt.Sprintf("%v", f.Field), fmt.Sprintf("%v", f.NameSpace), fmt.Sprintf("%v", f.Details)})
		}
	}
	fmt.Println("Documents in tasks in failed or retry status:")
	table.Render()
	fmt.Println()
}

func (verifier Verifier) getNamespaces(ctx context.Context, fieldName string) []string {
	var namespaces []string
	ret, err := verifier.verificationTaskCollection().Distinct(ctx, fieldName, bson.D{})
	if err != nil {
		return namespaces
	}
	for _, v := range ret {
		namespaces = append(namespaces, v.(string))
	}
	return namespaces
}
