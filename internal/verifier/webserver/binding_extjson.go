package webserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin/binding"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type extJSONBindingType struct{}

var ExtJSONBinding = extJSONBindingType{}

var _ binding.Binding = extJSONBindingType{}
var _ binding.BindingBody = extJSONBindingType{}

func (extJSONBindingType) Name() string {
	return "extJSON"
}

func (ejb extJSONBindingType) Bind(req *http.Request, obj any) error {
	if req == nil || req.Body == nil {
		return errors.New("invalid request")
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("reading HTTP request body: %w", err)
	}

	return ejb.BindBody(body, obj)
}

func (extJSONBindingType) BindBody(body []byte, obj any) error {
	err := bson.UnmarshalExtJSON(body, false, obj)

	// This validation logic is written out manually in gin’s bindings.
	// We duplicate it here.
	if err == nil && binding.Validator != nil {
		err = binding.Validator.ValidateStruct(obj)
	}

	return err
}
