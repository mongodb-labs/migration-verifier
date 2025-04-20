package util

import "github.com/mongodb-labs/migration-verifier/internal/types"

// Divide is syntactic sugar around float64(numerator) / float64(denominator).
func Divide[N types.RealNumber, D types.RealNumber](numerator N, denominator D) float64 {
	return float64(numerator) / float64(denominator)
}
