package retry

import (
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/option"
)

type retryCallbackInfo struct {
	callback    RetryCallback
	description string
}

// Retryer handles retrying operations that fail because of network failures.
type Retryer struct {
	retryLimit           time.Duration
	before               option.Option[func() error]
	callbacks            []retryCallbackInfo
	description          option.Option[string]
	additionalErrorCodes []int
}

// New returns a new Retryer with DefaultDurationLimit as its time limit.
func New() *Retryer {
	return &Retryer{
		retryLimit: DefaultDurationLimit,
	}
}

// WithErrorCodes returns a new Retryer that will retry on the codes passed to
// this method. This allows for a single retryer to customize the codes it
// wants to retry on. Note that if the Retryer already has additional custom
// error codes set, these are _replaced_ when this method is called.
func (r *Retryer) WithErrorCodes(codes ...int) *Retryer {
	r2 := r.clone()
	r2.additionalErrorCodes = codes

	return r2
}

// WithRetryLimit returns a new retryer with the specified time limit.
func (r *Retryer) WithRetryLimit(limit time.Duration) *Retryer {
	r2 := r.clone()
	r2.retryLimit = limit

	return r2
}

// WithBefore returns a new retryer with a callback that always runs before
// any retryer callback.
//
// This is useful if there are multiple callbacks and you need to reset some
// condition before each retryer iteration. (In the single-callback case it’s
// largely redundant.)
func (r *Retryer) WithBefore(todo func() error) *Retryer {
	r2 := r.clone()
	r2.before = option.Some(todo)

	return r2
}

// WithDescription returns a new retryer with the given description.
func (r *Retryer) WithDescription(msg string, args ...any) *Retryer {
	r2 := r.clone()
	r2.description = option.Some(fmt.Sprintf(msg, args...))

	return r2
}

// WithCallback returns a new retryer with the additional callback.
func (r *Retryer) WithCallback(
	callback RetryCallback,
	msg string, args ...any,
) *Retryer {
	r2 := r.clone()

	r2.callbacks = append(
		r2.callbacks,
		retryCallbackInfo{
			callback:    callback,
			description: fmt.Sprintf(msg, args...),
		},
	)

	return r2
}

func (r *Retryer) clone() *Retryer {
	r2 := *r

	r2.before = option.FromPointer(r.before.ToPointer())
	r2.description = option.FromPointer(r.description.ToPointer())
	r2.callbacks = slices.Clone(r.callbacks)
	r2.additionalErrorCodes = slices.Clone(r.additionalErrorCodes)

	return &r2
}
