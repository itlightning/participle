package participle_test

import (
	"strings"
	"testing"

	require "github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
)

// A capture repeated over a run of tokens calls AddToString once per token on the same field.
// The appends must reuse spare capacity, not copy the whole string on every call.
func TestGeneratedAddToStringAmortisesRepeatedAppends(t *testing.T) {
	const n = 1 << 14
	allocs := testing.AllocsPerRun(5, func() {
		var c participle.GeneratedParserContext
		var s string
		for i := 0; i < n; i++ {
			c.AddToString(&s, "=")
		}
		if len(s) != n {
			t.Fatalf("len %d, want %d", len(s), n)
		}
	})
	// Copying on every append costs n-1 allocations; amortised growth costs about log2(n).
	require.True(t, allocs < 64, "allocations per run: %v", allocs)
}

func TestGeneratedAddToStringKeepsEarlierValues(t *testing.T) {
	var c participle.GeneratedParserContext

	a := ""
	c.AddToString(&a, "x")
	c.AddToString(&a, "y")
	c.AddToString(&a, "z")
	require.Equal(t, "xyz", a)

	// A branch copy that is extended and then discarded leaves the original intact,
	// and the original then extends to its own value.
	branch := a
	c.AddToString(&branch, "1")
	c.AddToString(&branch, "2")
	snapshot := branch
	c.AddToString(&a, "w")
	c.AddToString(&a, "v")
	require.Equal(t, "xyz12", branch)
	require.Equal(t, "xyz12", snapshot)
	require.Equal(t, "xyzwv", a)

	// Two fields of equal length appended in turn.
	p, q := "ab", "cd"
	for i := 0; i < 50; i++ {
		c.AddToString(&p, "1")
		c.AddToString(&q, "2")
	}
	require.Equal(t, "ab"+strings.Repeat("1", 50), p)
	require.Equal(t, "cd"+strings.Repeat("2", 50), q)

	// A string equal in content to the last one produced, but held in other memory.
	foreign := "ab" + strings.Repeat("1", 50)
	c.AddToString(&foreign, "!")
	c.AddToString(&p, "?")
	require.Equal(t, "ab"+strings.Repeat("1", 50)+"!", foreign)
	require.Equal(t, "ab"+strings.Repeat("1", 50)+"?", p)

	// Empty additions leave the destination unchanged.
	e := "keep"
	c.AddToString(&e, "")
	require.Equal(t, "keep", e)
	require.Equal(t, "xyzwv", a)
}
