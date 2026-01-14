package compare

import "github.com/10gen/migration-verifier/mslices"

type Method string

const (
	Binary           Method = "binary"
	IgnoreOrder      Method = "ignoreOrder"
	ToHashedIndexKey Method = "toHashedIndexKey"
)

var (
	Methods = mslices.Of(
		Binary,
		IgnoreOrder,
		ToHashedIndexKey,
	)

	Default = Methods[0]
)

func (dcm Method) ShouldIgnoreFieldOrder() bool {
	return dcm == IgnoreOrder
}

func (dcm Method) ComparesFullDocuments() bool {
	return dcm == Binary || dcm == IgnoreOrder
}
