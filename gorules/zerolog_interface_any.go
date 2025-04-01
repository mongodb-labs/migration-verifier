//go:build ruleguard
// +build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

func NoZerologInterface(m dsl.Matcher) {
	m.Import("github.com/rs/zerolog")

	m.Match("$v.Interface").
		Where(m["v"].Type.Is("*zerolog.Event")).
		Report("Avoid Interface(); use Any() instead.").
}
