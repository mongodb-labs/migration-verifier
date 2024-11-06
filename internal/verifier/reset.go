package verifier

import (
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func (verifier *Verifier) ResetInProgressTasks(ctx mongo.SessionContext) error {
	err := verifier.resetPrimaryTaskIfNeeded(ctx)

	if err != nil {
		err = verifier.resetCollectionTasksIfNeeded(ctx)
	}

	if err != nil {
		err = verifier.resetPartitionTasksIfNeeded(ctx)
	}

	return err
}

func (verifier *Verifier) resetPrimaryTaskIfNeeded(ctx mongo.SessionContext) error {
	taskColl := verifier.verificationTaskCollection()

	incompletePrimary, err := taskColl.CountDocuments(
		ctx,
		bson.M{
			"type":   verificationTaskPrimary,
			"status": verificationTaskProcessing,
		},
	)
	if err != nil {
		return errors.Wrapf(err, "failed to count incomplete %#q tasks", verificationTaskPrimary)
	}

	switch incompletePrimary {
	case 0:
		// Nothing to do.
	case 1:
		verifier.logger.Info().
			Msg("Previous verifier run left primary task incomplete. Resetting.")

		_, err := taskColl.DeleteMany(
			ctx,
			bson.M{
				"type": bson.M{
					"$ne": verificationTaskPrimary,
				},
			},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to delete non-%#q tasks", verificationTaskPrimary)
		}

		_, err = taskColl.UpdateOne(
			ctx,
			bson.M{
				"type": verificationTaskPrimary,
			},
			bson.M{
				"$set": bson.M{"status": verificationTaskAdded},
			},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to reset %#q task", verificationTaskPrimary)
		}

		return nil
	default:
		verifier.logger.Panic().
			Int64("incompletePrimary", incompletePrimary).
			Msg("Found multiple incomplete primary tasks; there should only be 1.")
	}

	return nil
}

func (verifier *Verifier) resetCollectionTasksIfNeeded(ctx mongo.SessionContext) error {
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

	for _, task := range incompleteCollTasks {
		_, err := taskColl.DeleteMany(
			ctx,
			bson.M{
				"type":                             verificationTaskVerifyDocuments,
				"query_filter.partition.namespace": task.QueryFilter.Namespace,
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
			bson.M{
				"$set": bson.M{"status": verificationTaskAdded},
				// TODO unset
			},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to reset namespace %#q's %#q task", task.QueryFilter.Namespace, verificationTaskVerifyCollection)
		}
	}

	return nil
}

func (verifier *Verifier) resetPartitionTasksIfNeeded(ctx mongo.SessionContext) error {
	taskColl := verifier.verificationTaskCollection()

	_, err := taskColl.UpdateMany(
		ctx,
		bson.M{
			"type":   verificationTaskVerifyDocuments,
			"status": verificationTaskProcessing,
		},
		bson.M{
			"$set": bson.M{"status": verificationTaskAdded},
			// TODO unset
		},
	)
	if err != nil {
		return errors.Wrapf(err, "failed to reset in-progress %#q tasks", verificationTaskVerifyDocuments)
	}

	return nil
}
