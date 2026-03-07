package bsontools

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/ccoveille/go-safecast/v2"
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

// elementInfo holds the position and metadata of a BSON element within a raw document.
type elementInfo struct {
	pos       int
	valueAt   int
	valueSize int
	keyBytes  []byte
	bsonType  bson.Type
}

// subDocResult holds the output of recursing into an embedded document or array.
type subDocResult[T ~[]byte] struct {
	raw        T
	bytesAdded int32
	found      bool
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

func replaceOrRemoveInRaw[T ~[]byte](
	raw T,
	replacement *bson.RawValue,
	pointer []string,
) (T, bool, error) {
	sizeFromHeader, _, ok := bsoncore.ReadLength(raw)
	if !ok {
		return nil, false, fmt.Errorf("too few bytes to read BSON length")
	}

	return editMatchingElement(raw, replacement, pointer, sizeFromHeader)
}

func editMatchingElement[T ~[]byte](
	raw T,
	replacement *bson.RawValue,
	pointer []string,
	sizeFromHeader int32,
) (T, bool, error) {
	pos := 4
	for pos < int(sizeFromHeader)-1 {
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

		info := elementInfo{
			pos:       pos,
			valueAt:   valueAt,
			valueSize: len(el) - len(keyBytes) - 2,
			keyBytes:  keyBytes,
			bsonType:  bson.Type(el[0]),
		}

		var bytesAdded int32
		var err error

		// If this is the last node in the doc pointer, then remove/replace the element.
		//
		//nolint:nestif
		if len(pointer) == 1 {
			if replacement == nil {
				raw, bytesAdded, err = removeElement(raw, info)
			} else {
				raw, bytesAdded, err = replaceElement(raw, *replacement, info)
			}
			if err != nil {
				return raw, false, err
			}
		} else {
			result, err := recurseIntoSubDoc(raw, replacement, pointer, info)
			if err != nil {
				return raw, false, err
			}
			if !result.found {
				return raw, false, nil
			}
			raw = result.raw
			bytesAdded = result.bytesAdded
		}

		newSize, err := safecast.Convert[uint32](sizeFromHeader + bytesAdded)
		if err != nil {
			return raw, false, err
		}

		binary.LittleEndian.PutUint32(raw, newSize)

		return raw, true, nil
	}

	return raw, false, nil
}

func removeElement[T ~[]byte](raw T, info elementInfo) (T, int32, error) {
	raw = slices.Delete(raw, info.pos, info.valueAt+info.valueSize)
	bytesAdded, err := safecast.Convert[int32](-info.valueSize - len(info.keyBytes) - 2)
	return raw, bytesAdded, err
}

func replaceElement[T ~[]byte](
	raw T,
	replacement bson.RawValue,
	info elementInfo,
) (T, int32, error) {
	raw[info.pos] = byte(replacement.Type)
	bytesAdded, err := safecast.Convert[int32](len(replacement.Value) - info.valueSize)
	if err != nil {
		return raw, 0, err
	}
	raw = slices.Replace(raw, info.valueAt, info.valueAt+info.valueSize, replacement.Value...)
	return raw, bytesAdded, nil
}

func recurseIntoSubDoc[T ~[]byte](
	raw T,
	replacement *bson.RawValue,
	pointer []string,
	info elementInfo,
) (subDocResult[T], error) {
	if info.bsonType != bson.TypeArray && info.bsonType != bson.TypeEmbeddedDocument {
		return subDocResult[T]{}, PointerTooDeepError{
			givenPointer:   slices.Clone(pointer),
			elementType:    info.bsonType,
			elementPointer: slices.Clone(pointer[:1]),
		}
	}

	curDoc := raw[info.valueAt:]
	oldDocSize, _, ok := bsoncore.ReadLength(curDoc)
	if !ok {
		return subDocResult[T]{}, fmt.Errorf("old embedded doc too short to read length")
	}

	curDoc, found, err := replaceOrRemoveInRaw(curDoc, replacement, pointer[1:])
	if err != nil {
		var pe PointerTooDeepError
		if errors.As(err, &pe) {
			pe.givenPointer = append(slices.Clone(pointer[:1]), pe.givenPointer...)
			pe.elementPointer = append(slices.Clone(pointer[:1]), pe.elementPointer...)
			return subDocResult[T]{}, pe
		}

		return subDocResult[T]{}, fmt.Errorf("replace %#q: %w", pointer[1:], err)
	}

	if !found {
		return subDocResult[T]{}, nil
	}

	newDocSize, _, ok := bsoncore.ReadLength(curDoc)
	if !ok {
		return subDocResult[T]{}, fmt.Errorf("new embedded doc too short to read length")
	}

	return subDocResult[T]{
		raw:        append(raw[:info.valueAt], curDoc...),
		bytesAdded: newDocSize - oldDocSize,
		found:      true,
	}, nil
}
