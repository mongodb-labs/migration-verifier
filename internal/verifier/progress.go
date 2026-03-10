package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/verifier/api"
)

func (verifier *Verifier) GetProgress(ctx context.Context) (api.Progress, error) {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation := verifier.generation

	status, err := verifier.getVerificationStatusForGeneration(ctx, generation)
	if err != nil {
		return api.Progress{Error: err}, err
	}
	return api.Progress{
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
