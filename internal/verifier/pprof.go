package verifier

import (
	"context"
	"fmt"
	"os"
	"runtime/pprof"
	"time"
)

func (verifier *Verifier) MaybeStartPeriodicHeapProfileCollection(ctx context.Context) {
	if verifier.pprofInterval == 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(verifier.pprofInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collectHeapUsage()
			}
		}
	}()

}

func collectHeapUsage() {
	heapFileName := fmt.Sprintf("heap-%s.out", time.Now().UTC().Format("20060102T150405Z"))
	heapFile, err := os.Create(heapFileName)

	if err != nil {
		panic(err)
	}

	defer heapFile.Close()

	err = pprof.Lookup("heap").WriteTo(heapFile, 0)
	if err != nil {
		panic(err)
	}
}
