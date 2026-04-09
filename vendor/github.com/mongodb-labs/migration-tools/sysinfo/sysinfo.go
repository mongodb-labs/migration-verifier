// Package sysinfo exports tools to query & report system information.
package sysinfo

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/jaypipes/ghw"
	"github.com/jaypipes/ghw/pkg/cpu"
	"github.com/mongodb-labs/migration-tools/humantools"
	"github.com/samber/lo"
	"github.com/shirou/gopsutil/v4/mem"
)

// LogSystemInfo logs system specs useful for gauging vertical scale.
func LogSystemInfo(ctx context.Context, logger *slog.Logger) {
	// NB: Per docs, negative values make this function a pure reader.
	// No state is altered here.
	memlimitBytes := debug.SetMemoryLimit(-1)

	memlimitStr := lo.Ternary(
		memlimitBytes == math.MaxInt64,
		"none",
		humantools.FmtBytes(memlimitBytes),
	)

	attrs := []slog.Attr{
		slog.Int("gomaxprocs", runtime.GOMAXPROCS(0)),
		slog.String("gomemlimit", memlimitStr),
	}

	attrs = append(attrs, slog.Group(
		"cpu",
		lo.ToAnySlice(getCPUAttrs(ctx))...,
	))

	attrs = append(attrs, slog.Group(
		"memory",
		lo.ToAnySlice(getMemoryAttrs(ctx))...,
	))

	logger.InfoContext(ctx, "System info", lo.ToAnySlice(attrs)...)
}

func getCPUAttrs(ctx context.Context) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("totalThreads", runtime.NumCPU()),
	}

	cpuInfo, err := ghw.CPU(ctx)
	if err != nil {
		attrs = append(
			attrs,
			slog.Any("cpuInfoErr", err),
		)

		return attrs
	}

	attrs = append(
		attrs,
		slog.Uint64("totalCores", uint64(cpuInfo.TotalCores)),
	)

	// TotalHardwareThreads is the same data point as
	// runtime.NumCPU() above, so we skip it.

	// Only log processor IDs if there are different IDs.
	allSameID := len(cpuInfo.Processors) <= 1 || lo.EveryBy(
		cpuInfo.Processors[1:],
		func(p *cpu.Processor) bool {
			return p.ID == cpuInfo.Processors[0].ID
		},
	)

	// Log all processor details
	for i, proc := range cpuInfo.Processors {
		var groupAttrs []slog.Attr

		if !allSameID {
			groupAttrs = []slog.Attr{
				slog.Int("id", proc.ID),
			}
		}

		if proc.Vendor != "" {
			groupAttrs = append(groupAttrs, slog.String("vendor", proc.Vendor))
		}
		if proc.Model != "" {
			groupAttrs = append(groupAttrs, slog.String("model", proc.Model))
		}
		if proc.NumCores > 0 {
			groupAttrs = append(groupAttrs, slog.Uint64("cores", uint64(proc.NumCores)))
		}
		if proc.NumThreads > 0 {
			groupAttrs = append(groupAttrs, slog.Uint64("threads", uint64(proc.NumThreads)))
		}

		// Skip Capabilities since it’s long & esoteric.

		if len(groupAttrs) > 0 {
			attrs = append(
				attrs,
				slog.Group(
					fmt.Sprintf("processor%d", i),
					lo.ToAnySlice(groupAttrs)...,
				),
			)
		}
	}

	return attrs
}

func getMemoryAttrs(ctx context.Context) []slog.Attr {
	ghwMem, err := ghw.Memory(ctx)
	if err != nil {
		attrs := getSimpleMemoryAttrs(ctx)

		// NB: ghw.Memory() doesn’t support macOS as of this writing.

		if !strings.Contains(err.Error(), "not implemented on") {
			attrs = append(attrs, slog.Any("ghwErr", err))
		}

		return attrs
	}

	var attrs []slog.Attr

	if ghwMem.TotalPhysicalBytes > 0 {
		attrs = append(
			attrs,
			slog.String("physical", humantools.FmtBytes(ghwMem.TotalPhysicalBytes)),
		)
	}
	if ghwMem.TotalUsableBytes > 0 {
		attrs = append(attrs, slog.String("usable", humantools.FmtBytes(ghwMem.TotalUsableBytes)))
	}
	if len(ghwMem.SupportedPageSizes) > 0 {
		pageSizes := make([]string, len(ghwMem.SupportedPageSizes))
		for i, size := range ghwMem.SupportedPageSizes {
			pageSizes[i] = humantools.FmtBytes(size)
		}
		attrs = append(attrs, slog.Any("pageSizes", pageSizes))
	}

	if len(attrs) == 0 {
		return getSimpleMemoryAttrs(ctx)
	}
	return attrs
}

func getSimpleMemoryAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr

	vmem, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		attrs = append(attrs, slog.Any("gopsutilErr", err))
	} else {
		if vmem.Total > 0 {
			attrs = append(attrs, slog.String("total", humantools.FmtBytes(vmem.Total)))
		}
		if vmem.Available > 0 {
			attrs = append(attrs, slog.String("available", humantools.FmtBytes(vmem.Available)))
		}
	}

	return attrs
}
