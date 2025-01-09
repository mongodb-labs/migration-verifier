package verifier

// This file holds logic to prepare visual summaries of various aspects
// of the verification: how many mismatches, rates of processing,
// number of docs/namespaces/bytes, progress, etc.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/olekukonko/tablewriter"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"golang.org/x/exp/maps"
)

const changeEventsTableMaxSize = 10

// NOTE: Each of the following should print one trailing and one final
// newline.

// Returned booleans indicate:
//   - whether any mismatches were found
//   - whether any incomplete tasks were found
func (verifier *Verifier) reportCollectionMetadataMismatches(ctx context.Context, strBuilder *strings.Builder) (bool, bool, error) {
	generation, _ := verifier.getGeneration()

	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
		ctx,
		verifier.logger,
		verifier.verificationTaskCollection(),
		verificationTaskVerifyCollection,
		generation,
	)
	if err != nil {
		return false, false, err
	}

	anyAreIncomplete := len(incompleteTasks) > 0

	if len(failedTasks) != 0 {
		table := tablewriter.NewWriter(strBuilder)
		table.SetHeader([]string{"Index", "Cluster", "Field", "Namespace", "Details"})

		for _, v := range failedTasks {
			for _, f := range v.FailedDocs {
				table.Append([]string{fmt.Sprintf("%v", f.ID), fmt.Sprintf("%v", f.Cluster), fmt.Sprintf("%v", f.Field), fmt.Sprintf("%v", f.NameSpace), fmt.Sprintf("%v", f.Details)})
			}
		}
		strBuilder.WriteString("\nCollections/Indexes in failed or retry status:\n")
		table.Render()

		return true, anyAreIncomplete, nil
	}

	return false, anyAreIncomplete, nil
}

func (verifier *Verifier) reportDocumentMismatches(ctx context.Context, strBuilder *strings.Builder) (bool, bool, error) {
	generation, _ := verifier.getGeneration()

	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
		ctx,
		verifier.logger,
		verifier.verificationTaskCollection(),
		verificationTaskVerifyDocuments,
		generation,
	)

	if err != nil {
		return false, false, err
	}

	anyAreIncomplete := len(incompleteTasks) > 0

	if len(failedTasks) == 0 {

		// Nothing has failed/mismatched, so there’s nothing to print.
		return false, anyAreIncomplete, nil
	}

	strBuilder.WriteString("\n")

	// First present summaries of failures based on present/missing and differing content
	failureTypesTable := tablewriter.NewWriter(strBuilder)
	failureTypesTable.SetHeader([]string{"Failure Type", "Count"})

	contentMismatchCount := 0
	missingOrChangedCount := 0
	for _, task := range failedTasks {
		contentMismatchCount += len(task.FailedDocs)
		missingOrChangedCount += len(task.Ids)
	}

	failureTypesTable.Append([]string{"Documents With Differing Content", fmt.Sprintf("%v", contentMismatchCount)})
	failureTypesTable.Append([]string{"Missing or Changed Documents", fmt.Sprintf("%v", missingOrChangedCount)})
	strBuilder.WriteString("Failure summary:\n")
	failureTypesTable.Render()

	mismatchedDocsTable := tablewriter.NewWriter(strBuilder)
	mismatchedDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	mismatchedDocsTable.SetHeader([]string{"ID", "Cluster", "Field", "Namespace", "Details"})

	printAll := int64(contentMismatchCount) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTA:
	for _, task := range failedTasks {
		for _, f := range task.FailedDocs {
			if !printAll && mismatchedDocsTableRows >= verifier.failureDisplaySize {
				break OUTA
			}

			mismatchedDocsTableRows++
			mismatchedDocsTable.Append([]string{
				fmt.Sprintf("%v", f.ID),
				fmt.Sprintf("%v", f.Cluster),
				fmt.Sprintf("%v", f.Field),
				fmt.Sprintf("%v", f.NameSpace),
				fmt.Sprintf("%v", f.Details),
			})
		}
	}

	if mismatchedDocsTableRows > 0 {
		strBuilder.WriteString("\n")
		if printAll {
			strBuilder.WriteString("All documents in tasks in failed status due to differing content:\n")
		} else {
			strBuilder.WriteString(fmt.Sprintf("First %d documents in tasks in failed status due to differing content:\n", verifier.failureDisplaySize))
		}
		mismatchedDocsTable.Render()
	}

	missingOrChangedDocsTable := tablewriter.NewWriter(strBuilder)
	missingOrChangedDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	missingOrChangedDocsTable.SetHeader([]string{"Document ID", "Source Namespace", "Destination Namespace"})

	printAll = int64(missingOrChangedCount) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTB:
	for _, task := range failedTasks {
		for _, _id := range task.Ids {
			if !printAll && missingOrChangedDocsTableRows >= verifier.failureDisplaySize {
				break OUTB
			}

			missingOrChangedDocsTableRows++
			missingOrChangedDocsTable.Append([]string{
				fmt.Sprintf("%v", _id),
				fmt.Sprintf("%v", task.QueryFilter.Namespace),
				fmt.Sprintf("%v", task.QueryFilter.To),
			})
		}
	}

	if missingOrChangedDocsTableRows > 0 {
		strBuilder.WriteString("\n")
		if printAll {
			strBuilder.WriteString("All documents marked missing or changed:\n")
		} else {
			strBuilder.WriteString(fmt.Sprintf("First %d documents marked missing or changed:\n", verifier.failureDisplaySize))
		}
		missingOrChangedDocsTable.Render()
	}

	return true, anyAreIncomplete, nil
}

// Boolean returned indicates whether this generation has any tasks.
func (verifier *Verifier) printNamespaceStatistics(ctx context.Context, strBuilder *strings.Builder) (bool, error) {
	stats, err := verifier.GetNamespaceStatistics(ctx)
	if err != nil {
		return false, err
	}

	// The verifier sometimes ends up enqueueing an extra generation
	// with no tasks. For now we’ll quietly no-op when that happens.
	if len(stats) == 0 {
		return false, nil
	}

	var totalDocs, comparedDocs types.DocumentCount
	var totalBytes, comparedBytes types.ByteCount
	var totalNss, completedNss types.NamespaceCount

	for _, result := range stats {
		totalDocs += result.TotalDocs
		comparedDocs += result.DocsCompared
		totalBytes += result.TotalBytes
		comparedBytes += result.BytesCompared

		totalNss++
		if result.PartitionsDone > 0 {
			partitionsPending := result.PartitionsAdded + result.PartitionsProcessing
			if partitionsPending == 0 {
				completedNss++
			}
		}
	}

	strBuilder.WriteString("\n")

	strBuilder.WriteString(fmt.Sprintf(
		"Namespaces completed: %d of %d (%s%%)\n",
		completedNss, totalNss,
		reportutils.FmtPercent(completedNss, totalNss),
	))

	elapsed := time.Since(verifier.generationStartTime)

	docsPerSecond := float64(comparedDocs) / elapsed.Seconds()
	bytesPerSecond := float64(comparedBytes) / elapsed.Seconds()
	perSecondDataUnit := reportutils.FindBestUnit(bytesPerSecond)

	if totalDocs > 0 {
		strBuilder.WriteString(fmt.Sprintf(
			"Total source documents compared: %d of %d (%s%%, %s/sec)\n",
			comparedDocs,
			totalDocs,
			reportutils.FmtPercent(comparedDocs, totalDocs),
			reportutils.FmtFloat(docsPerSecond),
		))
	} else {
		strBuilder.WriteString(fmt.Sprintf(
			"Total source documents compared: %d (%s/sec)\n",
			comparedDocs,
			reportutils.FmtFloat(docsPerSecond),
		))
	}

	if totalBytes > 0 {
		dataUnit := reportutils.FindBestUnit(totalBytes)

		strBuilder.WriteString(fmt.Sprintf(
			"Total size of those documents: %s of %s %s (%s%%, %s %s/sec)\n",
			reportutils.BytesToUnit(comparedBytes, dataUnit),
			reportutils.BytesToUnit(totalBytes, dataUnit),
			dataUnit,
			reportutils.FmtPercent(comparedBytes, totalBytes),
			reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
			perSecondDataUnit,
		))
	} else {
		dataUnit := reportutils.FindBestUnit(comparedBytes)

		strBuilder.WriteString(fmt.Sprintf(
			"Total size of those documents: %s %s (%s %s/sec)\n",
			reportutils.BytesToUnit(comparedBytes, dataUnit),
			dataUnit,
			reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
			perSecondDataUnit,
		))
	}

	table := tablewriter.NewWriter(strBuilder)
	table.SetHeader([]string{"Src Namespace", "Src Docs Compared", "Src Data Compared"})

	tableHasRows := false

	for _, result := range stats {
		if result.PartitionsProcessing == 0 {
			continue
		}

		tableHasRows = true

		var docsCell string
		var dataCell string

		if result.TotalDocs > 0 {
			docsCell = fmt.Sprintf("%d of %d (%s%%)",
				result.DocsCompared, result.TotalDocs,
				reportutils.FmtPercent(result.DocsCompared, result.TotalDocs),
			)
		} else {
			docsCell = fmt.Sprintf("%d", result.DocsCompared)
		}

		if result.TotalBytes > 0 {
			dataUnit := reportutils.FindBestUnit(result.TotalBytes)

			dataCell = fmt.Sprintf("%s of %s %s (%s%%)",
				reportutils.BytesToUnit(result.BytesCompared, dataUnit),
				reportutils.BytesToUnit(result.TotalBytes, dataUnit),
				dataUnit,
				reportutils.FmtPercent(result.BytesCompared, result.TotalBytes),
			)
		} else {
			dataUnit := reportutils.FindBestUnit(result.BytesCompared)

			dataCell = fmt.Sprintf("%s %s",
				reportutils.BytesToUnit(result.BytesCompared, dataUnit),
				dataUnit,
			)
		}

		table.Append([]string{result.Namespace, docsCell, dataCell})
	}

	if tableHasRows {
		strBuilder.WriteString("\nNamespaces in progress:\n")
		table.Render()
	}

	return true, nil
}

func (verifier *Verifier) printEndOfGenerationStatistics(ctx context.Context, strBuilder *strings.Builder) (bool, error) {
	stats, err := verifier.GetNamespaceStatistics(ctx)
	if err != nil {
		return false, err
	}

	// The verifier sometimes ends up enqueueing an extra generation
	// with no tasks. For now we’ll quietly no-op when that happens.
	if len(stats) == 0 {
		return false, nil
	}

	var comparedDocs types.DocumentCount
	var comparedBytes types.ByteCount
	var completedNss types.NamespaceCount

	for _, result := range stats {
		comparedDocs += result.DocsCompared
		comparedBytes += result.BytesCompared

		partitionsPending := result.PartitionsAdded + result.PartitionsProcessing
		if partitionsPending == 0 {
			completedNss++
		}
	}

	strBuilder.WriteString("\n")

	strBuilder.WriteString(fmt.Sprintf(
		"Namespaces compared: %d\n",
		completedNss,
	))

	dataUnit := reportutils.FindBestUnit(comparedBytes)

	elapsed := time.Since(verifier.generationStartTime)

	docsPerSecond := float64(comparedDocs) / elapsed.Seconds()
	bytesPerSecond := float64(comparedBytes) / elapsed.Seconds()
	perSecondDataUnit := reportutils.FindBestUnit(bytesPerSecond)

	strBuilder.WriteString(fmt.Sprintf(
		"Source documents compared: %d (%s/sec)\n",
		comparedDocs,
		reportutils.FmtFloat(docsPerSecond),
	))
	strBuilder.WriteString(fmt.Sprintf(
		"Total size of those documents: %s %s (%s %s/sec)\n",
		reportutils.BytesToUnit(comparedBytes, dataUnit),
		dataUnit,
		reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
		perSecondDataUnit,
	))

	return true, nil
}

func (verifier *Verifier) printMismatchInvestigationNotes(strBuilder *strings.Builder) {
	gen, _ := verifier.getGeneration()

	lines := []string{
		"",
		"To investigate mismatches, connect to the metadata cluster, then run:",
		fmt.Sprintf("\tuse %s", verifier.metaDBName),
		fmt.Sprintf("\tdb.%s.find({generation: %d, status: 'failed'})", verificationTasksCollection, gen),
	}

	for _, line := range lines {
		strBuilder.WriteString(line + "\n")
	}
}

func (verifier *Verifier) printChangeEventStatistics(builder *strings.Builder) {
	nsStats := verifier.eventRecorder.Read()

	activeNamespacesCount := len(nsStats)

	totalEvents := 0
	nsTotals := map[string]int{}
	for ns, events := range nsStats {
		nsTotals[ns] = events.Total()
		totalEvents += nsTotals[ns]
	}

	eventsDescr := "none"
	if totalEvents > 0 {
		eventsDescr = fmt.Sprintf("%d total, across %d namespace(s)", totalEvents, activeNamespacesCount)
	}

	builder.WriteString(fmt.Sprintf("\nChange events this generation: %s\n", eventsDescr))

	if totalEvents > 0 {

		reverseSortedNamespaces := maps.Keys(nsTotals)
		sort.Slice(
			reverseSortedNamespaces,
			func(i, j int) bool {
				return nsTotals[reverseSortedNamespaces[i]] > nsTotals[reverseSortedNamespaces[j]]
			},
		)

		// Only report the busiest namespaces.
		if len(reverseSortedNamespaces) > changeEventsTableMaxSize {
			reverseSortedNamespaces = reverseSortedNamespaces[:changeEventsTableMaxSize]
		}

		table := tablewriter.NewWriter(builder)
		table.SetHeader([]string{"Namespace", "Insert", "Update", "Replace", "Delete", "Total"})

		for _, ns := range reverseSortedNamespaces {
			curNsStats := nsStats[ns]

			table.Append(
				append(
					[]string{ns},
					strconv.Itoa(curNsStats.Insert),
					strconv.Itoa(curNsStats.Update),
					strconv.Itoa(curNsStats.Replace),
					strconv.Itoa(curNsStats.Delete),
					strconv.Itoa(curNsStats.Total()),
				),
			)
		}

		builder.WriteString("\nMost frequently-changing namespaces:\n")
		table.Render()
	}

	srcLag, hasSrcLag := verifier.srcChangeStreamReader.GetLag().Get()
	if hasSrcLag {
		builder.WriteString(
			fmt.Sprintf("\nSource change stream lag: %s\n", reportutils.DurationToHMS(srcLag)),
		)
	}

	dstLag, hasDstLag := verifier.dstChangeStreamReader.GetLag().Get()
	if hasDstLag {
		if !hasSrcLag {
			builder.WriteString("\n")
		}
		builder.WriteString(
			fmt.Sprintf("Destination change stream lag: %s\n", reportutils.DurationToHMS(dstLag)),
		)
	}
}

func (verifier *Verifier) printWorkerStatus(builder *strings.Builder) {

	table := tablewriter.NewWriter(builder)
	table.SetHeader([]string{"Thread #", "Namespace", "Task", "Time Elapsed"})

	wsmap := verifier.workerTracker.Load()

	activeThreadCount := 0
	for w := 0; w <= verifier.numWorkers; w++ {
		if wsmap[w].TaskID == nil {
			continue
		}

		activeThreadCount++

		var taskIdStr string

		switch id := wsmap[w].TaskID.(type) {
		case primitive.ObjectID:
			theBytes, _ := id.MarshalText()

			taskIdStr = string(theBytes)
		default:
			taskIdStr = fmt.Sprintf("%s", wsmap[w].TaskID)
		}

		table.Append(
			[]string{
				strconv.Itoa(w),
				wsmap[w].Namespace,
				taskIdStr,
				reportutils.DurationToHMS(time.Since(wsmap[w].StartTime)),
			},
		)
	}

	builder.WriteString(fmt.Sprintf(
		"\nActive worker threads (%d of %d):\n",
		activeThreadCount,
		verifier.numWorkers,
	))

	table.Render()
}
