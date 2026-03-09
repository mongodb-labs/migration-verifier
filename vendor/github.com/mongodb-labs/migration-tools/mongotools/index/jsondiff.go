package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wI2L/jsondiff"
)

func describeJSONDiff(a, b []byte) (string, error) {
	patch, err := jsondiff.CompareJSON(
		a,
		b,
		jsondiff.Factorize(),
	)
	if err != nil {
		return "", fmt.Errorf("creating JSON patch: %w", err)
	}

	patchStr, err := fixJSONPatchFieldOrder([]byte(patch.String()))
	if err != nil {
		patchStr = fmt.Appendf(nil, "%s (failed to normalize order: %v)", patch.String(), err)
	}

	return string(patchStr), nil
}

// The jsondiff library’s patch.String() puts the fields in a weird order:
//
//	{"value":true,"op":"add","path":"/sparse"}
//
// This orders the fields more logically.
func fixJSONPatchFieldOrder(in []byte) ([]byte, error) {
	patchOrderStruct := struct {
		Op    string  `json:"op"`
		From  *string `json:"from,omitempty"`
		Path  string  `json:"path"`
		Value *any    `json:"value,omitempty"`
	}{}

	decoder := json.NewDecoder(bytes.NewBuffer(in))
	var out []byte

	for {
		toZero(&patchOrderStruct)

		err := decoder.Decode(&patchOrderStruct)

		switch {
		case err == nil:
			if len(out) > 0 {
				out = append(out, '\n')
			}

			cur, err := json.Marshal(patchOrderStruct)
			if err != nil {
				return nil, fmt.Errorf("marshaling operation: %w", err)
			}

			out = append(out, cur...)
		case errors.Is(err, io.EOF):
			return out, nil
		default:
			return nil, fmt.Errorf("unmarshaling JSON patch operation: %w", err)
		}
	}
}

func toZero[T any](v *T) {
	*v = *new(T)
}
