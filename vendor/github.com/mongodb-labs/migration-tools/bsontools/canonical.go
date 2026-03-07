package bsontools

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SortFields sorts a BSON document’s fields recursively.
// It modifies the provided bson.Raw directly.
func SortFields(in bson.Raw) error {
	return sortInPlaceInternal(in, false)
}

func EqualIgnoringOrder(a, b bson.Raw) (bool, error) {
	sortedA := slices.Clone(a)
	if err := SortFields(sortedA); err != nil {
		return false, fmt.Errorf("sorting A’s fields: %w", err)
	}

	sortedB := slices.Clone(b)
	if err := SortFields(sortedB); err != nil {
		return false, fmt.Errorf("sorting B’s fields: %w", err)
	}

	return bytes.Equal(sortedA, sortedB), nil
}

func sortInPlaceInternal(in bson.Raw, isArray bool) error {
	els, err := in.Elements()
	if err != nil {
		return fmt.Errorf("parsing fields: %w", err)
	}

	if len(els) == 0 {
		return nil
	}

	// 1. Recursively sort nested documents and arrays IN PLACE.
	err = iterateElements(els)
	if err != nil {
		return fmt.Errorf("iterating: %w", err)
	}

	// If this current document is an array, we DO NOT sort its keys
	// (they are "0", "1", "2"), but we did need to sort its contents above.
	if isArray {
		return nil
	}

	// 2. Parse keys once to avoid O(N log N) re-parsing during the sort.
	type parsedField struct {
		key string
		el  bson.RawElement
	}

	fields := make([]parsedField, 0, len(els))
	for _, el := range els {
		key := el.Key()
		fields = append(fields, parsedField{
			key: key,
			el:  el, // Note: el bytes might have been mutated by the recursion above!
		})
	}

	// 3. Sort the metadata based on the keys
	slices.SortStableFunc(fields, func(a, b parsedField) int {
		return cmp.Compare(a.key, b.key)
	})

	// 4. Safely rearrange the bytes.
	// We calculate the exact size of the elements block (excluding the 4-byte
	// document size header and the 1-byte null terminator).
	elementsBlockSize := len(in) - 5
	if elementsBlockSize <= 0 {
		return nil
	}

	// We use a scratch buffer to hold the newly ordered bytes. This prevents
	// variable-length elements from overwriting each other during the swap.
	scratch := make([]byte, 0, elementsBlockSize)
	for _, field := range fields {
		scratch = append(scratch, field.el...)
	}

	// 5. Overwrite the original elements space with the sorted bytes.
	copy(in[4:len(in)-1], scratch)

	return nil
}

func iterateElements(els []bson.RawElement) error {
	for _, el := range els {
		bType := bson.Type(el[0])

		if bType != bson.TypeEmbeddedDocument && bType != bson.TypeArray {
			continue
		}

		// These can’t fail since Elements() validated already.
		// NB: Since `val` is a sub-slice of `in`, modifying it modifies `in`.
		key := el.Key()
		val := el.Value()

		// Whether the value is a subdocument or an array, it’s a valid BSON
		// document.
		bsonBuf := val.Value

		if err := sortInPlaceInternal(bsonBuf, bType == bson.TypeArray); err != nil {
			return fmt.Errorf("sorting subdoc %#q: %w", key, err)
		}
	}

	return nil
}
