package util

import (
	"context"
	"io"
	"net"
	"strings"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"
)

// If we need to refer to more codes outside of this code, for example to add
// additional codes to the retryer, make sure to add the codes here instead of
// just using bare integers. In the future, it might make sense to use a
// `ErrorCode` newtype, but that requires a more invasive change to everything
// that uses error codes.
const (
	LockFailed              int = 107
	SampleTooManyDuplicates int = 28799
)

//
// Helpers for common server errors that mongosync encounters. All server error codes can be found at:
// https://github.com/mongodb/mongo/blob/12fc3ef71b375ce012ad639ee1adfed555819503/src/mongo/base/error_codes.yml
//

// IsHTTPClientTimeoutError returns true if this error is a http client timeout error.
func IsHTTPClientTimeoutError(err error) bool {
	return strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers")
}

// IsDuplicateKeyError returns true if this error is a DuplicateKeyError.
func IsDuplicateKeyError(err error) bool {
	return mongo.IsDuplicateKeyError(err)
}

// IsIndexConflictError returns true if this is an IndexConflictError.
//
// IndexOptionsConflict = 85
// IndexKeySpecsConflict = 86
func IsIndexConflictError(err error) bool {
	code := GetErrorCode(err)
	return code == 85 || code == 86
}

// IsIndexNotFoundError returns true if this is an IndexNotFoundError.
//
// IndexNotFound = 27
func IsIndexNotFoundError(err error) bool {
	return GetErrorCode(err) == 27
}

// IsNamespaceExistsError returns true if this is a NamespaceExistsError.
func IsNamespaceExistsError(err error) bool {
	code := GetErrorCode(err)
	// TODO (REP-942): Remove error 17399 once SERVER-64416 is fixed in all supported server versions.
	return code == 48 || code == 17399
}

// IsNamespaceNotFoundError returns true if this is a NamspaceNotFoundError.
func IsNamespaceNotFoundError(err error) bool {
	return GetErrorCode(err) == 26
}

// IsOptionNotSupportedOnView returns true if this is a OptionNotSupportedOnViewError.
func IsOptionNotSupportedOnView(err error) bool {
	return GetErrorCode(err) == 167
}

// IsNoDocumentsError returns true if this is a ErrNoDocuments.
func IsNoDocumentsError(err error) bool {
	return err == mongo.ErrNoDocuments
}

// IsFailedToParseError returns true if this is a FailedToParseError.
func IsFailedToParseError(err error) bool {
	return GetErrorCode(err) == 9
}

// IsContextCanceledError returns true if this is a Context Canceled error.
func IsContextCanceledError(err error) bool {
	return strings.Contains(err.Error(), context.Canceled.Error())
}

func isRetryablePoolError(err error) bool {
	rerr, ok := err.(driver.RetryablePoolError)
	return ok && rerr.Retryable()
}

func isFailedToSatisfyReadPreferenceError(err error) bool {
	return GetErrorCode(err) == 133
}

func isServerSelectionError(err error) bool {
	_, ok := err.(topology.ServerSelectionError)
	return ok
}

func isConnectionError(err error) bool {
	if connErr, ok := err.(topology.ConnectionError); ok {
		// Network errors are usually wrapped inside ConnectionError instead of being at top-level.
		return isNetworkError(connErr.Wrapped)
	}

	return false
}

// IsTransientError returns true if this is an error that is reconnectable and can be retried.
func IsTransientError(err error) bool {
	// Find the root cause.
	err = errors.Cause(err)
	if err == nil {
		return false
	}

	if IsContextCanceledError(err) {
		return false
	}

	// All w:majority write concern errors are retryable.
	if _, ok := err.(*mongo.WriteConcernError); ok {
		return true
	}

	// Retry on network errors, e.g. no reachable servers,
	// connection reset by peer, operation timed out, etc.
	if isNetworkError(err) {
		return true
	}

	if isConnectionError(err) {
		return true
	}

	if hasTransientErrorCode(err) {
		return true
	}

	if hasTransientErrorLabel(err) {
		return true
	}

	if isRetryablePoolError(err) {
		return true
	}

	if isServerSelectionError(err) {
		return true
	}

	if isFailedToSatisfyReadPreferenceError(err) {
		return true
	}

	return false
}

// isNetworkError returns true if this is a NetworkError.
func isNetworkError(err error) bool {
	// Connection errors from syscalls, connection reset by peer, etc.
	if _, ok := err.(net.Error); ok {
		return true
	}

	// XXX - some of these, especially the specific strings, may not be relevant, as they come
	// from old packages like the mgo driver. But we're not sure if they may surface from other
	// sources as well.
	if err == io.EOF || err.Error() == "no reachable servers" || err.Error() == "Closed explicitly" {
		return true
	}

	// XXX - similarly, this comes from spacemonkeygo/openssl.
	if err == io.ErrUnexpectedEOF || err.Error() == "connection closed" {
		return true
	}

	// Network errors from the driver
	return mongo.IsNetworkError(err)
}

// hasTransientErrorCode returns true if the error has one of a set of known-to-be-transient
// Mongo server error codes.
func hasTransientErrorCode(err error) bool {
	switch GetErrorCode(err) {
	case 6, 7, 64, 89, 91, 112, 136, 175, 189, 202, 262, 290, 314, 317,
		9001, 10107, 11600, 11601, 11602, 13388, 13435, 13436:
		// These error codes are either listed as retryable in the remote command retry
		// scheduler, or have been added here deliberately, since they have been observed to be
		// issued when applyOps/find/getMore is interrupted while the server is being shut
		// down.
		//
		// There is a list of error codes at
		// https://github.com/mongodb/mongo/blob/master/src/mongo/base/error_codes.yml. The
		// list below includes all codes that are in the NetworkError and RetriableError
		// categories, except 358 (InternalTransactionNotSupported) and 50915
		// (BackupCursorOpenConflictWithCheckpoint), as these do not apply to any operations
		// performed by mongosync.
		//
		// 6      HostUnreachable
		// 7      HostNotFound
		// 64     WriteConcernFailed
		// 89     NetworkTimeout
		// 91     ShutdownInProgress
		// 112    WriteConflict
		// 136    CappedPositionLost - XXX - there was some discussion over whether this should be included
		// 175    QueryPlanKilled, e.g. when a collection is dropped/renamed while a cursor is open on it
		// 189    PrimarySteppedDown
		// 202    NetworkInterfaceExceededTimeLimit
		// 262    ExceededTimeLimit
		// 290    TransactionExceededLifetimeLimitSeconds
		// 314    ObjectIsBusy
		// 317    ConnectionPoolExpired
		// 9001   SocketException
		// 10107  NotWritablePrimary
		// 11600  InterruptedAtShutdown
		// 11601  Interrupted
		// 11602  InterruptedDueToReplStateChange
		// 13388  StaleConfig
		// 13435  NotPrimaryNoSecondaryOk
		// 13436  NotPrimaryOrSecondary
		return true
	case 0:
		// The server may send "not master" without an error code.
		if strings.Contains(err.Error(), "not master") {
			return true
		}
	// These codes only apply to DDL operations. However, we decided that
	// there's no harm in including them in the default list. See REP-1289 for
	// more details.
	case 63, 117, 12586, 12587:
		// 63	  OBSOLETE_StaleShardVersion
		// 117	  ConflictingOperationInProgress
		// 12586  BackgroundOperationInProgressForDatabase
		// 12587  BackgroundOperationInProgressForNamespace
		return true
	}
	return false
}

// These labels come from the mongo source code at
// https://github.com/mongodb/mongo/blob/97900f2f11d0399cef7b36a2644eee3562f1ae41/src/mongo/db/error_labels.h. Note
// that the IsNetworkError() func already checks for the "NetworkError" label under the hood,
// so we don't need to include that here.
var transientErrorLabels = [3]string{
	"ResumableChangeStreamError",
	"RetryableWriteError",
	"TransientTransactionError",
}

// hasTransientErrorLabel returns true if the error is a mongo.ServerError with a label
// indicating a transient error.
func hasTransientErrorLabel(err error) bool {
	if err, ok := err.(mongo.ServerError); ok {
		for _, l := range transientErrorLabels {
			if err.HasErrorLabel(l) {
				return true
			}
		}
	}
	return false
}

// IsCollectionUUIDMismatchError returns true if this is a CollectionUUIDMismatchError.
func IsCollectionUUIDMismatchError(err error) bool {
	return GetErrorCode(err) == 361
}

// IsServerError returns true if the error implements the ServerError interface in driver.
func IsServerError(err error) bool {
	// Get the cause of the err.
	cause := errors.Cause(err)
	_, ok := cause.(mongo.ServerError)

	return ok
}

// IsCommandNotSupportedOnViewError returns true if this is a CommandNotSupportedOnView error.
func IsCommandNotSupportedOnViewError(err error) bool {
	return GetErrorCode(err) == 166
}

// GetErrorCode returns the error code corresponding to the provided error.
// It returns 0 if the error is nil or not one of the supported error types.
func GetErrorCode(err error) int {
	switch e := errors.Cause(err).(type) {
	case mongo.CommandError:
		return int(e.Code)
	case driver.Error:
		return int(e.Code)
	case driver.WriteCommandError:
		for _, we := range e.WriteErrors {
			return int(we.Code)
		}
		if e.WriteConcernError != nil {
			return int(e.WriteConcernError.Code)
		}
		return 0
	case driver.QueryFailureError:
		codeVal, err := e.Response.LookupErr("code")
		if err == nil {
			code, _ := codeVal.Int32OK()
			return int(code)
		}
		return 0 // this shouldn't happen
	case mongo.WriteError:
		return e.Code
	case mongo.BulkWriteError:
		return e.Code
	case mongo.WriteConcernError:
		return e.Code
	case mongo.WriteException:
		for _, we := range e.WriteErrors {
			return GetErrorCode(we)
		}
		if e.WriteConcernError != nil {
			return e.WriteConcernError.Code
		}
		return 0
	case mongo.BulkWriteException:
		// Return the first error code.
		for _, ecase := range e.WriteErrors {
			return GetErrorCode(ecase)
		}
		if e.WriteConcernError != nil {
			return e.WriteConcernError.Code
		}
		return 0
	default:
		return 0
	}
}

// HasServerErrorMessage returns true if the error is a mongo ServerError and contains the specified
// error message.
func HasServerErrorMessage(err error, message string) bool {
	cause := errors.Cause(err)
	serverErr, isServerErr := cause.(mongo.ServerError)
	if !isServerErr || serverErr == nil {
		return false
	}
	return serverErr.HasErrorMessage(message)
}

// GetActualCollectionFromCollectionUUIDMismatchError returns the value of the `actualCollection`
// field in a CollectionUUIDMismatch server error. It must only be called on this error.
//
// We expect to get this error back in two cases:
//
//	(1) If a collection is renamed.
//	(2) If a collection is dropped.
//
// In case (1) the collection still exists, so we can look up the `actualCollection` field
// in the CollectionUUIDMismatch error to get the collection's current name.
//
// In case (2) the collection doesn't exist, so there will be no `actualCollection` field.
// We return an empty collection name here.
func GetActualCollectionFromCollectionUUIDMismatchError(logger *logger.Logger, err error) (string, error) {
	// Get the root error.
	err = errors.Cause(err)
	// XXX - commented out for now because we cannot call util functions from
	// this package (error) since util calls error.
	//Invariant(logger, IsCollectionUUIDMismatchError(err), "GetActualCollectionFromCollectionUUIDMismatchError must be called with a UUIDMismatchError, received %s", err)

	actualCollection, lookupErr := getErrorRaw(err).LookupErr("actualCollection")

	if actualCollection.Type == bson.TypeNull {
		return "", nil
	}

	actualCollectionString, ok := actualCollection.StringValueOK()

	if !ok {
		return "", errors.New("actualCollection must be a string, received " + actualCollection.Type.String())
	}

	return actualCollectionString, lookupErr
}

func getErrorRaw(err error) bson.Raw {
	switch e := err.(type) {
	// A normal command error.
	case mongo.CommandError:
		return e.Raw

	// A driver error.
	case driver.Error:
		return bson.Raw(e.Raw)

	// A write concern error.
	case mongo.WriteConcernError:
		return e.Raw

	// A single write error.
	case mongo.WriteError:
		return e.Raw

	// Errors with 1 or more above writer errors.
	// We return the first write error's Raw error.
	case driver.WriteCommandError:
		for _, we := range e.WriteErrors {
			return getErrorRaw(we)
		}
		return nil
	case mongo.WriteException:
		for _, we := range e.WriteErrors {
			return getErrorRaw(we)
		}
		return nil

	// A bulk write error, consisting
	// of a single write error.
	case mongo.BulkWriteError:
		return e.WriteError.Raw

	// An error with 1 or more bulk write errors.
	// We return the first bulk write error's Raw error.
	case mongo.BulkWriteException:
		for _, we := range e.WriteErrors {
			return getErrorRaw(we)
		}
		return nil

	// No other error types have Raw errors.
	default:
		return nil
	}
}

// IsStaleClusterTimeError returns true if this is a StaleClusterTimeError.
func IsStaleClusterTimeError(err error) bool {
	return GetErrorCode(err) == 209
}
