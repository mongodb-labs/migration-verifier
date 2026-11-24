package option

import (
	"bytes"

	"github.com/goccy/go-yaml"
)

// UnmarshalYAML decodes an Option from YAML: a null (or an absent key, which never calls this
// method at all) yields None. Note the value is decoded directly rather than via *T: goccy silently
// leaves a **T nil when the document holds a scalar.
func (o *Option[T]) UnmarshalYAML(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*o = None[T]()
		return nil
	}

	var val T
	if err := yaml.Unmarshal(b, &val); err != nil {
		return err
	}
	*o = Some(val)
	return nil
}
