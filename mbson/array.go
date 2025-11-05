package mbson

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type arraySizeComputable interface {
	bson.Raw | bson.RawArray | bson.RawValue
}

// GetBSONArraySize returns the number of bytes needed to marshal the given
// slice to BSON.
func GetBSONArraySize[T arraySizeComputable](els []T) int {
	// 4 bytes for doc length
	// each element’s field name length
	// 2 extra bytes per element for BSON type & field name’s NUL
	// the element values
	// final NUL
	totalBytes := 0
	for _, el := range els {
		switch typedEl := any(el).(type) {
		case bson.Raw:
			totalBytes += len(typedEl)
		case bson.RawArray:
			totalBytes += len(typedEl)
		case bson.RawValue:
			totalBytes += len(typedEl.Value)
		default:
		}
	}

	return 4 + getBSONArrayTotalFieldNamesSize(len(els)) + 2*len(els) + totalBytes + 1
}

// This returns the total length, in bytes, of the document keys to
// marshal an array to BSON. For example, a 4-member array will have BSON keys
// "0", "1", "2", and "3", which total 4 bytes.
func getBSONArrayTotalFieldNamesSize(arrayLen int) int {
	if arrayLen < 10 {
		return arrayLen
	}

	bytes := 10

	if arrayLen < 100 {
		bytes += 2 * (arrayLen - 10)

		return bytes
	}

	// Each integer in [10..99] is 2-digit:
	bytes += 90 * 2

	if arrayLen < 1000 {
		bytes += 3 * (arrayLen - 100)

		return bytes
	}

	bytes += 900 * 3

	if arrayLen < 10000 {
		bytes += 4 * (arrayLen - 1000)

		return bytes
	}

	bytes += 9000 * 4

	if arrayLen < 100000 {
		bytes += 5 * (arrayLen - 10000)

		return bytes
	}

	bytes += 90000 * 5

	if arrayLen < 1000000 {
		bytes += 6 * (arrayLen - 100000)

		return bytes
	}

	panic(fmt.Sprintf("Unexpectedly high array length: %d", arrayLen))
}
