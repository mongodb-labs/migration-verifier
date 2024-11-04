package memorytracker

import (
	"context"
	"reflect"
	"slices"
	"sync"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/reportutils"
)

type Unit = int64
type reader = <-chan Unit
type Writer = chan<- Unit

type Tracker struct {
	logger      *logger.Logger
	softLimit   Unit
	curUsage    Unit
	selectCases []reflect.SelectCase
	mux         sync.RWMutex
}

func Start(ctx context.Context, logger *logger.Logger, max Unit) *Tracker {
	tracker := Tracker{
		softLimit: max,
		logger:    logger,
	}

	go tracker.track(ctx)

	return &tracker
}

func (mt *Tracker) AddWriter() Writer {
	mt.mux.RLock()
	defer mt.mux.RUnlock()

	newChan := make(chan Unit)

	mt.selectCases = append(mt.selectCases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(reader(newChan)),
	})

	return newChan
}

func (mt *Tracker) getSelectCases(ctx context.Context) []reflect.SelectCase {
	mt.mux.RLock()
	defer mt.mux.RUnlock()

	cases := make([]reflect.SelectCase, 1+len(mt.selectCases))
	cases[0] = reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	}

	for i := range mt.selectCases {
		cases[1+i] = mt.selectCases[i]
	}

	return cases
}

func (mt *Tracker) removeSelectCase(i int) {
	mt.mux.Lock()
	defer mt.mux.Unlock()

	mt.selectCases = slices.Delete(mt.selectCases, i-1, i)
}

func (mt *Tracker) track(ctx context.Context) {
	for {
		if mt.curUsage > mt.softLimit {
			mt.logger.Panic().
				Int64("usage", mt.curUsage).
				Int64("softLimit", mt.softLimit).
				Msg("track() loop should never be in memory excess!")
		}

		selectCases := mt.getSelectCases(ctx)

		chosen, gotVal, alive := reflect.Select(selectCases)

		if chosen == 0 {
			mt.logger.Debug().
				AnErr("contextErr", context.Cause(ctx)).
				Msg("Stopping memory tracker.")

			return
		}

		got := (gotVal.Interface()).(Unit)
		mt.curUsage += got

		if got < 0 {
			mt.logger.Info().
				Str("reclaimed", reportutils.FmtBytes(-got)).
				Str("tracked", reportutils.FmtBytes(mt.curUsage)).
				Msg("Reclaimed tracked memory.")
		}

		if !alive {
			if got != 0 {
				mt.logger.Panic().
					Int64("receivedValue", got).
					Msg("Got nonzero track value but channel is closed.")
			}

			// Closure of a channel indicates that the worker thread is
			// finished.
			mt.removeSelectCase(chosen)

			continue
		}

		didSingleThread := false

		for mt.curUsage > mt.softLimit {
			reader := (selectCases[chosen].Chan.Interface()).(reader)

			if !didSingleThread {
				mt.logger.Warn().
					Str("usage", reportutils.FmtBytes(mt.curUsage)).
					Str("softLimit", reportutils.FmtBytes(mt.softLimit)).
					Msg("Tracked memory usage now exceeds soft limit. Suspending concurrent reads until tracked usage falls.")

				didSingleThread = true
			}

			got, alive := <-reader
			mt.curUsage += got

			if !alive {
				mt.removeSelectCase(chosen)
			}
		}

		if didSingleThread {
			mt.logger.Info().
				Str("usage", reportutils.FmtBytes(mt.curUsage)).
				Str("softLimit", reportutils.FmtBytes(mt.softLimit)).
				Msg("Tracked memory usage is now below soft limit. Resuming concurrent reads.")
		}
	}
}
