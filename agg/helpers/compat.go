package helpers

import "github.com/10gen/migration-verifier/agg"

func SwitchToCond(in agg.Switch) agg.Cond {
	rootCond := agg.Cond{
		If:   in.Branches[0].Case,
		Then: in.Branches[0].Then,
	}

	curCond := &rootCond

	for _, branch := range in.Branches[1:] {
		newCond := agg.Cond{
			If:   branch.Case,
			Then: branch.Then,
		}

		curCond.Else = &newCond

		curCond = &newCond
	}

	return rootCond
}
