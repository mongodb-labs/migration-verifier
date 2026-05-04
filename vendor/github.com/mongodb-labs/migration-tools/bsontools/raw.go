package bsontools

import (
	"fmt"
	"iter"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// RawLookup extracts & unmarshals a referent value from a BSON document.
// It’s like bson.Raw.LookupErr combined with RawValueTo.
func RawLookup[T unmarshalTargets, D ~[]byte](in D, pointer ...string) (T, error) {
	doc := bson.Raw(in)

	rv, err := doc.LookupErr(pointer...)
	if err != nil {
		return *new(T), fmt.Errorf("extracting %#q: %w", pointer, err)
	}

	val, err := RawValueTo[T](rv)
	if err != nil {
		return *new(T), fmt.Errorf("casting %#q: %w", pointer, err)
	}

	return val, err
}

// CountRawElements returns a count of the fields in the given BSON document.
func CountRawElements[D ~[]byte](doc D) (int, error) {
	elsCount := 0

	for _, err := range RawElements(doc) {
		if err != nil {
			return 0, err
		}

		elsCount++
	}

	return elsCount, nil
}

// RawElements returns an iterator over a Raw’s elements.
//
// An empty buffer is treated as a valid empty BSON document (equivalent to
// the 5-byte all-NUL canonical form): no error, no elements yielded.
//
// Otherwise the document is rejected with an error if:
//   - It is too short to contain a BSON length header (4 bytes).
//   - The declared length is below the BSON minimum (5 bytes).
//   - The declared length does not match the actual buffer length.
//   - The final byte is not the required 0x00 document terminator.
//   - An element fails to parse or validate.
//
// If the iterator returns an error but the caller continues iterating,
// a panic will ensue.
func RawElements[D ~[]byte](doc D) iter.Seq2[bson.RawElement, error] {
	return func(yield func(bson.RawElement, error) bool) {
		yieldErr := func(err error) {
			if yield(nil, err) {
				panic(fmt.Errorf("must stop iteration after error (%w)", err))
			}
		}

		if len(doc) == 0 {
			return
		}

		length, rem, ok := bsoncore.ReadLength(doc)
		if !ok {
			yieldErr(fmt.Errorf(
				"%w (buffer is only %d bytes long)",
				bsoncore.NewInsufficientBytesError(doc, rem),
				len(doc),
			))
			return
		}

		if length < 5 {
			yieldErr(fmt.Errorf(
				"declared document length (%d) is below BSON minimum (5)",
				length,
			))
			return
		}

		if int(length) != len(doc) {
			yieldErr(fmt.Errorf(
				"declared document length (%d) mismatches actual buffer length (%d)",
				length,
				len(doc),
			))
			return
		}

		if doc[len(doc)-1] != 0 {
			yieldErr(fmt.Errorf(
				"BSON document missing trailing NUL terminator (last byte is 0x%02x)",
				doc[len(doc)-1],
			))
			return
		}

		rawElementsLoop(bson.Raw(doc), yield, yieldErr)
	}
}

// This is a separate function in order to mitigate cyclomatic complexity
// in RawElements.
func rawElementsLoop(
	doc bson.Raw,
	yield func(bson.RawElement, error) bool,
	yieldErr func(err error),
) {
	// Exclude the trailing 0x00 terminator from the iteration buffer so a
	// malformed last element cannot silently consume it.
	remaining := doc[4 : len(doc)-1]

	var el bsoncore.Element
	var ok bool

	for len(remaining) > 0 {
		el, remaining, ok = bsoncore.ReadElement(remaining)

		var err error
		if !ok {
			err = bsoncore.NewInsufficientBytesError(doc, remaining)
		} else {
			err = el.Validate()
		}

		if err != nil {
			yieldErr(err)
			return
		}

		if !yield(bson.RawElement(el), nil) {
			return
		}
	}
}
