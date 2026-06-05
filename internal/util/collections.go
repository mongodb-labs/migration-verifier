package util

import "go.mongodb.org/mongo-driver/v2/mongo"

// FullName returns the collection's full namespace.
func FullName(collection *mongo.Collection) string {
	return collection.Database().Name() + "." + collection.Name()
}
