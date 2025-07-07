//go:build ruleguard
// +build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

func NoRawInterface(m dsl.Matcher) {
	m.Match("interface{}").
		Report("Avoid $$; prefer `any`.").
		Suggest("any")
}
