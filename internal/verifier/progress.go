package verifier

import "context"

func (verifier *Verifier) GetProgress(ctx context.Context) (Progress, error) {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation := verifier.generation

	status, err := verifier.getVerificationStatusForGeneration(ctx, generation)
	if err != nil {
		return Progress{Error: err}, err
	}
	return Progress{
		Phase:      verifier.getPhaseWhileLocked(),
		Generation: verifier.generation,
		Status:     status,
	}, nil
}

func (verifier *Verifier) getPhaseWhileLocked() string {
	verifier.assertLocked()

	if !verifier.running {
		return Idle
	}

	if verifier.generation > 0 {
		return Recheck
	}

	return Check
}
