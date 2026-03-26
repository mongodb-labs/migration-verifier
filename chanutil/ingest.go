package chanutil

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/util"
)

// IngestCanceler cancels an ingest goroutine started by StartIngestMap or
// StartIngestFilter. It signals the goroutine to stop and blocks until it has
// exited. If the provided context expires before the goroutine exits (e.g.
// because the consumer of out has stopped reading), it force-cancels the
// goroutine and returns the context error.
type IngestCanceler func(context.Context) error

// startIngest is the shared implementation behind StartIngestMap and
// StartIngestFilter. process returns (value, true) to forward a value to out,
// or (zero, false) to drop it.
func startIngest[In any, Out any](
	ctx context.Context,
	out chan<- Out,
	process func(In) (Out, bool),
) (chan<- In, IngestCanceler) {
	in := make(chan In)
	done := make(chan struct{})
	innerCtx, innerCancel := contextplus.WithCancelCause(ctx)

	go func() {
		defer close(done)
		defer innerCancel(fmt.Errorf("goroutine finished"))

		for {
			opt, err := ReadWithDoneCheck(innerCtx, in)
			if err != nil || opt.IsNone() {
				return
			}

			outVal, send := process(opt.MustGet())
			if send {
				if WriteWithDoneCheck(innerCtx, out, outVal) != nil {
					return
				}
			}
		}
	}()

	return in, func(cancelCtx context.Context) error {
		close(in)

		select {
		case <-done:
			return nil
		case <-cancelCtx.Done():
			innerCancel(fmt.Errorf("cancel ctx ended"))
			<-done
			return util.WrapCtxErrWithCause(cancelCtx)
		}
	}
}
