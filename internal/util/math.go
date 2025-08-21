package util

import "github.com/10gen/migration-verifier/internal/types"

// DivideToF64 is syntactic sugar around float64(numerator) / float64(denominator).
func DivideToF64[N types.RealNumber, D types.RealNumber](numerator N, denominator D) float64 {
	return float64(numerator) / float64(denominator)
}
