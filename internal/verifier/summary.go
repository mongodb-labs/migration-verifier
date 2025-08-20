package verifier

// This file holds logic to prepare visual summaries of various aspects
// of the verification: how many mismatches, rates of processing,
// number of docs/namespaces/bytes, progress, etc.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/samber/lo"
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

		taskDiscrepancies, err := getMismatchesForTasks(
			ctx,
			verifier.verificationDatabase(),
			lo.Map(
				failedTasks,
				func(ft VerificationTask, _ int) primitive.ObjectID {
					return ft.PrimaryKey
				},
			),
		)
		if err != nil {
			return false, false, errors.Wrapf(
				err,
				"fetching %d failed tasks' discrepancies",
				len(failedTasks),
			)
		}

		for _, v := range failedTasks {
			for _, f := range taskDiscrepancies[v.PrimaryKey] {
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

	taskDiscrepancies, err := getMismatchesForTasks(
		ctx,
		verifier.verificationDatabase(),
		lo.Map(
			failedTasks,
			func(ft VerificationTask, _ int) primitive.ObjectID {
				return ft.PrimaryKey
			},
		),
	)
	if err != nil {
		return false, false, errors.Wrapf(
			err,
			"fetching %d failed tasks' discrepancies",
			len(failedTasks),
		)
	}

	contentMismatchCount := 0
	missingOrChangedCount := 0
	for _, task := range failedTasks {
		discrepancies, hasDiscrepancies := taskDiscrepancies[task.PrimaryKey]
		if !hasDiscrepancies {
			return false, false, errors.Wrapf(
				err,
				"task %v is marked %#q but has no recorded discrepancies; internal error?",
				task.PrimaryKey,
				task.Status,
			)
		}

		missingCount := lo.CountBy(
			discrepancies,
			func(d VerificationResult) bool {
				return d.DocumentIsMissing()
			},
		)

		contentMismatchCount += len(discrepancies) - missingCount
		missingOrChangedCount += missingCount
	}

	failureTypesTable.Append([]string{
		"Documents With Differing Content",
		fmt.Sprintf("%v", reportutils.FmtReal(contentMismatchCount)),
	})
	failureTypesTable.Append([]string{
		"Missing or Changed Documents",
		fmt.Sprintf("%v", reportutils.FmtReal(missingOrChangedCount)),
	})
	strBuilder.WriteString("Failure summary:\n")
	failureTypesTable.Render()

	mismatchedDocsTable := tablewriter.NewWriter(strBuilder)
	mismatchedDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	mismatchedDocsTable.SetHeader([]string{"ID", "Cluster", "Field", "Namespace", "Details"})

	printAll := int64(contentMismatchCount) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTA:
	for _, task := range failedTasks {
		for _, d := range taskDiscrepancies[task.PrimaryKey] {
			if d.DocumentIsMissing() {
				continue
			}

			if !printAll && mismatchedDocsTableRows >= verifier.failureDisplaySize {
				break OUTA
			}

			mismatchedDocsTableRows++
			mismatchedDocsTable.Append([]string{
				fmt.Sprintf("%v", d.ID),
				fmt.Sprintf("%v", d.Cluster),
				fmt.Sprintf("%v", d.Field),
				fmt.Sprintf("%v", d.NameSpace),
				fmt.Sprintf("%v", d.Details),
			})
		}
	}

	if mismatchedDocsTableRows > 0 {
		strBuilder.WriteString("\n")
		if printAll {
			strBuilder.WriteString("All documents in tasks in failed status due to differing content:\n")
		} else {
			fmt.Fprintf(strBuilder, "First %d documents in tasks in failed status due to differing content:\n", verifier.failureDisplaySize)
		}
		mismatchedDocsTable.Render()
	}

	missingOrChangedDocsTable := tablewriter.NewWriter(strBuilder)
	missingOrChangedDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	missingOrChangedDocsTable.SetHeader([]string{"Document ID", "Source Namespace", "Destination Namespace"})

	printAll = int64(missingOrChangedCount) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTB:
	for _, task := range failedTasks {
		for _, d := range taskDiscrepancies[task.PrimaryKey] {
			if !d.DocumentIsMissing() {
				continue
			}

			if !printAll && missingOrChangedDocsTableRows >= verifier.failureDisplaySize {
				break OUTB
			}

			missingOrChangedDocsTableRows++
			missingOrChangedDocsTable.Append([]string{
				fmt.Sprintf("%v", d.ID),
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
			fmt.Fprintf(strBuilder, "First %d documents marked missing or changed:\n", verifier.failureDisplaySize)
		}
		missingOrChangedDocsTable.Render()
	}

	return true, anyAreIncomplete, nil
}

// Boolean returned indicates whether this generation has any tasks.
func (verifier *Verifier) printNamespaceStatistics(ctx context.Context, strBuilder *strings.Builder, now time.Time) (bool, error) {
	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
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

	fmt.Fprintf(
		strBuilder,
		"Namespaces completed: %s of %s (%s%%)\n",
		reportutils.FmtReal(completedNss),
		reportutils.FmtReal(totalNss),
		reportutils.FmtPercent(completedNss, totalNss),
	)

	elapsed := now.Sub(verifier.generationStartTime)

	docsPerSecond := float64(comparedDocs) / elapsed.Seconds()
	bytesPerSecond := float64(comparedBytes) / elapsed.Seconds()
	perSecondDataUnit := reportutils.FindBestUnit(bytesPerSecond)

	if totalDocs > 0 {
		fmt.Fprintf(
			strBuilder,
			"Total source documents compared: %s of %s (%s%%, %s/sec)\n",
			reportutils.FmtReal(comparedDocs),
			reportutils.FmtReal(totalDocs),
			reportutils.FmtPercent(comparedDocs, totalDocs),
			reportutils.FmtReal(docsPerSecond),
		)
	} else {
		fmt.Fprintf(
			strBuilder,
			"Total source documents compared: %s (%s/sec)\n",
			reportutils.FmtReal(comparedDocs),
			reportutils.FmtReal(docsPerSecond),
		)
	}

	showDataTotals := verifier.docCompareMethod.ComparesFullDocuments()

	if showDataTotals {
		if totalBytes > 0 {
			dataUnit := reportutils.FindBestUnit(totalBytes)

			fmt.Fprintf(
				strBuilder,
				"Total size of those documents: %s of %s %s (%s%%, %s %s/sec)\n",
				reportutils.BytesToUnit(comparedBytes, dataUnit),
				reportutils.BytesToUnit(totalBytes, dataUnit),
				dataUnit,
				reportutils.FmtPercent(comparedBytes, totalBytes),
				reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
				perSecondDataUnit,
			)
		} else {
			dataUnit := reportutils.FindBestUnit(comparedBytes)

			fmt.Fprintf(
				strBuilder,
				"Total size of those documents: %s %s (%s %s/sec)\n",
				reportutils.BytesToUnit(comparedBytes, dataUnit),
				dataUnit,
				reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
				perSecondDataUnit,
			)
		}
	}

	table := tablewriter.NewWriter(strBuilder)

	headers := []string{"Src Namespace", "Src Docs Compared"}
	if showDataTotals {
		headers = append(headers, "Src Data Compared")
	}
	table.SetHeader(headers)

	tableHasRows := false

	for _, result := range stats {
		if result.PartitionsProcessing == 0 {
			continue
		}

		tableHasRows = true

		row := []string{result.Namespace}

		var docsCell string

		if result.TotalDocs > 0 {
			docsCell = fmt.Sprintf("%s of %s (%s%%)",
				reportutils.FmtReal(result.DocsCompared),
				reportutils.FmtReal(result.TotalDocs),
				reportutils.FmtPercent(result.DocsCompared, result.TotalDocs),
			)
		} else {
			docsCell = reportutils.FmtReal(result.DocsCompared)
		}

		row = append(row, docsCell)

		if showDataTotals {
			var dataCell string

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

			row = append(row, dataCell)
		}

		table.Append(row)
	}

	if tableHasRows {
		strBuilder.WriteString("\nNamespaces in progress:\n")
		table.Render()
	}

	return true, nil
}

func (verifier *Verifier) printEndOfGenerationStatistics(ctx context.Context, strBuilder *strings.Builder, now time.Time) (bool, error) {
	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
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

	fmt.Fprintf(
		strBuilder,
		"Namespaces compared: %s\n",
		reportutils.FmtReal(completedNss),
	)

	dataUnit := reportutils.FindBestUnit(comparedBytes)

	elapsed := now.Sub(verifier.generationStartTime)

	docsPerSecond := float64(comparedDocs) / elapsed.Seconds()
	bytesPerSecond := float64(comparedBytes) / elapsed.Seconds()
	perSecondDataUnit := reportutils.FindBestUnit(bytesPerSecond)

	fmt.Fprintf(
		strBuilder,
		"Source documents compared: %s (%s/sec)\n",
		reportutils.FmtReal(comparedDocs),
		reportutils.FmtReal(docsPerSecond),
	)

	if verifier.docCompareMethod.ComparesFullDocuments() {
		fmt.Fprintf(
			strBuilder,
			"Total size of those documents: %s %s (%s %s/sec)\n",
			reportutils.BytesToUnit(comparedBytes, dataUnit),
			dataUnit,
			reportutils.BytesToUnit(bytesPerSecond, perSecondDataUnit),
			perSecondDataUnit,
		)
	}

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

func (verifier *Verifier) printChangeEventStatistics(builder *strings.Builder, now time.Time) {
	var eventsTable *tablewriter.Table

	for _, cluster := range []struct {
		title         string
		eventRecorder *EventRecorder
		csReader      *ChangeStreamReader
	}{
		{"Source", verifier.srcEventRecorder, verifier.srcChangeStreamReader},
		{"Destination", verifier.dstEventRecorder, verifier.dstChangeStreamReader},
	} {
		nsStats := cluster.eventRecorder.Read()

		activeNamespacesCount := len(nsStats)

		totalEvents := 0
		nsTotals := map[string]int{}
		for ns, events := range nsStats {
			nsTotals[ns] = events.Total()
			totalEvents += nsTotals[ns]
		}

		elapsed := now.Sub(verifier.generationStartTime)

		eventsDescr := "none"
		if totalEvents > 0 {
			eventsDescr = fmt.Sprintf(
				"%s total (%s/sec), across %s namespace(s)",
				reportutils.FmtReal(totalEvents),
				reportutils.FmtReal(util.Divide(totalEvents, elapsed.Seconds())),
				reportutils.FmtReal(activeNamespacesCount),
			)
		}

		fmt.Fprintf(builder, "\n%s change events this generation: %s\n", cluster.title, eventsDescr)

		lag, hasLag := cluster.csReader.GetLag().Get()
		if hasLag {
			fmt.Fprintf(builder, "%s change stream lag: %s\n", cluster.title, reportutils.DurationToHMS(lag))
		}

		// We only print event breakdowns for the source because we assume that
		// events on the destination will largely mirror the source’s.
		if totalEvents > 0 && cluster.csReader == verifier.srcChangeStreamReader {
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

			eventsTable = tablewriter.NewWriter(builder)
			eventsTable.SetHeader([]string{"Namespace", "Insert", "Update", "Replace", "Delete", "Total"})

			for _, ns := range reverseSortedNamespaces {
				curNsStats := nsStats[ns]

				eventsTable.Append(
					append(
						[]string{ns},
						reportutils.FmtReal(curNsStats.Insert),
						reportutils.FmtReal(curNsStats.Update),
						reportutils.FmtReal(curNsStats.Replace),
						reportutils.FmtReal(curNsStats.Delete),
						reportutils.FmtReal(curNsStats.Total()),
					),
				)
			}
		}
	}

	if eventsTable != nil {
		builder.WriteString("\nSource’s most frequently-changing namespaces:\n")

		eventsTable.Render()
	}
}

func (verifier *Verifier) printWorkerStatus(builder *strings.Builder, now time.Time) {

	table := tablewriter.NewWriter(builder)
	table.SetHeader([]string{"Thread #", "Namespace", "Task", "Time Elapsed", "Detail"})

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
				reportutils.FmtReal(w),
				wsmap[w].Namespace,
				taskIdStr,
				reportutils.DurationToHMS(now.Sub(wsmap[w].StartTime)),
				wsmap[w].Detail,
			},
		)
	}

	fmt.Fprintf(
		builder,
		"\nActive worker threads (%s of %s):\n",
		reportutils.FmtReal(activeThreadCount),
		reportutils.FmtReal(verifier.numWorkers),
	)

	table.Render()
}
