package oplog

import (
	"testing"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOpUnmarshal(t *testing.T) {
	op := Op{
		Op:     "hey",
		TS:     bson.Timestamp{345, 456},
		Ns:     "hohqohewoqhwe",
		DocLen: 777,
		DocID:  mbson.ToRawValue("haha"),
	}

	raw := bson.Raw(lo.Must(bson.Marshal(op)))

	rt := &Op{}
	require.NoError(t, rt.UnmarshalFromBSON(raw))

	assert.Equal(t, &op, rt, "Op should round-trip BSON (raw: %+v)", raw)
}

func TestResumeTokenBSON(t *testing.T) {
	token := ResumeToken{
		TS: bson.Timestamp{T: 234234, I: 11},
	}

	raw := token.MarshalToBSON()

	ts, err := GetRawResumeTokenTimestamp(raw)
	require.NoError(t, err)

	assert.Equal(t, token.TS, ts, "extracted timestamp should match")

	var rt ResumeToken
	require.NoError(t, bson.Unmarshal(raw, &rt))

	assert.Equal(t, token, rt)
}
