package bsontools

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// PointerTooDeepError is like POSIX’s ENOTDIR error: it indicates that a
// nonfinal element in a document pointer is a scalar value.
type PointerTooDeepError struct {
	givenPointer []string

	elementType    bson.Type
	elementPointer []string
}

func (pe PointerTooDeepError) Error() string {
	return fmt.Sprintf(
		"cannot delete %#q from BSON doc because %#q is of a simple type (%s)",
		pe.givenPointer,
		pe.elementPointer,
		pe.elementType,
	)
}

// ReplaceInRaw “surgically” replaces one value in a BSON document with another.
// Its returned bool indicates whether the value was found.
//
// If any nonfinal node in the pointer is a scalar value (i.e., neither an array
// nor embedded document), a PointerTooDeepError is returned.
//
// Example usage (replaces /role/title):
//
//	rawDoc, found, err = ReplaceInRaw(rawDoc, newRoleTitle, "role", "title")
func ReplaceInRaw[T ~[]byte](raw T, newValue bson.RawValue, pointer ...string) (T, bool, error) {
	return replaceOrRemoveInRaw(raw, &newValue, pointer)
}

// RemoveFromRaw is like ReplaceInRaw, but it removes the element.
//
// Example usage (replaces /role/title):
//
//	rawDoc, found, err = RemoveFromRaw(rawDoc, "role", "title")
func RemoveFromRaw[T ~[]byte](raw T, pointer ...string) (T, bool, error) {
	return replaceOrRemoveInRaw(raw, nil, pointer)
}

func replaceOrRemoveInRaw[T ~[]byte](raw T, replacement *bson.RawValue, pointer []string) (T, bool, error) {
	sizeFromHeader, _, ok := bsoncore.ReadLength(raw)
	if !ok {
		return nil, false, fmt.Errorf("too few bytes to read BSON length")
	}

	pos := 4
	for pos < len(raw)-1 {
		el, _, ok := bsoncore.ReadElement(raw[pos:])
		if !ok {
			return nil, false, fmt.Errorf("invalid BSON element at offset %d", pos)
		}

		keyBytes := el.KeyBytes()

		valueAt := pos + 1 + len(keyBytes) + 1

		if string(keyBytes) != pointer[0] {
			pos += len(el)

			continue
		}

		bsonType := bson.Type(el[0])
		valueSize := len(el) - len(keyBytes) - 2

		var bytesAdded int32

		// If this is the last node in the doc pointer, then remove/replace
		// the element.
		if len(pointer) == 1 {
			if replacement == nil {
				raw = slices.Delete(raw, pos, valueAt+valueSize)
				bytesAdded = int32(-valueSize - len(keyBytes) - 2)
			} else {
				raw[pos] = byte(replacement.Type)

				bytesAdded = int32(len(replacement.Value) - valueSize)

				raw = slices.Replace(
					raw,
					valueAt,
					valueAt+valueSize,
					replacement.Value...,
				)
			}
		} else {
			if bsonType != bson.TypeArray && bsonType != bson.TypeEmbeddedDocument {
				return nil, false, PointerTooDeepError{
					givenPointer:   slices.Clone(pointer),
					elementType:    bsonType,
					elementPointer: slices.Clone(pointer[:1]),
				}
			}

			curDoc := raw[valueAt:]
			oldDocSize, _, ok := bsoncore.ReadLength(curDoc)
			if !ok {
				return nil, false, fmt.Errorf("old embedded doc too short to read length")
			}

			var found bool
			var err error
			curDoc, found, err = replaceOrRemoveInRaw(curDoc, replacement, pointer[1:])
			if err != nil {
				var pe PointerTooDeepError
				if errors.As(err, &pe) {
					pe.givenPointer = append(
						slices.Clone(pointer[:1]),
						pe.givenPointer...,
					)
					pe.elementPointer = append(
						slices.Clone(pointer[:1]),
						pe.elementPointer...,
					)

					return raw, false, pe
				}
			}

			if !found {
				return raw, false, nil
			}

			newDocSize, _, ok := bsoncore.ReadLength(curDoc)
			if !ok {
				return nil, false, fmt.Errorf("new embedded doc too short to read length")
			}

			bytesAdded = newDocSize - oldDocSize

			raw = append(
				raw[:valueAt],
				curDoc...,
			)
		}

		binary.LittleEndian.PutUint32(raw, uint32(sizeFromHeader+bytesAdded))

		return raw, true, nil
	}

	return raw, false, nil
}
