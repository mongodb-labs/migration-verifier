package verifier

// This file holds logic to prepare visual summaries of various aspects
// of the verification: how many mismatches, rates of processing,
// number of docs/namespaces/bytes, progress, etc.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/olekukonko/tablewriter"
)

// NOTE: Each of the following should print one trailing and one final
// newline.

// Returned booleans indicate:
//   - whether any mismatches were found
//   - whether any incomplete tasks were found
func (verifier *Verifier) reportCollectionMetadataMismatches(ctx context.Context, strBuilder *strings.Builder) (bool, bool, error) {
	generation, _ := verifier.getGeneration()

	failedTasks, incompleteTasks, err :=
		FetchFailedAndIncompleteTasks(ctx, verifier.verificationTaskCollection(), verificationTaskVerifyCollection, generation)
	if err != nil {
		return false, false, err
	}

	anyAreIncomplete := len(incompleteTasks) > 0

	if len(failedTasks) != 0 {
		table := tablewriter.NewWriter(strBuilder)
		table.SetHeader([]string{"Index", "Cluster", "Type", "Field", "Namespace", "Details"})

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

// Returned boolean indicates whether any mismatches were found.
func (verifier *Verifier) reportCollectionCountMismatches(ctx context.Context, strBuilder *strings.Builder) (bool, error) {
	maxMismatchedCountsToShow := verifier.failureDisplaySize

	mismatchedCountsTable := tablewriter.NewWriter(strBuilder)
	mismatchedCountsTable.SetHeader([]string{"Namespace (Source)", "Source Count", "Destination Count"})
	abridgedText := ""

	var diffCounts int64

	for i, n := range verifier.srcNamespaces {
		srcDb, srcColl := SplitNamespace(n)
		if srcDb == "" {
			continue
		}

		dstDb, dstColl := SplitNamespace(verifier.dstNamespaces[i])
		if dstDb == "" {
			continue
		}

		srcCollection := verifier.srcClientDatabase(srcDb).Collection(srcColl)
		srcSpec, err := verifier.getCollectionSpecification(ctx, srcCollection)
		if err != nil {
			return false, fmt.Errorf("Failed to fetch %s’s specification from the source: %+v", FullName(srcCollection), err)
		}

		// "EstimatedDocumentCount" on a view runs its pipeline, so may be very slow
		if srcSpec != nil && srcSpec.Type == "view" {
			continue
		}

		srcEst, err := srcCollection.EstimatedDocumentCount(ctx)
		if err != nil {
			return false, fmt.Errorf("Failed to fetch %s’s document count from the source: %+v", FullName(srcCollection), err)
		}

		dstCollection := verifier.dstClientDatabase(dstDb).Collection(dstColl)
		dstSpec, err := verifier.getCollectionSpecification(ctx, dstCollection)
		if err != nil {
			return false, fmt.Errorf("Failed to fetch %s’s specification from the destination: %+v", FullName(dstCollection), err)
		}

		if dstSpec != nil && dstSpec.Type == "view" {
			continue
		}

		dstEst, err := dstCollection.EstimatedDocumentCount(ctx)
		if err != nil {
			return false, fmt.Errorf("Failed to fetch %s’s document count from the destination: %+v", FullName(dstCollection), err)
		}

		// Finally, we have document counts for both source &
		// destination. Now we can process those:

		if srcEst != dstEst {
			diffCounts++

			if diffCounts < maxMismatchedCountsToShow {
				mismatchedCountsTable.Append([]string{
					FullName(srcCollection),
					strconv.FormatInt(srcEst, 10),
					strconv.FormatInt(dstEst, 10),
				})
			} else {
				abridgedText = fmt.Sprintf(" (first %d)", maxMismatchedCountsToShow)
			}
		}
	}

	if diffCounts > 0 {
		strBuilder.WriteString("\n")

		strBuilder.WriteString(fmt.Sprintf(
			"Mismatched collection document counts%s:\n",
			abridgedText,
		))
		mismatchedCountsTable.Render()

		return true, nil
	}

	return false, nil
}

func (verifier *Verifier) reportDocumentMismatches(ctx context.Context, strBuilder *strings.Builder) (bool, bool, error) {
	generation, _ := verifier.getGeneration()

	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(ctx, verifier.verificationTaskCollection(), verificationTaskVerifyDocuments, generation)

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

	contentMismatch := 0
	missing := 0
	for _, v := range failedTasks {
		contentMismatch += len(v.FailedDocs)
		missing += len(v.Ids)
	}

	failureTypesTable.Append([]string{"Documents With Differing Content", fmt.Sprintf("%v", contentMismatch)})
	failureTypesTable.Append([]string{"Documents Missing On Source or Dest", fmt.Sprintf("%v", missing)})
	strBuilder.WriteString("Failure summary:\n")
	failureTypesTable.Render()

	mismatchedDocsTable := tablewriter.NewWriter(strBuilder)
	mismatchedDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	mismatchedDocsTable.SetHeader([]string{"ID", "Cluster", "Type", "Field", "Namespace", "Details"})

	printAll := int64(contentMismatch) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTA:
	for _, v := range failedTasks {
		for _, f := range v.FailedDocs {
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

	missingDocsTable := tablewriter.NewWriter(strBuilder)
	missingDocsTableRows := types.ToNumericTypeOf(0, verifier.failureDisplaySize)
	missingDocsTable.SetHeader([]string{"Document ID", "Source NameSpace", "Destination Namespace"})

	printAll = int64(missing) < (verifier.failureDisplaySize + int64(0.25*float32(verifier.failureDisplaySize)))
OUTB:
	for _, v := range failedTasks {
		for _, _id := range v.Ids {
			if !printAll && missingDocsTableRows >= verifier.failureDisplaySize {
				break OUTB
			}

			missingDocsTableRows++
			missingDocsTable.Append([]string{
				fmt.Sprintf("%v", _id),
				fmt.Sprintf("%v", v.QueryFilter.Namespace),
				fmt.Sprintf("%v", v.QueryFilter.To),
			})
		}
	}

	if missingDocsTableRows > 0 {
		strBuilder.WriteString("\n")
		if printAll {
			strBuilder.WriteString("All documents present in source/destination missing in destination/source:\n")
		} else {
			strBuilder.WriteString(fmt.Sprintf("First %d documents present in source/destination missing in destination/source:\n", verifier.failureDisplaySize))
		}
		missingDocsTable.Render()
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
		dataUnit := reportutils.FindBestUnit(totalBytes)

		strBuilder.WriteString(fmt.Sprintf(
			"Total source documents compared: %d of %d (%s%%, %s/sec)\n",
			comparedDocs,
			totalDocs,
			reportutils.FmtPercent(comparedDocs, totalDocs),
			reportutils.FmtFloat(docsPerSecond),
		))
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
			"Total source documents compared: %d (%s/sec)\n",
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
	}

	table := tablewriter.NewWriter(strBuilder)
	table.SetHeader([]string{"Src Namespace", "Src Docs Compared", "Src Data Size"})

	tableHasRows := false

	for _, result := range stats {
		if result.PartitionsProcessing == 0 {
			continue
		}

		tableHasRows = true

		// Generation #1 will have TotalDocs and Total Bytes;
		// subsequent generations won’t.
		if result.TotalDocs > 0 {
			dataUnit := reportutils.FindBestUnit(result.TotalBytes)

			table.Append([]string{
				result.Namespace,
				fmt.Sprintf("%d of %d (%s%%)",
					result.DocsCompared, result.TotalDocs,
					reportutils.FmtPercent(result.DocsCompared, result.TotalDocs),
				),

				fmt.Sprintf("%s of %s %s (%s%%)",
					reportutils.BytesToUnit(result.BytesCompared, dataUnit),
					reportutils.BytesToUnit(result.TotalBytes, dataUnit),
					dataUnit,
					reportutils.FmtPercent(result.BytesCompared, result.TotalBytes),
				),
			})
		} else {
			dataUnit := reportutils.FindBestUnit(result.BytesCompared)

			table.Append([]string{
				result.Namespace,
				fmt.Sprintf("%d", result.DocsCompared),
				fmt.Sprintf("%s %s",
					reportutils.BytesToUnit(result.BytesCompared, dataUnit),
					dataUnit,
				),
			})
		}
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
		fmt.Sprintf("To investigate mismatches, connect to %s, then run:", verifier.metaURI),
		fmt.Sprintf("\tuse %s", verifier.metaDBName),
		fmt.Sprintf("\tdb.%s.find({generation: %d, status: 'failed'})", verificationTasksCollection, gen),
	}

	for _, line := range lines {
		strBuilder.WriteString(line + "\n")
	}
}
