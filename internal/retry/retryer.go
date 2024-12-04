package retry

import (
	"time"

	"github.com/10gen/migration-verifier/option"
)

// Retryer handles retrying operations that fail because of network failures.
type Retryer struct {
	retryLimit           time.Duration
	retryRandomly        bool
	before               option.Option[func()]
	additionalErrorCodes []int
}

// New returns a new retryer.
func New(retryLimit time.Duration) *Retryer {
	return NewWithRandomlyRetries(retryLimit, false)
}

// NewWithRandomlyRetries returns a new retryer, but allows the option of setting the
// retryRandomly field.
func NewWithRandomlyRetries(retryLimit time.Duration, retryRandomly bool) *Retryer {
	return &Retryer{
		retryLimit:    retryLimit,
		retryRandomly: retryRandomly,
	}
}

// WithErrorCodes returns a new Retryer that will retry on the codes passed to
// this method. This allows for a single function to customize the codes it
// wants to retry on. Note that if the Retryer already has additional custom
// error codes set, these are _replaced_ when this method is called.
func (r *Retryer) WithErrorCodes(codes ...int) *Retryer {
	r2 := *r
	r2.additionalErrorCodes = codes

	return &r2
}

// WithBefore sets a callback that always runs before any retryer callback.
//
// This is useful if there are multiple callbacks and you need to reset some
// condition before each retryer iteration. (In the single-callback case it’s
// largely redundant.)
func (r *Retryer) WithBefore(todo func()) *Retryer {
	r2 := *r
	r2.before = option.Some(todo)

	return &r2
}
