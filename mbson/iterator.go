package mbson

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
)

const maxBSONDocSize = 16 << 20

var headerLen = len(binary.LittleEndian.AppendUint32(nil, 0))

// Iterator reads documents from an io.Reader. The documents should be
// concatenated end-to-end.
type Iterator struct {
	reader io.Reader
}

// NewIterator creates an Iterator from an io.Reader.
func NewIterator(rdr io.Reader) *Iterator {
	return &Iterator{rdr}
}

// Next returns the next document from the stream, if it exists.
func (bi *Iterator) Next() (option.Option[bson.Raw], error) {
	docSizeBuf := make([]byte, headerLen)
	_, err := io.ReadFull(bi.reader, docSizeBuf)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return option.None[bson.Raw](), nil
		}

		return option.None[bson.Raw](), errors.Wrapf(err, "reading next BSON doc’s length")
	}

	docSize := binary.LittleEndian.Uint32(docSizeBuf)
	if docSize > maxBSONDocSize {
		return option.None[bson.Raw](), fmt.Errorf(
			"internal corruption: found excess BSON document size (%d; max=%d) in stream",
			docSize,
			maxBSONDocSize,
		)
	}

	docBuf := make(bson.Raw, docSize)
	copy(docBuf, docSizeBuf)

	_, err = io.ReadFull(bi.reader, docBuf[len(docSizeBuf):])
	if err != nil {
		return option.None[bson.Raw](), errors.Wrapf(err, "reading next BSON doc")
	}

	return option.Some(docBuf), nil
}
