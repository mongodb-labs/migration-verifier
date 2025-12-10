package verifier

// This file holds logic to prepare visual summaries of various aspects
// of the verification: how many mismatches, rates of processing,
// number of docs/namespaces/bytes, progress, etc.

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	changeEventsTableMaxSize = 10

	lagWarnThreshold        = 2 * time.Minute
	saturationWarnThreshold = 0.9
)

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

	// TODO

	if len(failedTasks) != 0 {
		table := tablewriter.NewWriter(strBuilder)
		table.SetHeader([]string{"Index", "Cluster", "Field", "Namespace", "Details"})

		taskDiscrepancies, err := getMismatchesForTasks(
			ctx,
			verifier.verificationDatabase(),
			lo.Map(
				failedTasks,
				func(ft VerificationTask, _ int) bson.ObjectID {
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
				table.Append([]string{
					fmt.Sprintf("%v", f.ID),
					f.Cluster,
					f.Field,
					f.NameSpace,
					f.Details,
				})
			}
		}
		strBuilder.WriteString("\nCollections/Indexes in failed or retry status:\n")
		table.Render()

		return true, anyAreIncomplete, nil
	}

	return false, anyAreIncomplete, nil
}

func (verifier *Verifier) reportDocumentMismatches(ctx context.Context, strBuilder *strings.Builder) (option.Option[time.Duration], bool, error) {
	generation, _ := verifier.getGeneration()

	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
		ctx,
		verifier.logger,
		verifier.verificationTaskCollection(),
		verificationTaskVerifyDocuments,
		generation,
	)

	if err != nil {
		return option.None[time.Duration](), false, err
	}

	anyAreIncomplete := len(incompleteTasks) > 0

	if len(failedTasks) == 0 {

		// Nothing has failed/mismatched, so there’s nothing to print.
		return option.None[time.Duration](), anyAreIncomplete, nil
	}

	strBuilder.WriteString("\n")

	failedTaskMap := lo.SliceToMap(
		lo.Range(len(failedTasks)),
		func(i int) (bson.ObjectID, VerificationTask) {
			return failedTasks[i].PrimaryKey, failedTasks[i]
		},
	)
	failedTaskIDs := slices.Collect(maps.Keys(failedTaskMap))

	reportData, err := getDocumentMismatchReportData(
		ctx,
		verifier.verificationDatabase(),
		failedTaskIDs,
		verifier.failureDisplaySize,
	)
	if err != nil {
		return option.None[time.Duration](), false, errors.Wrapf(
			err,
			"fetching %d failed tasks’ most persistent discrepancies",
			len(failedTasks),
		)
	}

	if reportData.Counts.Total() == 0 {
		fmt.Printf("failedTaskIDs: %+v\n", failedTaskIDs)
		fmt.Printf("reportData: %+v\n", reportData)

		panic("No failed tasks, but no mismatches at all?!?")
	}

	// First present summaries of failures based on present/missing and differing content
	countsTable := tablewriter.NewWriter(strBuilder)

	countsHeaders := []string{"Mismatch Type", "Count"}

	countsTable.SetHeader(countsHeaders)

	if reportData.Counts.ContentDiffers > 0 {
		countsTable.Append([]string{
			"Differing Content",
			reportutils.FmtReal(reportData.Counts.ContentDiffers),
		})
	}

	if reportData.Counts.MissingOnDst > 0 {
		countsTable.Append([]string{
			"Missing on Destination",
			reportutils.FmtReal(reportData.Counts.MissingOnDst),
		})
	}

	if reportData.Counts.ExtraOnDst > 0 {
		countsTable.Append([]string{
			"Extra on Destination",
			reportutils.FmtReal(reportData.Counts.ExtraOnDst),
		})
	}

	countsTable.Render()

	if len(reportData.ContentDiffers) > 0 {
		mismatchedDocsTable := tablewriter.NewWriter(strBuilder)
		mismatchedDocsTable.SetHeader([]string{
			"Src NS",
			"Doc ID",
			"Field",
			"Details",
			"Duration",
		})

		tableIsComplete := reportData.Counts.ContentDiffers == int64(len(reportData.ContentDiffers))

		for _, m := range reportData.ContentDiffers {
			if m.Detail.DocumentIsMissing() {
				panic(fmt.Sprintf("found missing-type mismatch but expected content-differs: %+v", m))
			}

			task := failedTaskMap[m.Task]

			times := m.Detail.MismatchTimes
			duration := time.Duration(times.DurationMS) * time.Millisecond

			mismatchedDocsTable.Append([]string{
				task.QueryFilter.Namespace,
				fmt.Sprintf("%v", m.Detail.ID),
				m.Detail.Field,
				m.Detail.Details,
				reportutils.DurationToHMS(duration),
			})
		}

		strBuilder.WriteString("\n")
		if tableIsComplete {
			fmt.Fprint(
				strBuilder,
				"All documents found with differing content:\n",
			)
		} else {
			fmt.Fprintf(
				strBuilder,
				"First %d documents found with differing content:\n",
				verifier.failureDisplaySize,
			)
		}

		mismatchedDocsTable.Render()
	}

	if len(reportData.MissingOnDst) > 0 {
		missingDocsTable := tablewriter.NewWriter(strBuilder)
		missingDocsTable.SetHeader([]string{
			"Src NS",
			"Doc ID",
			"Duration",
		})

		tableIsComplete := reportData.Counts.MissingOnDst == int64(len(reportData.MissingOnDst))

		for _, d := range reportData.MissingOnDst {
			if !d.Detail.DocumentIsMissing() {
				panic(fmt.Sprintf("MissingOnDst: found content-mismatch mismatch but expected missing: %+v", reportData))
			}

			task := failedTaskMap[d.Task]

			times := d.Detail.MismatchTimes
			duration := time.Duration(times.DurationMS) * time.Millisecond

			missingDocsTable.Append([]string{
				task.QueryFilter.Namespace,
				fmt.Sprintf("%v", d.Detail.ID),
				reportutils.DurationToHMS(duration),
			})
		}

		strBuilder.WriteString("\n")

		if tableIsComplete {
			fmt.Fprint(
				strBuilder,
				"All documents found missing on the destination:\n",
			)
		} else {
			fmt.Fprintf(
				strBuilder,
				"First %d documents found missing on the destination:\n",
				verifier.failureDisplaySize,
			)
		}

		missingDocsTable.Render()
	}

	if len(reportData.ExtraOnDst) > 0 {
		extraDocsTable := tablewriter.NewWriter(strBuilder)
		extraDocsTable.SetHeader([]string{
			"Src NS",
			"Doc ID",
			"Duration",
		})

		tableIsComplete := reportData.Counts.ExtraOnDst == int64(len(reportData.ExtraOnDst))

		for _, d := range reportData.ExtraOnDst {
			if !d.Detail.DocumentIsMissing() {
				panic(fmt.Sprintf("ExtraOnDst: found content-mismatch mismatch but expected missing (%+v); reportData = %+v", d, reportData))
			}

			task := failedTaskMap[d.Task]

			times := d.Detail.MismatchTimes
			duration := time.Duration(times.DurationMS) * time.Millisecond

			extraDocsTable.Append([]string{
				task.QueryFilter.Namespace,
				fmt.Sprintf("%v", d.Detail.ID),
				reportutils.DurationToHMS(duration),
			})
		}

		strBuilder.WriteString("\n")

		if tableIsComplete {
			fmt.Fprint(
				strBuilder,
				"All documents found only on the destination:\n",
			)
		} else {
			fmt.Fprintf(
				strBuilder,
				"First %d documents found only on the destination:\n",
				verifier.failureDisplaySize,
			)
		}

		extraDocsTable.Render()
	}

	var longestDurationOpt option.Option[time.Duration]

	allShownMismatches := lo.Map(
		lo.Flatten(mslices.Of(
			reportData.ContentDiffers,
			reportData.MissingOnDst,
			reportData.ExtraOnDst,
		)),
		func(mi MismatchInfo, _ int) time.Duration {
			return time.Duration(mi.Detail.MismatchTimes.DurationMS) * time.Millisecond
		},
	)

	if len(allShownMismatches) > 0 {
		longestDurationOpt = option.Some(lo.Max(allShownMismatches))
	}

	return longestDurationOpt, anyAreIncomplete, nil
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

	var activeWorkers int
	perNamespaceWorkerStats := verifier.getPerNamespaceWorkerStats()
	for _, nsWorkerStats := range perNamespaceWorkerStats {
		for _, workerStats := range nsWorkerStats {
			activeWorkers++
			comparedDocs += workerStats.SrcDocCount
			comparedBytes += workerStats.SrcByteCount
		}
	}

	if activeWorkers > 0 {
		fmt.Fprintf(
			strBuilder,
			"Active document comparison threads: %d of %d\n",
			activeWorkers,
			verifier.numWorkers,
		)
	}

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

	headers := []string{"Src Namespace", "Threads", "Src Docs Compared"}
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

		var threads int

		docsCompared := result.DocsCompared
		bytesCompared := result.BytesCompared

		if nsWorkerStats, ok := perNamespaceWorkerStats[result.Namespace]; ok {
			threads = len(nsWorkerStats)

			for _, workerStats := range nsWorkerStats {
				docsCompared += workerStats.SrcDocCount
				bytesCompared += workerStats.SrcByteCount
			}
		}

		row := []string{result.Namespace, reportutils.FmtReal(threads)}

		var docsCell string

		if result.TotalDocs > 0 {
			docsCell = fmt.Sprintf("%s of %s (%s%%)",
				reportutils.FmtReal(docsCompared),
				reportutils.FmtReal(result.TotalDocs),
				reportutils.FmtPercent(docsCompared, result.TotalDocs),
			)
		} else {
			docsCell = reportutils.FmtReal(docsCompared)
		}

		row = append(row, docsCell)

		if showDataTotals {
			var dataCell string

			if result.TotalBytes > 0 {
				dataUnit := reportutils.FindBestUnit(result.TotalBytes)

				dataCell = fmt.Sprintf("%s of %s %s (%s%%)",
					reportutils.BytesToUnit(bytesCompared, dataUnit),
					reportutils.BytesToUnit(result.TotalBytes, dataUnit),
					dataUnit,
					reportutils.FmtPercent(bytesCompared, result.TotalBytes),
				)
			} else {
				dataUnit := reportutils.FindBestUnit(bytesCompared)

				dataCell = fmt.Sprintf("%s %s",
					reportutils.BytesToUnit(bytesCompared, dataUnit),
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

func (verifier *Verifier) printChangeEventStatistics(builder io.Writer) int {
	var eventsTable *tablewriter.Table

	fmt.Fprint(builder, "\n")

	totalEventsForBothClusters := 0

	for _, cluster := range []struct {
		title         string
		eventRecorder *EventRecorder
		csReader      changeReader
	}{
		{"Source", verifier.srcEventRecorder, verifier.srcChangeReader},
		{"Destination", verifier.dstEventRecorder, verifier.dstChangeReader},
	} {
		nsStats := cluster.eventRecorder.Read()

		activeNamespacesCount := len(nsStats)

		totalEvents := 0
		nsTotals := map[string]int{}
		for ns, events := range nsStats {
			nsTotals[ns] = events.Total()
			totalEvents += nsTotals[ns]
		}

		eventsDescr := "none"
		if totalEvents > 0 {
			eventsDescr = fmt.Sprintf(
				"%s total, across %s namespace(s)",
				reportutils.FmtReal(totalEvents),
				reportutils.FmtReal(activeNamespacesCount),
			)
		}

		totalEventsForBothClusters += totalEvents

		fmt.Fprintf(builder, "%s change events this generation: %s\n", cluster.title, eventsDescr)

		if eventsPerSec, has := cluster.csReader.getEventsPerSecond().Get(); has {
			var lagNote string

			prog, hasProg := cluster.csReader.getCurrentTimes().Get()

			if hasProg {
				lagNote = fmt.Sprintf("lag: %s; ", reportutils.DurationToHMS(prog.Lag()))
			}

			saturation := cluster.csReader.getBufferSaturation()

			fmt.Fprintf(
				builder,
				"%s: %s writes per second (%sbuffer %s%% full)\n",
				cluster.title,
				reportutils.FmtReal(eventsPerSec),
				lagNote,
				reportutils.FmtReal(100*saturation),
			)

			if hasProg && prog.Lag() > lagWarnThreshold {
				fmt.Fprint(
					builder,
					"⚠️ Lag is excessive. Verification may fail. See documentation.\n",
				)
			}

			if saturation > saturationWarnThreshold {
				fmt.Fprint(
					builder,
					"⚠️ Buffer almost full. Metadata writes are too slow. See documentation.\n",
				)
			}
		}

		if cluster.csReader == verifier.srcChangeReader {
			fmt.Fprint(builder, "\n")
		}

		// We only print event breakdowns for the source because we assume that
		// events on the destination will largely mirror the source’s.
		if totalEvents > 0 && cluster.csReader == verifier.srcChangeReader {
			reverseSortedNamespaces := slices.Collect(maps.Keys(nsTotals))
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

		fmt.Fprint(builder, "\n")
	}

	if eventsTable != nil {
		fmt.Fprint(builder, "\nSource’s most frequently-changing namespaces:\n")

		eventsTable.Render()
	}

	return totalEventsForBothClusters
}

func (verifier *Verifier) getPerNamespaceWorkerStats() map[string][]WorkerStatus {
	wsmap := verifier.workerTracker.Load()

	retMap := map[string][]WorkerStatus{}

	for _, workerStats := range wsmap {
		if workerStats.TaskID == nil {
			continue
		}

		retMap[workerStats.Namespace] = append(
			retMap[workerStats.Namespace],
			workerStats,
		)
	}

	return retMap
}

func (verifier *Verifier) printWorkerStatus(builder *strings.Builder, now time.Time) {

	table := tablewriter.NewWriter(builder)
	table.SetHeader([]string{"Thread #", "Namespace", "Task", "Time Elapsed", "Detail"})

	wsmap := verifier.workerTracker.Load()

	activeThreadCount := 0
	for w := range verifier.numWorkers {
		if wsmap[w].TaskID == nil {
			continue
		}

		activeThreadCount++

		var taskIdStr string

		switch id := wsmap[w].TaskID.(type) {
		case bson.ObjectID:
			theBytes, _ := id.MarshalText()

			taskIdStr = string(theBytes)
		default:
			taskIdStr = fmt.Sprintf("%s", wsmap[w].TaskID)
		}

		var detail string
		if wsmap[w].TaskType == verificationTaskVerifyDocuments {
			detail = fmt.Sprintf(
				"%s documents (%s)",
				reportutils.FmtReal(wsmap[w].SrcDocCount),
				reportutils.FmtBytes(wsmap[w].SrcByteCount),
			)
		}

		table.Append(
			[]string{
				reportutils.FmtReal(w),
				wsmap[w].Namespace,
				taskIdStr,
				reportutils.DurationToHMS(now.Sub(wsmap[w].StartTime)),
				detail,
			},
		)
	}

	fmt.Fprintf(builder, "\nWorker thread details:\n")

	table.Render()
}
