package verifier

import (
	"context"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var defaultTaskUpdate = bson.M{
	"$set":   bson.M{"status": verificationTaskAdded},
	"$unset": bson.M{"begin_time": 1},
}

func (verifier *Verifier) ResetInProgressTasks(ctx context.Context) error {
	didReset, err := verifier.handleIncompletePrimary(ctx)

	if err == nil {
		if didReset {
			return nil
		}

		err = verifier.resetCollectionTasksIfNeeded(ctx)
	}

	if err == nil {
		err = verifier.resetPartitionTasksIfNeeded(ctx)
	}

	return err
}

func (verifier *Verifier) handleIncompletePrimary(ctx context.Context) (bool, error) {
	taskColl := verifier.verificationTaskCollection()

	cursor, err := taskColl.Find(
		ctx,
		bson.M{
			"type":   verificationTaskPrimary,
			"status": bson.M{"$ne": verificationTaskCompleted},
		},
	)
	if err != nil {
		return false, errors.Wrapf(err, "failed to fetch incomplete %#q task", verificationTaskPrimary)
	}

	var incompletePrimaries []VerificationTask
	err = cursor.All(ctx, &incompletePrimaries)
	if err != nil {
		return false, errors.Wrapf(err, "failed to read incomplete %#q task", verificationTaskPrimary)
	}

	switch len(incompletePrimaries) {
	case 0:
		// Nothing to do.
	case 1:
		// Invariant: task status should be “added”.
		if incompletePrimaries[0].Status != verificationTaskAdded {
			verifier.logger.Panic().
				Any("task", incompletePrimaries[0]).
				Msg("Primary task status has invalid state.")
		}

		verifier.logger.Info().
			Msg("Previous verifier run left primary task incomplete. Deleting non-primary tasks.")

		deleted, err := taskColl.DeleteMany(
			ctx,
			bson.M{
				"type": bson.M{
					"$ne": verificationTaskPrimary,
				},
			},
		)
		if err != nil {
			return false, errors.Wrapf(err, "failed to delete non-%#q tasks", verificationTaskPrimary)
		}

		verifier.logger.Info().
			Int64("deletedTasksCount", deleted.DeletedCount).
			Msg("Found and deleted non-primary tasks.")

		return true, nil
	default:
		verifier.logger.Panic().
			Any("tasks", incompletePrimaries).
			Msg("Found multiple incomplete primary tasks; there should only be 1.")
	}

	return false, nil
}

func (verifier *Verifier) resetCollectionTasksIfNeeded(ctx context.Context) error {
	taskColl := verifier.verificationTaskCollection()

	cursor, err := taskColl.Find(
		ctx,
		bson.M{
			"type":   verificationTaskVerifyCollection,
			"status": verificationTaskProcessing,
		},
	)
	if err != nil {
		return errors.Wrapf(err, "failed to find incomplete %#q tasks", verificationTaskVerifyCollection)
	}
	var incompleteCollTasks []VerificationTask
	err = cursor.All(ctx, &incompleteCollTasks)
	if err != nil {
		return errors.Wrapf(err, "failed to read incomplete %#q tasks", verificationTaskVerifyCollection)
	}

	if len(incompleteCollTasks) > 0 {
		verifier.logger.Info().
			Int("count", len(incompleteCollTasks)).
			Msg("Previous verifier run left collection-level verification task(s) pending. Resetting.")
	}

	for _, task := range incompleteCollTasks {
		_, err := taskColl.DeleteMany(
			ctx,
			bson.M{
				"type":                   verificationTaskVerifyDocuments,
				"query_filter.namespace": task.QueryFilter.Namespace,
			},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to delete namespace %#q's %#q tasks", task.QueryFilter.Namespace, verificationTaskVerifyDocuments)
		}

		_, err = taskColl.UpdateOne(
			ctx,
			bson.M{
				"type":                   verificationTaskVerifyCollection,
				"query_filter.namespace": task.QueryFilter.Namespace,
			},
			defaultTaskUpdate,
		)
		if err != nil {
			return errors.Wrapf(err, "failed to reset namespace %#q's %#q task", task.QueryFilter.Namespace, verificationTaskVerifyCollection)
		}
	}

	return nil
}

func (verifier *Verifier) resetPartitionTasksIfNeeded(ctx context.Context) error {
	taskColl := verifier.verificationTaskCollection()

	_, err := taskColl.UpdateMany(
		ctx,
		bson.M{
			"type":   verificationTaskVerifyDocuments,
			"status": verificationTaskProcessing,
		},
		defaultTaskUpdate,
	)
	if err != nil {
		return errors.Wrapf(err, "failed to reset in-progress %#q tasks", verificationTaskVerifyDocuments)
	}

	return nil
}
