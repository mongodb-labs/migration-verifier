package reportutils

// This package exposes a number of tools that facilitate consistent
// formatting in log reports.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/dustin/go-humanize"
	"golang.org/x/exp/constraints"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const decimalPrecision = 2

var realNumFmtPattern = "%." + strconv.Itoa(decimalPrecision) + "f"

var printer = message.NewPrinter(language.AmericanEnglish)

// num16Plus is like realNum, but it excludes 8-bit int/uint.
type num16Plus interface {
	constraints.Float |
		~uint | ~uint16 | ~uint32 | ~uint64 |
		~int | ~int16 | ~int32 | ~int64
}

type realNum interface {
	constraints.Float | constraints.Integer
}

// DataUnit signifies some unit of data.
type DataUnit string

const (
	Bytes DataUnit = "bytes"
	KiB   DataUnit = "KiB"
	MiB   DataUnit = "MiB"
	GiB   DataUnit = "GiB"
	TiB   DataUnit = "TiB"
	PiB   DataUnit = "PiB"

	// Anything larger than the above seems like overkill.
)

var unitSize = map[DataUnit]uint64{
	KiB: humanize.KiByte,
	MiB: humanize.MiByte,
	GiB: humanize.GiByte,
	TiB: humanize.TiByte,
	PiB: humanize.PiByte,
}

// DurationToHMS stringifies `duration` as, e.g., "1h 22m 3.23s".
// It’s a lot like Duration.String(), but with spaces between,
// and the lowest unit shown is always the second.
func DurationToHMS(duration time.Duration) string {

	hours := int(math.Floor(duration.Hours()))
	minutes := int(math.Floor(duration.Minutes())) % 60

	secs := math.Mod(duration.Seconds(), 60)

	str := FmtReal(secs) + "s"

	if hours > 0 {
		str = fmt.Sprintf("%dh %dm %s", hours, minutes, str)
	} else if minutes > 0 {
		str = fmt.Sprintf("%dm %s", minutes, str)
	}

	return str
}

// BytesToUnit returns a stringified number that represents `count`
// in the given `unit`. For example, count=1024 and unit=KiB would
// return "1".
func BytesToUnit[T num16Plus](count T, unit DataUnit) string {

	// Ideally go-humanize could do this for us,
	// but as of this writing it can’t.
	// https://github.com/dustin/go-humanize/issues/111

	// We could put Bytes into the unitSize map above and handle it
	// the same as other units, but that would entail int/float
	// conversion and possibly risk little rounding errors. We might
	// as well keep it simple where we can.

	var retval float64

	if unit == Bytes {
		retval = float64(count)
	} else {
		myUnitSize, exists := unitSize[unit]

		if !exists {
			panic(fmt.Sprintf("Missing unit in unitSize: %s", unit))
		}

		retval = float64(count) / float64(myUnitSize)
	}

	return FmtReal(retval)
}

// FmtReal provides a standard formatting of real numbers, with a consistent
// precision and trailing decimal zeros removed.
func FmtReal[T types.RealNumber](num T) string {
	return printer.Sprintf(realNumFmtPattern, num)
}

func fmtQuotient[T, U realNum](dividend T, divisor U) string {
	return FmtReal(float64(dividend) / float64(divisor))
}

// FmtPercent returns a stringified percentage without a trailing `%`,
// formatted as per FmtFloat(). FmtPercent also ensures that any
// percentage less than 100% is reported as something less; e.g.,
// 99.999997 doesn’t get rounded up to 100.
func FmtPercent[T, U realNum](numerator T, denominator U) string {
	str := fmtQuotient(100*numerator, denominator)

	// If the numerator & denominator are large then it’s possible
	// for str to be “100” without the numbers actually being equal.
	// For our purposes, though, “100” percent should mean the
	// denominator cannot exceed the numerator.
	//
	// (For now it’s ok to return “100” if the numerator exceeds the
	// denominator.)
	if str == "100" && (U(numerator) != denominator) {
		if U(numerator) < denominator {
			return "99." + strings.Repeat("9", decimalPrecision)
		}
	}

	return str
}

// FindBestUnit gives the “best” DataUnit for the given `count` of bytes.
//
// You can then give that DataUnit to BytesToUnit() to stringify
// multiple byte counts to the same unit.
func FindBestUnit[T num16Plus](count T) DataUnit {

	// humanize.IBytes() does most of what we want but lacks the
	// flexibility to specify a precision. It’s not complicated to
	// implement here anyway.

	if count < T(humanize.KiByte) {
		return Bytes
	}

	// If the log2 is, e.g., 32.05, we want to use 2^30, i.e., GiB.
	log2 := math.Log2(float64(count))

	// Convert log2 to the next-lowest multiple of 10.
	unitNum := 10 * uint64(math.Floor(log2/10))

	// Now find that power of 2, which we can compare against
	// the values of the unitSize map (above).
	unitNum = 1 << unitNum

	// Just in case, someday, exibytes become relevant …
	var biggestSize uint64
	var biggestUnit DataUnit

	for unit, size := range unitSize {
		if size == unitNum {
			return unit
		}

		if size > biggestSize {
			biggestSize = size
			biggestUnit = unit
		}
	}

	return biggestUnit
}

// FmtBytes is a convenience that combines BytesToUnit with FindBestUnit.
// Use it to format a single count of bytes.
func FmtBytes[T num16Plus](count T) string {
	unit := FindBestUnit(count)
	return BytesToUnit(count, unit) + " " + string(unit)
}
