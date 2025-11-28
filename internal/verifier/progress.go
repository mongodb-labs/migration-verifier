package verifier

import (
	"context"
	"time"
)

func (verifier *Verifier) GetProgress(ctx context.Context) (Progress, error) {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation := verifier.generation
	vStatus, err := verifier.getVerificationStatusForGeneration(ctx, generation)

	progressTime := time.Now()
	genElapsed := progressTime.Sub(verifier.generationStartTime)

	/*
		status, err := verifier.GetVerificationStatus(ctx)
		if err != nil {
			return Progress{Error: err}, err
		}
		return Progress{
			Phase:      verifier.phase,
			Generation: verifier.generation,
			Status:     status,
		}, nil
	*/
}
