package verifier

import (
	"context"
	"testing"

	"github.com/pkg/errors"
)

type CheckRunner struct {
	checkDoneChan        chan error
	generationDoneChan   chan struct{}
	doNextGenerationChan chan struct{}
}

// RunVerifierCheck starts a Check and returns a CheckRunner to track this
// Verifier.
//
// The next method to call is AwaitGenerationEnd.
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
func (cr *CheckRunner) AwaitGenerationEnd() error {
	select {
	case <-cr.generationDoneChan:
		return nil
	case err := <-cr.checkDoneChan:
		return errors.Wrap(err, "verifier failed while test awaited generation completion")
	}
}

// StartNextGeneration blocks until it can tell the check to start
// the next generation.
func (cr *CheckRunner) StartNextGeneration() error {
	select {
	case cr.doNextGenerationChan <- struct{}{}:
		return nil
	case err := <-cr.checkDoneChan:
		return errors.Wrap(err, "verifier failed while test waited to start next generation")
	}

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
