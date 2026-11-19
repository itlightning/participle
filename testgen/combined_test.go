package testgen

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	require "github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// TestGenerateCombinedParser intentionally regenerates the parser file to give coverage to generator.
// Any changes to the committed parser code should be inspected for correctness (tests below help) and committed.
func TestGenerateCombinedParser(t *testing.T) {
	defer patchMaxIterations()()
	participle.MustGenerateParserFile[combinedGenerator](combinedParser, "combined_gen.go", 0664,
		participle.GenerateReadLimitChecks())
}

// TODO: struct positions & tokens, non-empty group, lookahead group, literal switch
// TODO: case-insensitive, literal with token type, union pointer types, Capture interface

func TestCombinedParser_Success(t *testing.T) {
	defer patchMaxIterations()()
	for in, wantFeatures := range map[string][]Feature{
		`nil`:                                 {Nil{}},
		`nil nil`:                             {Nil{}, Nil{}},
		`nil nil nil nil nil nil nil nil nil`: {Nil{}, Nil{}, Nil{}, Nil{}, Nil{}, Nil{}, Nil{}, Nil{}, Nil{}},

		`str "\"'\""`:      {Strings{Const: "str", Quoted: `"'"`}},
		`str "x" "y" join`: {Strings{Const: "str", Quoted: `xy`, Ptr: getPointer("join")}},
		`str "xyz" x.y.z`:  {Strings{Const: "str", Quoted: `xyz`, Ptr: getPointer("x.y.z")}},
		`str """" a.b nil`: {Strings{Const: "str", Quoted: ``, Ptr: getPointer("a.b")}, Nil{}},

		`int 1`:          {Ints{Normal: 1}},
		`int 00100`:      {Ints{Normal: 64}},
		`int 2 -3`:       {Ints{Normal: 2, Ptr: getPointer(int8(-3))}},
		`int 1 2 1 nil`:  {Ints{Normal: 1, Array: []int16{2, 1}}, Nil{}},
		`int 0 1 2 3 -4`: {Ints{Normal: 0, Array: []int16{1, 2, 3}, Ptr: getPointer(int8(-4))}},

		`uint 42`:                {Uints{Normal: 42}},
		`uint 0 1 1 2 3`:         {Uints{Normal: 0, Small: getPointer(uint32(11)), Big: []uint64{2, 3}}},
		`uint 0 1 1 2 3 4 5 6 7`: {Uints{Normal: 0, Small: getPointer(uint32(45)), Big: []uint64{2, 3, 6, 7}}},

		`float 1`:              {Floats{A: getPointer(1.0)}},
		`float 2 . 34`:         {Floats{A: getPointer(2.34)}},
		`float 0 1e30`:         {Floats{A: getPointer(0.), B: 1e30}},
		`float 4 . 7 4.2`:      {Floats{A: getPointer(4.7), B: 4.2}},
		`float 01 . 07 4.2e1 `: {Floats{A: getPointer(1.07), B: 42}},
		`float 42 42`:          {Floats{A: getPointer(42.0), B: 42}},

		`struct one two`: {Structs{
			Normal: Root{"one"},
			Array:  []Rec{{Root: Root{"two"}}},
		}},
		`struct x y {z}`: {Structs{
			Normal: Root{"x"},
			Array:  []Rec{{Root: Root{"y"}}, {In: &Rec{Root: Root{"z"}}}},
		}},
		`struct a {{{b}}} {{c}}`: {Structs{
			Normal: Root{"a"},
			Array:  []Rec{{In: &Rec{In: &Rec{In: &Rec{Root: Root{"b"}}}}}, {In: &Rec{In: &Rec{Root: Root{"c"}}}}},
		}},
		`struct normal array [ptr]`: {Structs{
			Normal:   Root{"normal"},
			Array:    []Rec{{Root: Root{"array"}}},
			PtrArray: []*Root{{Str: "ptr"}},
		}},
		`struct b c {d} [e,f,g]`: {Structs{
			Normal:   Root{"b"},
			Array:    []Rec{{Root: Root{"c"}}, {In: &Rec{Root: Root{"d"}}}},
			PtrArray: []*Root{{Str: "e"}, {Str: "f"}, {Str: "g"}},
		}},

		`negation`:              {Negation{}},
		`negation nil`:          {Negation{}, Nil{}},
		`negation 42`:           {Negation{Answer: 42}},
		`negation 1 2 3.14 nil`: {Negation{Trash: []string{"1", "2", "3.14"}}, Nil{}},
		`negation "x"`:          {Negation{Trash: []string{"x"}}},
		`negation 4 2 42`:       {Negation{Trash: []string{"4", "2"}, Answer: 42}},
		`negation.`:             {Negation{Trash: []string{"."}}},
		`negation..`:            {Negation{Trash: []string{".", "."}}},
		`negation...`:           {Negation{Trash: []string{".", ".", "."}}},
		`negation...?`:          {Negation{Trash: []string{".", ".", ".", "?"}}},

		`custom content`:          {Custom{Req: "content"}},
		`custom creative-content`: {Custom{Opt: "creative", Req: "content"}},
		`custom cut nil`:          {Custom{Req: "cut"}, Nil{}},
		`custom curious-cute-cat`: {Custom{Opt: "cute", Req: "cat"}},

		`parseable 4.7`:  {Parseable{Rounded: 5}},
		`parseable -4.2`: {Parseable{Rounded: -4}},
	} {
		t.Run(in, func(t *testing.T) {
			nativeOut, err := combinedParser.ParseString("", in, participle.SkipGeneratedParser())
			require.NoError(t, err, "Not valid input even for native parser")
			require.Equal(t, wantFeatures, nativeOut.Features, "Native parser sanity check failed")
			generatedOut, err := combinedParser.ParseString("", in)
			require.NoError(t, err, "Unexpected generated parser error")
			require.Equal(t, wantFeatures, generatedOut.Features, "Generated parser result different")
		})
	}
}

func TestCombinedParser_Error(t *testing.T) {
	defer patchMaxIterations()()
	for in, test := range map[string]struct {
		wantError string
	}{
		``:        {`sub-expression Feature+ must match at least once`},
		`unknown`: {`1:1: sub-expression Feature+ must match at least once`},

		`str`:          {`1:1: sub-expression Feature+ must match at least once`},
		`str "bad`:     {`1:9: literal not terminated`},
		`str "1" 1`:    {`1:9: unexpected token "1"`},
		`str "" x.`:    {`1:10: unexpected token "<EOF>" (expected <ident>)`},
		`str "" x.1`:   {`1:9: unexpected token ".1"`},
		`str "" x.y..`: {`1:12: unexpected token "." (expected <ident>)`},
		`str "" x. 1`:  {`1:11: unexpected token "1" (expected <ident>)`},

		`int 1.1`:     {`1:1: sub-expression Feature+ must match at least once`},
		`int -1`:      {`1:1: sub-expression Feature+ must match at least once`},
		`int 0 99999`: {`Ints.Array: strconv.ParseInt: parsing "99999": value out of range`},
		`int 0 -1000`: {`Ints.Ptr: strconv.ParseInt: parsing "-1000": value out of range`},
		`int 1 2 -x`:  {`1:10: unexpected token "x" (expected <int>)`},

		`uint`:                   {`1:1: sub-expression Feature+ must match at least once`},
		`uint -1`:                {`Uints.Normal: strconv.ParseUint: parsing "-1": invalid syntax`},
		`uint 0 1`:               {`1:9: unexpected token "<EOF>" (expected <int>)`},
		`uint 0 1 2`:             {`1:11: unexpected token "<EOF>" (expected <int> <int>)`},
		`uint 0 1 2 3`:           {`1:13: unexpected token "<EOF>" (expected <int>)`},
		`uint 0 1 2 3 x`:         {`1:14: unexpected token "x" (expected <int>)`},
		`uint 0 1 2 3 4 5`:       {`1:17: unexpected token "<EOF>" (expected <int>)`},
		`uint 0 55555 55555 3 4`: {`Uints.Small: strconv.ParseUint: parsing "5555555555": value out of range`},

		`float    `:     {`1:1: sub-expression Feature+ must match at least once`},
		`float 1.2`:     {`1:1: sub-expression Feature+ must match at least once`},
		`float 1 1e40`:  {`Floats.B: strconv.ParseFloat: parsing "1e40": value out of range`},
		`float 0.x`:     {`1:1: sub-expression Feature+ must match at least once`},
		`float 1 . 2 x`: {`1:13: unexpected token "x"`},

		`struct`:         {`1:1: sub-expression Feature+ must match at least once`},
		`struct 1`:       {`1:1: sub-expression Feature+ must match at least once`},
		`struct a`:       {`1:1: sub-expression Feature+ must match at least once`},
		`struct a b 1`:   {`1:12: unexpected token "1"`},
		`struct a b {}`:  {`1:13: unexpected token "}" (expected Rec "}")`},
		`struct a b {x]`: {`1:14: unexpected token "]" (expected "}")`},

		`struct a b {{x}]`: {`1:16: unexpected token "]" (expected "}")`},
		`struct a b {{x]}`: {`1:15: unexpected token "]" (expected "}")`},

		`struct a {b} []`:   {`1:15: unexpected token "]" (expected Root ("," Root)* "]")`},
		`struct a {b} [c,]`: {`1:17: unexpected token "]" (expected Root)`},
		`struct a {b} [c,1`: {`1:17: unexpected token "1" (expected Root)`},
		`struct a {b} [c,d`: {`1:18: unexpected token "<EOF>" (expected "]")`},

		`negation nope`:         {`1:10: unexpected token "nope"`},
		`negation 42 42`:        {`1:13: unexpected token "42"`},
		`negation....`:          {`1:9: unexpected token "."`},
		`negation.............`: {`1:10: unexpected token "."`}, // Behavior different due to hitting + match limit

		`custom`:              {`1:1: sub-expression Feature+ must match at least once`},
		`custom nil`:          {`1:1: sub-expression Feature+ must match at least once`},
		`custom -`:            {`1:1: sub-expression Feature+ must match at least once`},
		`custom c-nil`:        {`1:10: must start with c`},
		`custom cut-cat--cup`: {`1:16: unexpected token "-" (expected CustomContent)`},
		`custom cut-cat cup`:  {`1:16: unexpected token "cup"`},

		`parseable 0`:  {`1:1: sub-expression Feature+ must match at least once`},
		`parseable -y`: {`1:12: should have been a float`},

		`nil nil nil nil nil nil nil nil nil nil`: {`1:40: too many iterations of Feature+ (> 10)`},
		`str """"""""""""""""""""`:                {`1:25: too many iterations of <string>+ (> 10)`},
		`str "" x.x.x.x.x.x.x.x.x.x.x`:            {`1:29: too many iterations of ("." <ident>)* (> 10)`},
		`custom c-c-c-c-c-c-c-c-c-c-c`:            {`1:28: too many iterations of (CustomContent "-")* (> 10)`},
	} {
		t.Run(in, func(t *testing.T) {
			native, err := combinedParser.ParseString("", in, participle.SkipGeneratedParser())
			require.EqualError(t, err, test.wantError, "Native parser has no error or a different one")
			generated, err := combinedParser.ParseString("", in)
			require.EqualError(t, err, test.wantError, "Generated parser has no error or a different one")
			require.Equal(t, native, generated, "Partial parse trees different")
		})
	}
}

func TestCombinedSubParsing(t *testing.T) {
	subParser, err := participle.ParserForProduction[Rec](combinedParser)
	require.NoError(t, err)
	result, err := subParser.ParseString("", "{a}")
	require.NoError(t, err)
	require.Equal(t, &Rec{In: &Rec{Root: Root{"a"}}}, result)
	result, err = subParser.ParseString("", "{{b}")
	require.Equal(t, &Rec{In: &Rec{In: &Rec{Root: Root{"b"}}}}, result)
	require.EqualError(t, err, `1:5: unexpected token "<EOF>" (expected "}")`)
}

func TestNotGeneratedError(t *testing.T) {
	type notGenerated struct{ participle.GeneratedParserBase }
	recParser, err := participle.Build[Rec](participle.UseGeneratedParser[notGenerated]())
	require.NoError(t, err)
	result, err := recParser.ParseString("", "{x}", participle.SkipGeneratedParser()) // OK due to the option
	require.NoError(t, err)
	require.Equal(t, &Rec{In: &Rec{Root: Root{"x"}}}, result)
	_, err = recParser.ParseString("", "{x}")
	require.EqualError(t, err, "no generated parser for type testgen.Rec, please regenerate parser code")
}

func getPointer[T any](str T) *T {
	return &str
}

func pos(offset int) lexer.Position {
	return lexer.Position{Offset: offset, Line: 1, Column: offset + 1}
}

// patchMaxIterations decreases MaxIterations during the test, allowing to test that behavior on smaller inputs.
func patchMaxIterations() func() {
	var originalMaxIterations = participle.MaxIterations
	participle.MaxIterations = 10
	return func() {
		participle.MaxIterations = originalMaxIterations
	}
}

const combinedBenchInput = `
	str "quoted" "strings" un.quoted
	int 1 2 3 -3 2 1
	uint 123 1 2 3 4 5 6 7 8
	float 42 . 47 42.0047e1
	struct one two {three} {{four}} [a, b, c]
	negation 1 2 3 42
	custom 0123
	parseable 420.987
`

func BenchmarkCombinedParser_ParsingOnly(b *testing.B) {
	lexed, err := lexer.TextScannerLexer.Lex("", strings.NewReader(combinedBenchInput))
	require.NoError(b, err)
	peeking, err := lexer.Upgrade(lexed)
	require.NoError(b, err)
	checkpoint := peeking.MakeCheckpoint()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		peeking.LoadCheckpoint(checkpoint)
		_, _ = combinedParser.ParseFromLexer(peeking)
	}
}

// A long run of captures into one string field must cost linear memory in the generated parser.
// Quadratic concatenation allocates about 16 times the bytes when the run is 4 times longer.
func TestCombinedParser_LongStringCaptureRun(t *testing.T) {
	original := participle.MaxIterations
	participle.MaxIterations = 1 << 20
	defer func() { participle.MaxIterations = original }()

	parseRun := func(n int) (Strings, uint64) {
		in := "str" + strings.Repeat(` "ab"`, n)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		out, err := combinedParser.ParseString("", in)
		runtime.ReadMemStats(&after)
		require.NoError(t, err)
		require.Equal(t, 1, len(out.Features))
		native, err := combinedParser.ParseString("", in, participle.SkipGeneratedParser())
		require.NoError(t, err)
		require.Equal(t, native.Features, out.Features)
		return out.Features[0].(Strings), after.TotalAlloc - before.TotalAlloc
	}

	const n = 1 << 12
	short, shortBytes := parseRun(n)
	long, longBytes := parseRun(4 * n)
	require.Equal(t, strings.Repeat("ab", n), short.Quoted)
	require.Equal(t, strings.Repeat("ab", 4*n), long.Quoted)
	ratio := float64(longBytes) / float64(shortBytes)
	require.True(t, ratio < 8, "allocated bytes %d for %d tokens, %d for %d tokens (ratio %.1f)",
		shortBytes, n, longBytes, 4*n, ratio)
}

// A parse succeeds when its token reads stay within the lexer's read limit and fails with
// ErrReadLimit one read over it. The struct-entry checks stop an over-limit parse early instead
// of letting it run to the end.
func TestCombinedParser_ReadLimit(t *testing.T) {
	original := participle.MaxIterations
	participle.MaxIterations = 1 << 20
	defer func() { participle.MaxIterations = original }()

	parse := func(in string, limit int) (*Combined, int, error) {
		lexed, err := lexer.TextScannerLexer.Lex("", strings.NewReader(in))
		require.NoError(t, err)
		peeking, err := lexer.Upgrade(lexed)
		require.NoError(t, err)
		if limit > 0 {
			peeking.SetReadLimit(limit)
		}
		out, err := combinedParser.ParseFromLexer(peeking)
		return out, peeking.Reads(), err
	}

	in := strings.Repeat("struct a {{b}} [c, d] ", 1000)
	unlimited, reads, err := parse(in, 0)
	require.NoError(t, err)

	atLimit, atLimitReads, err := parse(in, reads)
	require.NoError(t, err)
	require.Equal(t, reads, atLimitReads)
	require.Equal(t, unlimited, atLimit)

	_, _, err = parse(in, reads-1)
	require.True(t, errors.Is(err, participle.ErrReadLimit), "error %v", err)

	_, stoppedReads, err := parse(in, reads/100)
	require.True(t, errors.Is(err, participle.ErrReadLimit), "error %v", err)
	require.True(t, stoppedReads < reads/50, "stopped after %d of %d reads", stoppedReads, reads)
}
