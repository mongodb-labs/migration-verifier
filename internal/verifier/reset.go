package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var defaultTaskUpdate = bson.M{
	"$set":   bson.M{"status": tasks.Added},
	"$unset": bson.M{"begin_time": 1},
}

// ResetInProgressTasks surveys the metadata & rolls back any in-progress tasks.
// Specifically:
// - If the primary task is not complete: delete all non-primary tasks. Done!
// - Otherwise:
//   - For each in-progress collection-metadata task:
//   - delete all doc-verification tasks for that namespace
//   - reset the metadata task
//   - For each (remaining) in-progress document task:
//   - reset the task
//
// The above is resumable. Thus you can safely call this outside a transaction.
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
			"type":   tasks.Primary,
			"status": bson.M{"$ne": tasks.Completed},
		},
	)
	if err != nil {
		return false, errors.Wrapf(err, "failed to fetch incomplete %#q task", tasks.Primary)
	}

	var incompletePrimaries []tasks.Task
	err = cursor.All(ctx, &incompletePrimaries)
	if err != nil {
		return false, errors.Wrapf(err, "failed to read incomplete %#q task", tasks.Primary)
	}

	switch len(incompletePrimaries) {
	case 0:
		// Nothing to do.
	case 1:
		// Invariant: task status should be “added”.
		if incompletePrimaries[0].Status != tasks.Added {
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
					"$ne": tasks.Primary,
				},
			},
		)
		if err != nil {
			return false, errors.Wrapf(err, "failed to delete non-%#q tasks", tasks.Primary)
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
			"type":   tasks.VerifyCollection,
			"status": tasks.Processing,
		},
	)
	if err != nil {
		return errors.Wrapf(err, "failed to find incomplete %#q tasks", tasks.VerifyCollection)
	}
	var incompleteCollTasks []tasks.Task
	err = cursor.All(ctx, &incompleteCollTasks)
	if err != nil {
		return errors.Wrapf(err, "failed to read incomplete %#q tasks", tasks.VerifyCollection)
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
				"type":                   tasks.VerifyDocuments,
				"query_filter.namespace": task.QueryFilter.Namespace,
			},
		)
		if err != nil {
			return errors.Wrapf(err, "failed to delete namespace %#q's %#q tasks", task.QueryFilter.Namespace, tasks.VerifyDocuments)
		}

		_, err = taskColl.UpdateOne(
			ctx,
			bson.M{
				"type":                   tasks.VerifyCollection,
				"query_filter.namespace": task.QueryFilter.Namespace,
			},
			defaultTaskUpdate,
		)
		if err != nil {
			return errors.Wrapf(err, "failed to reset namespace %#q's %#q task", task.QueryFilter.Namespace, tasks.VerifyCollection)
		}
	}

	return nil
}

func (verifier *Verifier) resetPartitionTasksIfNeeded(ctx context.Context) error {
	taskColl := verifier.verificationTaskCollection()

	_, err := taskColl.UpdateMany(
		ctx,
		bson.M{
			"type":   tasks.VerifyDocuments,
			"status": tasks.Processing,
		},
		defaultTaskUpdate,
	)
	if err != nil {
		return errors.Wrapf(err, "failed to reset in-progress %#q tasks", tasks.VerifyDocuments)
	}

	return nil
}
