package verifier

import (
	"context"
	"testing"
)

type CheckRunner struct {
	checkDoneChan        chan error
	generationDoneChan   chan struct{}
	doNextGenerationChan chan struct{}
}

func RunVerifierCheck(ctx context.Context, t *testing.T, verifier *Verifier) *CheckRunner {
	verifierDoneChan := make(chan error)

	generationDoneChan := make(chan struct{})
	doNextGenerationChan := make(chan struct{})

	go func() {
		err := verifier.CheckDriver(ctx, nil, generationDoneChan, doNextGenerationChan)
		verifierDoneChan <- err
	}()

	return &CheckRunner{
		checkDoneChan:        verifierDoneChan,
		generationDoneChan:   generationDoneChan,
		doNextGenerationChan: doNextGenerationChan,
	}
}

// AwaitGenerationEnd blocks until the check’s current generation ends.
func (cr *CheckRunner) AwaitGenerationEnd() {
	<-cr.generationDoneChan
}

// StartNextGeneration blocks until it can tell the check to start
// the next generation.
func (cr *CheckRunner) StartNextGeneration() {
	cr.doNextGenerationChan <- struct{}{}
}

// Await will await generations and start new ones until the check
// finishes. It returns the error that verifier.CheckDriver() returns.
func (cr *CheckRunner) Await() error {
	for {
		select {
		case err := <-cr.checkDoneChan:
			return err

		case <-cr.generationDoneChan:
		case cr.doNextGenerationChan <- struct{}{}:
		}
	}
}
