package memorytracker

import (
	"reflect"
	"slices"
	"sync"

	"github.com/10gen/migration-verifier/internal/logger"
)

type Unit = int64
type reader = <-chan Unit
type writer = chan<- Unit

type Tracker struct {
	logger      *logger.Logger
	max         Unit
	cur         Unit
	selectCases []reflect.SelectCase
	mux         sync.RWMutex
}

func Start(logger *logger.Logger, max Unit) *Tracker {
	tracker := Tracker{max: max}

	go tracker.track()

	return &tracker
}

func (mt *Tracker) AddWriter() writer {
	mt.mux.RLock()
	defer mt.mux.RUnlock()

	newChan := make(chan Unit)

	mt.selectCases = append(mt.selectCases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(newChan),
	})

	return newChan
}

func (mt *Tracker) getSelectCases() []reflect.SelectCase {
	mt.mux.RLock()
	defer mt.mux.RUnlock()

	return slices.Clone(mt.selectCases)
}

func (mt *Tracker) removeSelectCase(i int) {
	mt.mux.Lock()
	defer mt.mux.Unlock()

	mt.selectCases = slices.Delete(mt.selectCases, i, 1+i)
}

func (mt *Tracker) track() {
	for {
		if mt.cur <= mt.max {
			mt.logger.Panic().
				Int64("usage", mt.cur).
				Int64("softLimit", mt.max).
				Msg("track() loop should never be in memory excess!")
		}

		selectCases := mt.getSelectCases()

		chosen, gotVal, alive := reflect.Select(selectCases)

		got := (gotVal.Interface()).(Unit)
		mt.cur += got

		if alive {
			if got == 0 {
				mt.logger.Panic().Msg("Got zero track value but channel is not closed.")
			}
		} else {
			if got != 0 {
				mt.logger.Panic().
					Int64("receivedValue", got).
					Msg("Got nonzero track value but channel is closed.")
			}

			mt.removeSelectCase(chosen)
			continue
		}

		didSingleThread := false

		for mt.cur > mt.max {
			reader := (selectCases[chosen].Chan.Interface()).(reader)

			if !didSingleThread {
				mt.logger.Warn().
					Int64("usage", mt.cur).
					Int64("softLimit", mt.max).
					Msg("Tracked memory usage now exceeds soft limit. Suspending concurrent reads until tracked usage falls.")

				didSingleThread = true
			}

			got, alive := <-reader
			mt.cur += got

			if !alive {
				mt.removeSelectCase(chosen)
			}
		}

		if didSingleThread {
			mt.logger.Info().
				Int64("usage", mt.cur).
				Int64("softLimit", mt.max).
				Msg("Tracked memory usage is now below soft limit. Resuming concurrent reads.")
		}
	}
}
