package retry

import (
	"time"
)

// Retryer handles retrying operations that fail because of network and UUID
// mismatch failures.
type Retryer struct {
	retryLimit              time.Duration
	retryRandomly           bool
	additionalErrorCodes    []int
	retryOnUUIDNotSupported bool
	aggregateDisallowsUUIDs bool
}

// New returns a new retryer.
func New(retryLimit time.Duration) Retryer {
	return NewWithRandomlyRetries(retryLimit, false)
}

// NewWithRandomlyRetries returns a new retryer, but allows the option of setting the
// retryRandomly field.
func NewWithRandomlyRetries(retryLimit time.Duration, retryRandomly bool) Retryer {
	return Retryer{
		retryLimit:           retryLimit,
		retryRandomly:        retryRandomly,
		additionalErrorCodes: nil,
	}
}

// WithErrorCodes returns a new Retryer that will retry on the codes passed to
// this method. This allows for a single function to customize the codes it
// wants to retry on. Note that if the Retryer already has additional custom
// error codes set, these are _replaced_ when this method is called.
func (r Retryer) WithErrorCodes(codes ...int) Retryer {
	return Retryer{
		retryLimit:           r.retryLimit,
		retryRandomly:        r.retryRandomly,
		additionalErrorCodes: codes,
	}
}

// Mongod versions older than 5.0 do not support collectionUUID in aggregate commands.  This method
// will result in not using the UUID and instead using the collection name in those cases.  Be
// warned that this will make the application vulnerable to inconsistencies due to collection
// renames.
//
// See also RequestWithUUID in retry.go
func (r Retryer) SetRetryOnUUIDNotSupported() Retryer {
	r.retryOnUUIDNotSupported = true
	return r
}
