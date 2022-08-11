package verifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	//TODO: add comments for each of these so the warnings will stop :)
	MISSING           = "Missing"
	MISMATCH          = "Mismatch"
	CLUSTERTARGET     = "dstClient"
	CLUSTERSOURCE     = "srcClient"
	SRCNAMESPACEFIELD = "query_filter.namespace"
	DSTNAMESPACEFIELD = "query_filter.to"
	refetch           = "TODO_CHANGE_ME_REFETCH"
)

// Verifier is the main state for the migration verifier
type Verifier struct {
	metaClient *mongo.Client
	srcClient  *mongo.Client
	dstClient  *mongo.Client
	workers    int

	comparisonRetryDelayMillis time.Duration
	workerSleepDelayMillis     time.Duration

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
	return &Verifier{}
}

func (v *Verifier) handleArgs(args []string) error {
	if len(args) != 3 {
		panic("usage ./migration_verifier srcClient destination")
	}
	// TODO: handle delays, log setup, and namespaces with command line flags
	// TODO: decide readPref
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Print("hello world")
	var err error
	v.srcClient, err = mongo.Connect(context.TODO())
	if err != nil {
		return err
	}
	if err := v.srcClient.Ping(context.TODO(), nil); err != nil {
		return err
	}
	v.comparisonRetryDelayMillis = 30_000
	v.workerSleepDelayMillis = 5_000
	v.metaDBName = "TODO_CHANGE_ME"
	v.workers = 10 // TODO set reasonable number
	return nil
}

// Run runs the migration_verifier
func (verifier *Verifier) Run(args []string) {
	err := verifier.handleArgs(args)
	if err != nil {
		panic(err)
	}
	doneChan := make(chan bool)
	go verifier.Verify(doneChan)

	<-doneChan
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

func getDocuments(collection *mongo.Collection, task *VerificationTask) (map[interface{}]bson.Raw, error) {
	var filter bson.D
	if len(task.FailedIDs) > 0 {
		filter = bson.D{
			bson.E{
				Key:   "_id",
				Value: bson.M{"$in": task.FailedIDs},
			},
		}
	} else {
		filter = bson.D{
			bson.E{
				Key:   "_id",
				Value: bson.M{"$in": task.Ids},
			},
		}
	}
	ctx := context.Background()

	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	documentMap := make(map[interface{}]bson.Raw)
	for cursor.Next(ctx) {
		var rawDoc bson.Raw
		cursor.Decode(&rawDoc)
		id := rawDoc.Lookup("_id").String()

		documentMap[id] = rawDoc
	}

	return documentMap, nil
}

func (verifier *Verifier) FetchAndCompareDocuments(task *VerificationTask) ([]VerificationResult, error) {
	var err error

	srcClientMap, err := getDocuments(verifier.srcClientCollection(task), task)
	if err != nil {
		return nil, err
	}

	var dstClientMap map[interface{}]bson.Raw
	if task.QueryFilter.To != "" {
		dstClientMap, err = getDocuments(verifier.dstClientCollectionByNameSpace(task.QueryFilter.To), task)
	} else {
		dstClientMap, err = getDocuments(verifier.dstClientCollection(task), task)
	}
	if err != nil {
		return nil, err
	}
	return verifier.compareDocuments(srcClientMap, dstClientMap, task.QueryFilter.Namespace)
}

func (verifier *Verifier) compareDocuments(srcClientMap, dstClientMap map[interface{}]bson.Raw, namespace string) ([]VerificationResult, error) {
	var mismatchedIds []VerificationResult
	for id, srcClientDoc := range srcClientMap {
		dstClientDoc, ok := dstClientMap[id]
		if !ok {
			//verifier.logger.Info().Msg("Document %+v missing on dstClient!", id)
			mismatchedIds = append(mismatchedIds, VerificationResult{
				ID:        srcClientDoc.Lookup("_id"),
				Details:   MISSING,
				Cluster:   CLUSTERTARGET,
				NameSpace: namespace,
			})
			continue
		}
		match := bytes.Equal(srcClientDoc, dstClientDoc)
		if match {
			continue
		}
		//verifier.logger.Info().Msg("Byte comparison failed for id %s, falling back to field comparison", id)

		manualMatch, err := verifier.manuallyCompareDocumentFields(srcClientDoc.Lookup("_id"), srcClientDoc, dstClientDoc, namespace)
		if len(manualMatch) > 0 || err != nil {
			mismatchedIds = append(mismatchedIds, manualMatch...)
		}
	}

	if len(srcClientMap) != len(dstClientMap) {
		for id, dstClientDoc := range dstClientMap {
			_, ok := srcClientMap[id]
			if !ok {
				//verifier.logger.Info().Msg("Document %+v missing on srcClient!", id)
				mismatchedIds = append(mismatchedIds, VerificationResult{
					ID:      dstClientDoc.Lookup("_id"),
					Details: MISSING,
					Cluster: CLUSTERSOURCE,
				})
			}
		}
	}

	return mismatchedIds, nil
}

func (verifier *Verifier) manuallyCompareDocumentFields(id interface{}, srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {

	var results []VerificationResult

	srcClientRaw, err := srcClientDoc.Elements()
	if err != nil {
		verifier.logger.Error().Msgf("Error parsing srcClient document id %s - %+v", id, err)
		return results, err
	}
	dstClientRaw, err := dstClientDoc.Elements()
	if err != nil {
		verifier.logger.Error().Msgf("Error parsing dstClient document id %s - %+v", id, err)
		return results, err
	}

	srcClientMap := map[string]bson.RawValue{}
	dstClientMap := map[string]bson.RawValue{}
	allKeys := map[string]bool{}
	for _, v := range srcClientRaw {
		key := v.Key()
		srcClientMap[key] = v.Value()
		allKeys[key] = true
	}
	for _, v := range dstClientRaw {
		key := v.Key()
		dstClientMap[key] = v.Value()
		allKeys[key] = true
	}

	for key := range allKeys {
		srcClientValue, ok := srcClientMap[key]
		if !ok {
			//verifier.logger.Info().Msg("Document %s is missing field %s on srcClient cluster!", id, key)
			results = append(results, VerificationResult{ID: id, Field: key, Details: MISSING, Cluster: CLUSTERSOURCE, NameSpace: namespace})
		}
		dstClientValue, ok := dstClientMap[key]
		if !ok {
			//verifier.logger.Info().Msg("Document %s is missing field %s on dstClient cluster!", id, key)
			results = append(results, VerificationResult{ID: id, Field: key, Details: MISSING, Cluster: CLUSTERTARGET, NameSpace: namespace})
		}

		if !reflect.DeepEqual(srcClientValue, dstClientValue) {
			details := fmt.Sprintf("Document %s failed comparison on field %s between srcClient (Type: %s) and dstClient (Type: %s)", id, key, srcClientValue.Type, dstClientValue.Type)
			//verifier.logger.Info(details)
			results = append(results, VerificationResult{ID: id, Field: key, Details: MISMATCH + " : " + details, Cluster: CLUSTERTARGET, NameSpace: namespace})
		}
	}

	return results, nil
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
		} else {
			//verifier.logger.Info().Msg("Saved refetch document for id %s", id)
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
			verifier.ProcessVerifyTask(workerNum, task)
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

func (verifier *Verifier) Verify(done <-chan bool) error {
	var err error

	// Log out the verification status when initially booting up so it's easy to see the current state
	verificationStatus, err := verifier.GetVerificationStatus()
	if err != nil {
		verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
	} else {
		verifier.logger.Info().Msgf("Initial verification status: %+v", verificationStatus)
	}

	verifier.logger.Info().Msgf("Starting %d verification workers", verifier.workers)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	for i := 0; i < verifier.workers; i++ {
		wg.Add(1)
		go verifier.Work(i, ctx, &wg)
		time.Sleep(10 * time.Millisecond)
	}
	ticker := time.NewTicker(60 * time.Second)
	processed := false
	for !processed {
		select {
		case <-done:
			processed = true
			break
		case <-ticker.C:
			verifier.PrintVerificationSummary(ctx)
		}
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
		verifier.srcNamespaces = verifier.getNamespaces(ctx, SRCNAMESPACEFIELD)
		// if still no namespace, nothing to print!
		if len(verifier.srcNamespaces) == 0 {
			verifier.logger.Info().Msg("Unable to find the namespace to display DB stats.")
			return
		}
	}

	if len(verifier.dstNamespaces) == 0 {
		verifier.dstNamespaces = verifier.getNamespaces(ctx, DSTNAMESPACEFIELD)
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
