//go:build generated

package conformance_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alecthomas/assert/v2"
	"github.com/alecthomas/participle/v2/lexer"
	"github.com/alecthomas/participle/v2/lexer/internal/conformance"
)

// This should only be run by TestLexerConformanceGenerated.
func TestLexerConformanceGeneratedInternal(t *testing.T) {
	testLexer(t, conformance.GeneratedConformanceLexer)
}

// This should only be run by TestLexerConformanceGenerated.
func TestLexerWorkLimitGeneratedInternal(t *testing.T) {
	// Inside ExprString only ExprChar ([^$"\\]+) has a repetition loop, and it steps over each of
	// the 1000 letters once, so the work is exactly 1000.
	input := `EXPRTEST:"` + strings.Repeat("a", 1000) + `"`
	lexAll := func(limit int) (int, error) {
		lex, err := conformance.GeneratedConformanceLexer.(lexer.StringDefinition).LexString("", input)
		assert.NoError(t, err)
		limiter := lex.(lexer.WorkLimiter)
		if limit > 0 {
			limiter.SetWorkLimit(limit)
		}
		_, err = lexer.ConsumeAll(lex)
		return limiter.Work(), err
	}
	work, err := lexAll(0)
	assert.NoError(t, err)
	assert.Equal(t, 1000, work)
	_, err = lexAll(1000)
	assert.NoError(t, err)
	_, err = lexAll(999)
	assert.True(t, errors.Is(err, lexer.ErrWorkLimit), "error %v", err)
}
