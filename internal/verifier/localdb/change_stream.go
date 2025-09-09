package localdb

import "go.mongodb.org/mongo-driver/bson"

// SetSrcChangeStreamResumeToken saves the source change stream’s
// resume token.
func (ldb *LocalDB) SetSrcChangeStreamResumeToken(token bson.Raw) error {
	return ldb.setMetadataValue("srcCSResumeToken", token)
}

// GetSrcChangeStreamResumeToken gets the source change stream’s
// resume token.
func (ldb *LocalDB) GetSrcChangeStreamResumeToken() (bson.Raw, error) {
	return ldb.getMetadataValue("srcCSResumeToken")
}

// SetDstChangeStreamResumeToken saves the destination change stream’s
// resume token.
func (ldb *LocalDB) SetDstChangeStreamResumeToken(token bson.Raw) error {
	return ldb.setMetadataValue("dstCSResumeToken", token)
}

// GetDstChangeStreamResumeToken gets the destination change stream’s
// resume token.
func (ldb *LocalDB) GetDstChangeStreamResumeToken() (bson.Raw, error) {
	return ldb.getMetadataValue("dstCSResumeToken")
}
