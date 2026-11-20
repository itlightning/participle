package participle

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

// TODO: don't create checkpoints for single-token nodes

type GeneratedParserFns map[reflect.Type]func(ctx GeneratedParserContext, out interface{}) error

type GeneratedParser interface {
	GeneratedParsers() GeneratedParserFns
}

// GeneratedParserBase should be embedded by a type created for a parser grammar where code generation is desired.
type GeneratedParserBase struct{}

func (c GeneratedParserBase) GeneratedParsers() GeneratedParserFns {
	// Placeholder method, will be shadowed by generated code
	return GeneratedParserFns{}
}

type GeneratedParserContext struct {
	Lex           *lexer.PeekingLexer
	parserOpts    *parserOptions
	allowTrailing bool
	concat        *strings.Builder // see AddToString

	err generatedError
}

func NewGeneratedParserContext[G any](
	parser *Parser[G], lex *lexer.PeekingLexer, allowTrailing bool,
) GeneratedParserContext {
	return GeneratedParserContext{
		Lex:           lex,
		parserOpts:    &parser.parserOptions,
		allowTrailing: allowTrailing,
	}
}

// ErrStepLimit is wrapped by the error a generated parser returns when its lexer went over the
// limit set with lexer.PeekingLexer.SetStepLimit.
var ErrStepLimit = errors.New("parse step limit exceeded")

// StepAndCheckLimit counts entering a grammar struct as one step (see lexer.PeekingLexer.Steps),
// reports whether the lexer is over its step limit, and sets a parse error when it is. Parsers
// generated with GenerateStepLimitChecks call it on entry to every struct of the grammar, inlined
// or not, so once over the limit every struct fails at once and the parse unwinds through the
// ordinary error path. Between two struct entries generated code only matches tokens, each test
// either consuming its token or ending its loop, so past the limit the parse stops within one run
// of tokens.
func (c *GeneratedParserContext) StepAndCheckLimit() bool {
	if !c.Lex.Step() {
		return false
	}
	c.setError(genParseError, ErrStepLimit.Error(), nil)
	return true
}

func (c *GeneratedParserContext) AboveLookahead(checkpoint lexer.Checkpoint) bool {
	return c.Lex.Cursor()-checkpoint.Cursor() > c.parserOpts.useLookahead
}

func (c *GeneratedParserContext) InvokeCustom(index int) (interface{}, bool) {
	def := &c.parserOpts.customDefs[index]
	results := def.parseFn.Call([]reflect.Value{reflect.ValueOf(c.Lex)})
	if err, _ := results[1].Interface().(error); err != nil {
		if err == NextMatch {
			c.SetTokenError(def.typ.Name())
		} else {
			c.SetCustomError("", err)
		}
		return nil, false
	}
	return results[0].Interface(), true
}

// AddToString appends add to *dest. Generated code calls it once per captured token, so a capture
// repeated over a run of tokens appends to the same string many times; plain concatenation copies
// the whole string on each call, which is quadratic in the length of the run.
//
// The context keeps one strings.Builder holding the last string it produced. When *dest still
// equals that string, add is appended to the builder in place (amortised growth) and *dest takes
// the longer string; otherwise the builder restarts from *dest. The builder only ever writes past
// the end of strings it has handed out, so every string held elsewhere (a branch copy, a field
// assigned earlier) keeps its value. The equality check costs O(1) in the common case because the
// runtime compares equal pointers without reading the bytes.
func (c *GeneratedParserContext) AddToString(dest *string, add string) {
	// Using assignment instead of concatenation is faster for empty strings (usual case), even with the extra check
	if *dest == "" {
		*dest = add
		return
	}
	if add == "" {
		return
	}
	if c.concat == nil {
		c.concat = &strings.Builder{}
	}
	if c.concat.String() != *dest {
		c.concat.Reset()
		c.concat.Grow(len(*dest) + len(add))
		c.concat.WriteString(*dest)
	}
	c.concat.WriteString(add)
	*dest = c.concat.String()
}

// generatedError encapsulates error handling state within a generated parser.
//
// It is merely a namespace and doesn't make sense without GeneratedParserContext. It supports holding 3 error types:
//   genTokenError:  UnexpectedTokenError - info optionally holds expected
//   genParseError:  ParseError           - info holds the error message
//   genCustomError: the error interface  - info optionally holds additional error context
// They are only actually created once parsing actually stops (most are suppressed), significantly reducing allocations.
type generatedError struct {
	active bool
	typ    genErrorType
	cursor int
	token  *lexer.Token
	info   string
	custom error
}

type genErrorType int8

const (
	genNoError = iota
	genTokenError
	genParseError
	genCustomError
)

func (c *GeneratedParserContext) HasErr() bool {
	return c.err.active
}

func (c *GeneratedParserContext) SuppressError() {
	c.err.active = false
}

func (c *GeneratedParserContext) ResetError() {
	c.err = generatedError{}
}

func (c *GeneratedParserContext) SetTokenError(expect string) {
	if c.err.typ != genNoError && c.err.cursor > c.Lex.Cursor() {
		c.err.active = true
		return
	}
	c.setError(genTokenError, expect, nil)
}

func (c *GeneratedParserContext) SetParseError(msg string) {
	c.setError(genParseError, msg, nil)
}

func (c *GeneratedParserContext) SetCustomError(details string, err error) {
	c.setError(genCustomError, details, err)
}

func (c *GeneratedParserContext) AddTokenErrorExpected(expect string) {
	if c.err.active && c.err.typ == genTokenError && c.err.cursor == c.Lex.Cursor() && c.err.info == "" {
		c.err.info = expect
	}
}

func (c *GeneratedParserContext) setError(typ genErrorType, info string, err error) {
	c.err.active = true
	c.err.typ = typ
	c.err.cursor = c.Lex.Cursor()
	c.err.token = c.Lex.Peek()
	c.err.info = info
	c.err.custom = err
}

func (c *GeneratedParserContext) FinalError() error {
	if !c.HasErr() && !(c.Lex.Peek().EOF() || c.allowTrailing) {
		c.SetTokenError("")
	}
	// Over the limit the parse fails whatever its outcome, so that whether a parse succeeds
	// depends only on its input and the limit, not on where the last check happened to fall.
	if c.Lex.OverStepLimit() {
		return Wrapf(c.Lex.Peek().Pos, ErrStepLimit, "%d parse steps", c.Lex.Steps())
	}

	if !c.err.active {
		return nil
	}
	switch c.err.typ {
	case genTokenError:
		return &UnexpectedTokenError{Unexpected: *c.err.token, Expect: c.err.info}
	case genParseError:
		return &ParseError{Msg: c.err.info, Pos: c.err.token.Pos}
	case genCustomError:
		if c.err.info == "" {
			return c.err.custom
		}
		return &ParseError{Msg: fmt.Sprintf("%s: %s", c.err.info, c.err.custom.Error())}
	default:
		panic(fmt.Errorf("unknown generated error type: %v", c.err.typ))
	}
}

// codeGenerator is an internal utility for generating parser code; holding configuration and generated code.
//
// It also encapsulates some logic common to multiple node implementations.
type codeGenerator struct {
	symbols          map[string]lexer.TokenType
	caseInsensitive  map[lexer.TokenType]bool
	packagePrefix    string
	lookahead        int
	printAllocations bool
	stepLimitChecks  bool

	out            bytes.Buffer
	lines          int
	indent         int
	structs        map[string]generatedStruct
	missingStructs []string
}

type generatedStruct struct {
	node   *strct
	usages int
}

type generatedParserTypeCase struct {
	typeCase string
	method   string
}

// GenerateOption modifies the code GenerateParserCode produces.
type GenerateOption func(g *codeGenerator)

// GenerateStepLimitChecks makes every generated struct count a step and check the lexer's step
// limit on entry (see GeneratedParserContext.StepAndCheckLimit). Without it a parse counts only
// consumed tokens, and a parse over the limit still fails, but only once it has finished.
func GenerateStepLimitChecks() GenerateOption {
	return func(g *codeGenerator) {
		g.stepLimitChecks = true
	}
}

func MustGenerateParserFile[PG GeneratedParser, G any](parser *Parser[G], filename string, perm os.FileMode, opts ...GenerateOption) {
	code := GenerateParserCode[PG](parser, opts...)
	if err := os.WriteFile(filename, code, perm); err != nil {
		panic(err)
	}
}

func GenerateParserCode[PG GeneratedParser, G any](parser *Parser[G], opts ...GenerateOption) []byte {
	generatorType := reflect.TypeOf(*new(PG))
	rootNode := parser.typeNodes[parser.rootType].(*strct)
	rootType := rootNode.typ.Name()
	grammarPkgExternal := generatorType.PkgPath() != rootNode.typ.PkgPath()
	dstPackageParts := strings.Split(generatorType.PkgPath(), "/")
	dstPackage := dstPackageParts[len(dstPackageParts)-1]
	parserName := generatorType.Name()
	parserCtxName := generatorType.Name() + "Context"

	gen := codeGenerator{
		symbols:          parser.lex.Symbols(),
		caseInsensitive:  parser.caseInsensitiveTokens,
		packagePrefix:    "",
		printAllocations: false,

		structs: map[string]generatedStruct{},
	}
	for _, opt := range opts {
		opt(&gen)
	}
	gen.queueGeneratingStruct(rootNode)

	gen.statement(`// Code generated by participle parser generator. DO NOT EDIT.`)
	gen.statement(`// source: ` + rootNode.typ.PkgPath() + `.` + rootType)
	gen.statement(`// If code breaks after changes to the grammar, simply delete this file and regenerate.`)
	gen.statement(``)
	gen.statement(`package ` + dstPackage)
	gen.statement(``)
	gen.statement(`import (`)
	gen.statementIndent(`"reflect"`)
	gen.statementIndent(`"strconv"`)
	gen.statement(``)
	gen.statementIndent(`"github.com/alecthomas/participle/v2"`)
	if grammarPkgExternal {
		gen.statement(``)
		gen.statementIndent(fmt.Sprintf(`source %q`, rootNode.typ.PkgPath()))
		gen.packagePrefix = "source."
	}
	gen.statement(`)`)
	gen.statement(``)
	gen.statement(`var _ = strconv.ParseInt // Avoid unused package error`)
	gen.statement(``)
	gen.statement(`type ` + parserCtxName + ` struct {`)
	gen.statementIndent(`participle.GeneratedParserContext`)
	gen.statement(`}`)
	gen.statement(``)

	var typeCases []generatedParserTypeCase
	var maxTypeCaseLength = 0

	for len(gen.missingStructs) > 0 {
		item := gen.structs[gen.missingStructs[0]]
		gen.missingStructs = gen.missingStructs[1:]
		normalizedName := item.node.normalizedName()
		qualifiedName := gen.packagePrefix + item.node.typ.Name()

		gen.statement(fmt.Sprintf(`func (_ %s) Parse%s(ctx participle.GeneratedParserContext, out interface{}) error {`,
			parserName, normalizedName))
		gen.statementIndent(fmt.Sprintf(`c := %s{GeneratedParserContext: ctx}`, parserCtxName))
		gen.statementIndent(fmt.Sprintf(`c.parse%s(out.(*%s))`, normalizedName, qualifiedName))
		gen.statementIndent(`return c.FinalError()`)
		gen.statement(`}`)
		gen.statement(``)

		state := generatorState{
			target: generatedVariable{
				name:         `out`,
				rValuePrefix: `*`,
				typ:          item.node.typ,
			},
		}

		gen.statement(fmt.Sprintf(`func (c *%s) parse%s(out *%s) {`, parserCtxName, normalizedName, qualifiedName))
		state.errorLabel = gen.newLabel(`strct`+normalizedName, `Error`)
		gen.indent++
		if gen.stepLimitChecks {
			gen.statement(`if c.StepAndCheckLimit() {`)
			gen.gotoLabelIndent(state.errorLabel, 1)
			gen.statement(`}`)
		}
		gen.statement(`c.SuppressError()`)
		item.node.generateBody(state, &gen)
		gen.writeLabel(state.errorLabel)
		gen.indent--
		gen.statement(`}`)
		gen.statement(``)

		typeCase := fmt.Sprintf(`reflect.TypeOf((*%s)(nil)): `, qualifiedName)
		typeCases = append(typeCases, generatedParserTypeCase{typeCase: typeCase, method: `g.Parse` + normalizedName})
		if len(typeCase) > maxTypeCaseLength {
			maxTypeCaseLength = len(typeCase)
		}
	}

	gen.statement(`// GeneratedParsers exposes non-inlined methods for sub-parsing.`)
	gen.statement(fmt.Sprintf(`func (g %s) GeneratedParsers() participle.GeneratedParserFns {`, parserName))
	gen.indent++
	gen.statement(`// The entrypoint to the generated code. Basically a static method. Shadows embedded method.`)
	gen.statement(`return participle.GeneratedParserFns{`)
	for _, typ := range typeCases {
		padding := strings.Repeat(" ", maxTypeCaseLength-len(typ.typeCase)) // Makes the code well formatted
		gen.statementIndent(typ.typeCase + padding + typ.method + `,`)
	}
	gen.statement(`}`)
	gen.indent--
	gen.statement(`}`)

	return gen.out.Bytes()
}

func (g *codeGenerator) queueGeneratingStruct(node *strct) {
	name := node.typ.String()
	if item, ok := g.structs[name]; !ok {
		g.missingStructs = append(g.missingStructs, name)
		g.structs[name] = generatedStruct{node: node, usages: 1}
	} else {
		item.usages++
		g.structs[name] = item
	}
}

func (g *codeGenerator) shouldUseDirectCaptureForType(targetType reflect.Type) bool {
	switch kind := targetType.Kind(); kind {
	case reflect.String, reflect.Bool, reflect.Slice:
		return true
	default:
		return false
	}
}

func (g *codeGenerator) captureTokens(state generatorState) {
	g.captureTokenPrimitive(state, state.capture.field.Type, state.targetRef(), 1)
}

func (g *codeGenerator) captureTokenPrimitive(state generatorState, targetType reflect.Type, targetRef string, depth int) {
	srcRef := state.capture.sourceRef()
	switch kind := targetType.Kind(); kind {
	case reflect.String:
		//g.statement(targetRef + ` += ` + srcRef)
		g.statement(`c.AddToString(&` + targetRef + `, ` + srcRef + `)`)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		convert := fmt.Sprintf(`strconv.ParseInt(%s, 0, %d)`, srcRef, sizeOfKind(kind))
		g.captureTokenNumber(state, kind, reflect.Int64, targetRef, convert)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		convert := fmt.Sprintf(`strconv.ParseUint(%s, 0, %d)`, srcRef, sizeOfKind(kind))
		g.captureTokenNumber(state, kind, reflect.Uint64, targetRef, convert)
	case reflect.Float32, reflect.Float64:
		convert := fmt.Sprintf(`strconv.ParseFloat(%s, %d)`, srcRef, sizeOfKind(kind))
		g.captureTokenNumber(state, kind, reflect.Float64, targetRef, convert)
	case reflect.Bool:
		if !state.capture.direct {
			g.statement(`var _ = ` + srcRef) // Prevent an error due to the capture buffer being unused
		}
		g.statement(targetRef + ` = true`)
	case reflect.Ptr:
		g.statement(`{`)
		g.indent++
		captureVar := fmt.Sprintf(`ptrValue%d`, depth)
		g.statement(fmt.Sprintf(`var %s %s`, captureVar, targetType.Elem().String()))
		g.statement(`if ` + targetRef + ` != nil {`)
		g.statementIndent(fmt.Sprintf(`%s = *%s`, captureVar, targetRef))
		g.statement(`}`)
		g.captureTokenPrimitive(state, targetType.Elem(), captureVar, depth+1)
		g.reportAllocation("capturing pointer "+targetRef, ``, 0)
		g.statement(targetRef + ` = &` + captureVar)
		g.indent--
		g.statement(`}`)
	case reflect.Slice:
		g.statement(`{`)
		g.indent++
		captureVar := fmt.Sprintf(`sliceItem%d`, depth)
		g.statement(fmt.Sprintf(`var %s %s`, captureVar, targetType.Elem().String()))
		g.captureTokenPrimitive(state, targetType.Elem(), captureVar, depth+1)
		g.statement(fmt.Sprintf(`%s = append(%s, %s)`, targetRef, targetRef, captureVar))
		g.indent--
		g.statement(`}`)
	default:
		panic(fmt.Errorf("generator encountered unsupported struct field type: %v", targetType))
	}
}

func (g *codeGenerator) captureTokenNumber(state generatorState, kind, produces reflect.Kind, targetRef, convert string) {
	g.statement(`if numValue, err := ` + convert + `; err != nil {`)
	if state.capture.direct {
		g.statementIndent(`c.Lex.Next()`) // Just to make error lookahead check consistent with native parser
	}
	g.statementIndent(fmt.Sprintf(`c.SetCustomError(%q, err)`, state.targetTypePath()))
	g.gotoLabelIndent(state.structErrorLabel, 1) // Capture errors abort parsing the struct
	g.statement(`} else {`)
	if kind == produces {
		g.statementIndent(targetRef + ` = numValue`)
	} else {
		g.statementIndent(fmt.Sprintf(`%s = %s(numValue)`, targetRef, kind.String()))
	}
	g.statement(`}`)
}

func (g *codeGenerator) captureStruct(state generatorState, value generatedVariable, usages int) {
	targetRef := state.targetRef()
	switch state.capture.field.Type.Kind() {
	case reflect.Struct, reflect.Interface:
		g.statement(targetRef + ` = ` + value.name)
	case reflect.Ptr:
		// Without this trick, the variable would always be allocated on heap, causing allocations even when not needed
		// Most times, the overhead of copying the value from stack to heap will be lower than the needless allocations
		// TODO !!@@@@ what is the usages check for? just... why?
		if usages > 1 {
			g.reportAllocation("capturing struct pointer "+targetRef+" = "+value.name, ``, 0)
			g.statement(targetRef + ` = &` + value.name)
		} else {
			g.statement(`{`)
			g.reportAllocation("capturing struct pointer "+targetRef+" = "+value.name, ``, 1)
			g.statementIndent(value.name + `Heap := ` + value.name)
			g.statementIndent(targetRef + ` = &` + value.name + `Heap`)
			g.statement(`}`)
		}
	case reflect.Slice:
		// TODO: check slice of pointers? slice of slices, etc? should be recursive?
		itemRef := value.name
		switch state.capture.field.Type.Elem().Kind() {
		case reflect.Interface:
			itemRef = fmt.Sprintf("%s%s(%s)", g.packagePrefix, state.capture.field.Type.Elem().Name(), itemRef)
		case reflect.Ptr:
			itemRef = "&" + itemRef
		}
		g.statement(fmt.Sprintf(`%s = append(%s, %s)`, targetRef, targetRef, itemRef))
	default:
		panic(state.capture.field.Type.String())
	}
}

func (g *codeGenerator) reportAllocation(cause string, condition string, indent int) {
	if !g.printAllocations {
		return
	}
	if condition == "" {
		g.statementCustomIndent(fmt.Sprintf(`println("allocation: %d: %s")`, g.lines, cause), indent)
		return
	}
	g.statementCustomIndent(`if `+condition+` {`, indent)
	g.statementCustomIndent(fmt.Sprintf(`println("allocation: %d: %s")`, g.lines, cause), indent+1)
	g.statementCustomIndent(`}`, indent)
}

func (g *codeGenerator) getFieldRef(variable generatedVariable, fieldIndex []int) string {
	fieldName := variable.typ.FieldByIndex(fieldIndex).Name
	return variable.name + `.` + fieldName
}

func (g *codeGenerator) handleMismatchIndent(state generatorState, indent int) {
	g.statementCustomIndent(fmt.Sprintf(`c.SetTokenError(%q)`, state.failUnexpectedWith), indent)
	g.gotoLabelIndent(state.errorLabel, indent)
}

func (g *codeGenerator) processToken(state generatorState) {
	if state.capture != nil {
		if state.capture.direct {
			g.captureTokens(state)
		} else {
			operation := ` = c.Lex.Peek().Value`
			if state.capture.nonEmpty {
				operation = ` += c.Lex.Peek().Value`
			}
			state.capture.nonEmpty = true
			g.statement(state.capture.sourceRef() + operation)
		}
	}
	g.statement(`c.Lex.Next()`)
}

func (g *codeGenerator) statement(code string) {
	g.statementCustomIndent(code, 0)
}

func (g *codeGenerator) statementIndent(code string) {
	g.statementCustomIndent(code, 1)
}

func (g *codeGenerator) statementCustomIndent(code string, indent int) {
	if g.out.Len() > 10_000_000 {
		panic(fmt.Errorf("generator sanity check: %d", g.out.Len()))
	}
	// Don't write indentation if it's an empty line for improving readability
	if code != `` {
		g.out.WriteString(strings.Repeat("\t", g.indent+indent))
		g.out.WriteString(code)
	}
	g.out.WriteRune('\n')
	g.lines++
}

func (g *codeGenerator) newLabel(location string, action string) *jumpLabel {
	return &jumpLabel{name: fmt.Sprintf("%s%d%s", location, g.lines, action)}
}

func (g *codeGenerator) gotoLabelIndent(label *jumpLabel, indent int) {
	if label == nil {
		return // Means no goto is necessary
	}
	label.used = true
	g.statementCustomIndent(`goto `+label.name, indent)
}

func (g *codeGenerator) writeLabel(label *jumpLabel) bool {
	if !label.used {
		return false
	}
	g.statementCustomIndent(label.name+`:`, -1)
	return true
}

// generatorState is an internal helper wrapping node-local state of parser code generation.
//
// It is important this is passed by value as overrides to the fields should usually only affect child nodes.
type generatorState struct {
	target             generatedVariable
	captureSink        *captureSink
	capture            *generatedCapture
	errorLabel         *jumpLabel
	structErrorLabel   *jumpLabel
	failUnexpectedWith string
}

func (s *generatorState) targetRef() string {
	return s.target.name + `.` + s.capture.field.Name
}

func (s *generatorState) targetTypePath() string {
	return s.target.typ.Name() + `.` + s.capture.field.Name
}

func (s *generatorState) ensureCaptureSink(gen *codeGenerator, childNode node) func() {
	if s.captureSink != nil {
		return func() {}
	}
	switch childNode.(type) {
	case *literal, *reference, *strct:
		return func() {} // Optimization: If no capture can be inside, the sink isn't necessary
	}
	s.captureSink = &captureSink{variable: "branch_" + s.target.name}
	gen.statement(s.captureSink.variable + ` := ` + s.target.name)
	return func() {
		if len(s.captureSink.fields) > 0 {
			for _, field := range s.captureSink.fields {
				gen.statement(s.target.name + `.` + field + ` = ` + s.captureSink.variable + `.` + field)
			}
		} else {
			gen.statement(`_ = ` + s.captureSink.variable) // Otherwise would be an unused variable
		}
	}
}

type generatedVariable struct {
	name         string
	rValuePrefix string
	typ          reflect.Type
}

type captureSink struct {
	variable string
	fields   []string
}

func (s *captureSink) captureField(name string) {
	for _, other := range s.fields {
		if name == other {
			return
		}
	}
	s.fields = append(s.fields, name)
}

type jumpLabel struct {
	name string
	used bool
}

type generatedCapture struct {
	field    reflect.StructField
	direct   bool
	nonEmpty bool
}

func (c *generatedCapture) sourceRef() string {
	if c.direct {
		return `c.Lex.Peek().Value`
	}
	return `buf` + c.field.Name
}
