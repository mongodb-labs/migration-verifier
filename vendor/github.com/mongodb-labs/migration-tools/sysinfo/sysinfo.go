package sysinfo

import (
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"

	"github.com/mongodb-labs/migration-tools/humantools"
	"github.com/samber/lo"
	"github.com/shirou/gopsutil/v3/mem"
)

// LogSystemInfo logs system specs useful for gauging vertical scale.
// It temporarily reads the current Go memory limit via debug.SetMemoryLimit(-1)
// and restores it before returning; this touches process-global runtime state.
func LogSystemInfo(logger *slog.Logger) {
	memlimitBytes := debug.SetMemoryLimit(-1)

	memlimitStr := lo.Ternary(
		memlimitBytes == math.MaxInt64,
		"none",
		humantools.FmtBytes(memlimitBytes),
	)

	attrs := []any{
		slog.Int("cpus", runtime.NumCPU()),
		slog.Int("gomaxprocs", runtime.GOMAXPROCS(0)),
		slog.String("gomemlimit", memlimitStr),
	}

	memStats, err := mem.VirtualMemory()
	if err != nil {
		attrs = append(attrs, slog.Any("totalRAMErr", err))
	} else {
		attrs = append(attrs, slog.String("totalRAM", humantools.FmtBytes(memStats.Total)))
	}

	logger.Info("System info", attrs...)
}
