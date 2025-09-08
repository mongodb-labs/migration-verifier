package localdb

import "go.mongodb.org/mongo-driver/bson"

func (ldb *LocalDB) SetSrcChangeStreamResumeToken(token bson.Raw) error {
	return ldb.setMetadataValue("srcCSResumeToken", token)
}
func (ldb *LocalDB) GetSrcChangeStreamResumeToken() (bson.Raw, error) {
	return ldb.getMetadataValue("srcCSResumeToken")
}

func (ldb *LocalDB) SetDstChangeStreamResumeToken(token bson.Raw) error {
	return ldb.setMetadataValue("dstCSResumeToken", token)
}
func (ldb *LocalDB) GetDstChangeStreamResumeToken() (bson.Raw, error) {
	return ldb.getMetadataValue("dstCSResumeToken")
}
