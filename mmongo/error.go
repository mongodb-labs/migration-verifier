package mmongo

import (
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/mongo"
)

// ErrorHasCode returns true if (and only if) this error is a
// mongo.ServerError that contains the given error code.
func ErrorHasCode[T ~int](err error, code T) bool {
	var serverError mongo.ServerError

	return errors.As(err, &serverError) && serverError.HasErrorCode(int(code))
}
