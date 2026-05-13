package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/ccoveille/go-safecast/v2"
)

// GetSummary returns a SummaryResponse summarizing the verifier's current
// state, including mismatch counts, change-event totals, and check-phase ETA.
// minDurationSecs filters document mismatches by minimum mismatch duration.
func (verifier *Verifier) GetSummary(
	ctx context.Context,
	minDurationSecs uint32,
) (api.SummaryResponse, error) {
	var progress api.Progress
	var nsMismatches []api.NSMismatchInfo
	var docMismatches api.DocMismatchSummary

	eg, egCtx := contextplus.ErrGroup(ctx)

	eg.Go(func() error {
		var err error
		progress, err = verifier.GetProgress(egCtx)
		return err
	})

	eg.Go(func() error {
		var err error
		nsMismatches, err = collectNSMismatches(egCtx, verifier)
		return err
	})

	eg.Go(func() error {
		var err error
		docMismatches, err = collectDocMismatches(egCtx, verifier, minDurationSecs)
		return err
	})

	if err := eg.Wait(); err != nil {
		return api.SummaryResponse{}, err
	}

	resp := api.SummaryResponse{
		EstCheckSecsRemaining: computeCheckETASeconds(progress),
		RecentRecheckSecs:     progress.RecentRecheckSecs,
		CheckStats:            buildCheckStats(progress),
		NSMismatches:          nsMismatches,
		DocMismatches:         docMismatches,
		TotalRechecks:         safecast.MustConvert[int64](uint64(progress.TotalRechecksDone)),
		SrcChangeEvents:       progress.SrcChangeStats.EventCounts,
		DstChangeEvents:       progress.DstChangeStats.EventCounts,
		Notes:                 buildNotes(minDurationSecs),
	}

	if resp.NSMismatches == nil {
		resp.NSMismatches = []api.NSMismatchInfo{}
	}

	return resp, nil
}

// computeCheckETASeconds computes the Check phase’s remaining seconds by
// dividing remaining bytes by current bytes/sec rate. Returns None when the
// verifier is not in the Check phase or has no measured rate — i.e.,
// when there is no meaningful ETA. Returns Some(0) once the check is
// complete (no bytes pending).
func computeCheckETASeconds(p api.Progress) option.Option[float64] {
	if p.Phase != Check || p.SrcBytesComparedPerSecond == 0 {
		return option.None[float64]()
	}

	totalBytes := p.GenerationStats.TotalSrcBytes
	comparedBytes := p.GenerationStats.SrcBytesCompared
	if comparedBytes >= totalBytes {
		return option.Some[float64](0)
	}

	bytesPending := totalBytes - comparedBytes
	return option.Some(float64(bytesPending) / p.SrcBytesComparedPerSecond)
}

// buildCheckStats returns the generation-0 stats. During the initial check
// (generation 0) it reports the current GenerationStats; afterwards it reports
// the cached final gen-0 stats.
func buildCheckStats(p api.Progress) option.Option[api.GenerationStats] {
	if p.Generation == 0 {
		return option.Some(p.GenerationStats)
	}

	if g0, has := p.Gen0Stats.Get(); has {
		return option.Some(g0)
	}

	return option.None[api.GenerationStats]()
}

func buildNotes(minDurationSecs uint32) []string {
	var notes []string
	if minDurationSecs > 0 {
		notes = append(notes, fmt.Sprintf("Filtering document mismatches by minimum duration (%d seconds)", minDurationSecs))
	}
	return notes
}

func collectNSMismatches(ctx context.Context, verifier *Verifier) ([]api.NSMismatchInfo, error) {
	out := make(chan api.NSMismatchInfo)

	eg, egCtx := contextplus.ErrGroup(ctx)
	eg.Go(func() error {
		return verifier.SendNamespaceMismatches(egCtx, nil, out)
	})

	var collected []api.NSMismatchInfo
	for m := range out {
		collected = append(collected, m)
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return collected, nil
}

func collectDocMismatches(
	ctx context.Context,
	verifier *Verifier,
	minSecs uint32,
) (api.DocMismatchSummary, error) {
	out := make(chan api.DocMismatchInfo)

	eg, egCtx := contextplus.ErrGroup(ctx)
	eg.Go(func() error {
		return verifier.SendDocumentMismatches(egCtx, minSecs, out)
	})

	summary := api.DocMismatchSummary{
		ByType:      map[string]int64{},
		ByNamespace: map[string]int64{},
	}

	for m := range out {
		summary.Total++
		summary.ByType[string(m.Type)]++
		summary.ByNamespace[m.Namespace]++
	}

	if err := eg.Wait(); err != nil {
		return api.DocMismatchSummary{}, err
	}

	return summary, nil
}
