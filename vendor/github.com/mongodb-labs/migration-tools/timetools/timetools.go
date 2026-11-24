package timetools

import (
	"fmt"
	"math"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"golang.org/x/exp/constraints"
)

type realNumber interface {
	constraints.Integer | constraints.Float
}

const (
	minDuration = time.Duration(math.MinInt64)
	maxDuration = time.Duration(math.MaxInt64)
)

// ToDuration multiplies the given count & duration, with proper handling
// of numeric types. Returns an error on overflow.
func ToDuration[T realNumber](count T, unit time.Duration) (time.Duration, error) {
	if unit <= 0 {
		return 0, fmt.Errorf("invalid time unit (%s): must be positive", unit)
	}

	if f := float64(count); math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid count (%v): must be finite", count)
	}

	countAsDuration, err := safecast.Convert[time.Duration](count)
	if err != nil {
		return 0, fmt.Errorf("cannot convert count %v to %T: %w", count, time.Duration(0), err)
	}

	// If the count is an integer value, then just convert to a Duration.
	// NB: This is what we want even if the Go type is a float.
	if T(countAsDuration) == count {
		return intToDuration(countAsDuration, unit)
	}

	// The count is a float, which takes a different workflow.
	result := float64(count) * float64(unit)

	if result < float64(minDuration) {
		return 0, fmt.Errorf("float underflow: %v * %s", count, unit)
	}
	// float64(maxDuration) rounds UP to 2^63 (MaxInt64 = 2^63-1 is not exactly
	// representable), so >= is required: result == float64(maxDuration) is already
	// an overflow, but > would miss it.
	if result >= float64(maxDuration) {
		return 0, fmt.Errorf("float overflow: %v * %s", count, unit)
	}
	return time.Duration(result), nil
}

func intToDuration(count, unit time.Duration) (time.Duration, error) {
	if count >= 0 {
		// The normal case: both positive

		maxCount := maxDuration / unit
		if count > maxCount {
			return 0, fmt.Errorf("integer overflow: %d * %s", count, unit)
		}
	} else {
		// Less normal: count negative, unit positive

		minCount := minDuration / unit

		if count < minCount {
			return 0, fmt.Errorf("integer underflow: %d * %s", count, unit)
		}
	}

	return unit * count, nil
}
