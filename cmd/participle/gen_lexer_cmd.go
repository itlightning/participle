package main

import (
	_ "embed" // For go:embed.
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"regexp/syntax"
	"sort"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/participle/v2/lexer"
)

type genLexerCmd struct {
	Name      string `help:"Name of the lexer."`
	Output    string `short:"o" help:"Output file."`
	Tags      string `help:"Build tags to include in the generated file."`
	CountWork bool   `help:"Count the matchers' work and implement lexer.WorkLimiter."`
	Package   string `arg:"" required:"" help:"Go package for generated code."`
	Lexer     string `arg:"" default:"-" type:"existingfile" help:"JSON representation of a Participle lexer (read from stdin if omitted)."`
}

func (c *genLexerCmd) Help() string {
	return `
Generates Go code implementing the given JSON representation of a lexer. The
generated code should in general by around 10x faster and produce zero garbage
per token.
`
}

func (c *genLexerCmd) Run() error {
	var r *os.File
	if c.Lexer == "-" {
		r = os.Stdin
	} else {
		var err error
		r, err = os.Open(c.Lexer)
		if err != nil {
			return err
		}
		defer r.Close()
	}

	rules := lexer.Rules{}
	err := json.NewDecoder(r).Decode(&rules)
	if err != nil {
		return err
	}
	def, err := lexer.New(rules)
	if err != nil {
		return err
	}
	out := os.Stdout
	if c.Output != "" {
		out, err = os.Create(c.Output)
		if err != nil {
			return err
		}
		defer out.Close()
	}
	err = generateLexer(out, c.Package, def, c.Name, c.Tags, c.CountWork)
	if err != nil {
		return err
	}
	return nil
}

var (
	//go:embed codegen.go.tmpl
	codegenTemplateSource string
	codegenBackrefRe      = regexp.MustCompile(`(\\+)(\d)`)
	codegenTemplate       = template.Must(template.New("lexgen").Funcs(template.FuncMap{
		"IsPush": func(r lexer.Rule) string {
			if p, ok := r.Action.(lexer.ActionPush); ok {
				return p.State
			}
			return ""
		},
		"IsPop": func(r lexer.Rule) bool {
			_, ok := r.Action.(lexer.ActionPop)
			return ok
		},
		"IsReturn": func(r lexer.Rule) bool {
			return r == lexer.ReturnRule
		},
		"OrderRules": orderRules,
		"HaveBackrefs": func(def *lexer.StatefulDefinition, state string) bool {
			for _, rule := range def.Rules()[state] {
				if codegenBackrefRe.MatchString(rule.Pattern) {
					return true
				}
			}
			return false
		},
	}).Parse(codegenTemplateSource))
)

func generateLexer(w io.Writer, pkg string, def *lexer.StatefulDefinition, name, tags string, countWork bool) error {
	type ctx struct {
		Package   string
		Name      string
		Tags      string
		Def       *lexer.StatefulDefinition
		CountWork bool
	}
	rules := def.Rules()
	err := codegenTemplate.Execute(w, ctx{pkg, name, tags, def, countWork})
	if err != nil {
		return err
	}
	seen := map[string]bool{} // Rules can be duplicated by Include().
	for _, rules := range orderRules(rules) {
		for _, rule := range rules.Rules {
			if rule.Name == "" {
				panic(rule)
			}
			if seen[rule.Name] {
				continue
			}
			seen[rule.Name] = true
			fmt.Fprintf(w, "\n")
			err := generateRegexMatch(w, name, rule.Name, rule.Pattern, countWork)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type orderedRule struct {
	Name  string
	Rules []lexer.Rule
}

func orderRules(rules lexer.Rules) []orderedRule {
	orderedRules := []orderedRule{}
	for name, rules := range rules {
		orderedRules = append(orderedRules, orderedRule{
			Name:  name,
			Rules: rules,
		})
	}
	sort.Slice(orderedRules, func(i, j int) bool {
		return orderedRules[i].Name < orderedRules[j].Name
	})
	return orderedRules
}

// uncountedLoopSpan is the longest loop span a counting matcher does not add to its work. Short
// spans are almost every loop on ordinary text (words, whitespace runs), and skipping the memory
// add for them is most of what keeps counting cheap. Leaving them out keeps the count a bound.
// The outermost loops of a pattern each run once per rule attempt, and a loop nested inside
// another steps only over bytes its outer loop steps over, once per level of nesting. So an
// outermost loop whose span is counted pays for the loops inside it up to the pattern's nesting
// depth, and one whose span is not counted covers at most uncountedLoopSpan bytes at every
// level: the uncounted work per attempt is bounded by a constant of the pattern, as the work
// outside loops already is, and a rescan of a long run is still counted.
const uncountedLoopSpan = 16

// generateRegexMatch writes the matcher for one rule. With countWork the matcher takes a work
// counter and adds to it the bytes its repetition loops step over (see lexer.WorkLimiter); a
// backreference rule runs the regexp package and counts nothing.
func generateRegexMatch(w io.Writer, lexerName, name, pattern string, countWork bool) error {
	workParam := ""
	if countWork {
		workParam = ", work *int"
	}
	if codegenBackrefRe.FindStringIndex(pattern) != nil {
		fmt.Fprintf(w, "func match%s%s(s string, p int, backrefs []string%s) (groups []int) {\n", lexerName, name, workParam)
		fmt.Fprintf(w, "  re, err := lexer.BackrefRegex(%sBackRefCache, %q, backrefs)\n", lexerName, pattern)
		fmt.Fprintf(w, "  if err != nil { panic(fmt.Sprintf(\"%%s: %%s\", err, backrefs)) }\n")
		fmt.Fprintf(w, "  return re.FindStringSubmatchIndex(s[p:])\n")
		fmt.Fprintf(w, "}\n")
		return nil
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return err
	}
	ids := map[string]int{}
	idn := 0
	reid := func(re *syntax.Regexp) int {
		key := re.Op.String() + ":" + re.String()
		id, ok := ids[key]
		if ok {
			return id
		}
		id = idn
		idn++
		ids[key] = id
		return id
	}
	exists := func(re *syntax.Regexp) bool {
		key := re.Op.String() + ":" + re.String()
		_, ok := ids[key]
		return ok
	}
	re = re.Simplify()
	fmt.Fprintf(w, "// %s\n", re)
	fmt.Fprintf(w, "func match%s%s(s string, p int, backrefs []string%s) (groups [%d]int) {\n", lexerName, name, workParam, 2*re.MaxCap()+2)
	flattened := flatten(re)

	// Fast-path a single literal.
	if len(flattened) == 1 && re.Op == syntax.OpLiteral {
		n := utf8.RuneCountInString(string(re.Rune))
		if re.Flags&syntax.FoldCase != 0 {
			fmt.Fprintf(w, "if p+%d <= len(s) && strings.EqualFold(s[p:p+%d], %q) {\n", n, n, string(re.Rune))
		} else {
			if n == 1 {
				fmt.Fprintf(w, "if p < len(s) && s[p] == %q {\n", re.Rune[0])
			} else {
				fmt.Fprintf(w, "if p+%d <= len(s) && s[p:p+%d] == %q {\n", n, n, string(re.Rune))
			}
		}
		fmt.Fprintf(w, "groups[0] = p\n")
		fmt.Fprintf(w, "groups[1] = p + %d\n", n)
		fmt.Fprintf(w, "}\n")
		fmt.Fprintf(w, "return\n")
		fmt.Fprintf(w, "}\n")
		return nil
	}
	for _, re := range flattened {
		if exists(re) {
			continue
		}
		fmt.Fprintf(w, "// %s (%s)\n", re, re.Op)
		fmt.Fprintf(w, "l%d := func(s string, p int) int {\n", reid(re))
		if re.Flags&syntax.NonGreedy != 0 {
			panic("non-greedy match not supported: " + re.String())
		}
		switch re.Op {
		case syntax.OpNoMatch: // matches no strings
			fmt.Fprintf(w, "return p\n")

		case syntax.OpEmptyMatch: // matches empty string
			fmt.Fprintf(w, "if len(s) == 0 { return p }\n")
			fmt.Fprintf(w, "return -1\n")

		case syntax.OpLiteral: // matches Runes sequence
			n := utf8.RuneCountInString(string(re.Rune))
			if re.Flags&syntax.FoldCase != 0 {
				if n == 1 && !unicode.IsLetter(re.Rune[0]) {
					fmt.Fprintf(w, "if p < len(s) && s[p] == %q { return p+1 }\n", re.Rune[0])
				} else {
					fmt.Fprintf(w, "if p+%d <= len(s) && strings.EqualFold(s[p:p+%d], %q) { return p+%d }\n", n, n, string(re.Rune), n)
				}
			} else {
				if n == 1 {
					fmt.Fprintf(w, "if p < len(s) && s[p] == %q { return p+1 }\n", re.Rune[0])
				} else {
					fmt.Fprintf(w, "if p+%d <= len(s) && s[p:p+%d] == %q { return p+%d }\n", n, n, string(re.Rune), n)
				}
			}
			fmt.Fprintf(w, "return -1\n")

		case syntax.OpCharClass: // matches Runes interpreted as range pair list
			fmt.Fprintf(w, "if len(s) <= p { return -1 }\n")
			needDecode := false
			asciiSet := true
			for i := 0; i < len(re.Rune); i += 2 {
				l, r := re.Rune[i], re.Rune[i+1]
				ln, rn := utf8.RuneLen(l), utf8.RuneLen(r)
				if ln != 1 || rn != 1 {
					needDecode = true
				}
				if l > 0x7f || r > 0x7f || l != r {
					asciiSet = false
				}
			}
			if needDecode {
				fmt.Fprintf(w, "var (rn rune; n int)\n")
				decodeRune(w, "p", "rn", "n")
			} else {
				fmt.Fprintf(w, "rn := s[p]\n")
			}
			if asciiSet {
				if len(re.Rune) == 2 {
					fmt.Fprintf(w, "if rn == %q { return p+1 }\n", re.Rune[0])
				} else if len(re.Rune) == 4 {
					fmt.Fprintf(w, "if rn == %q || rn == %q { return p+1 }\n", re.Rune[0], re.Rune[2])
				} else {
					fmt.Fprintf(w, "switch rn {\n")
					fmt.Fprintf(w, "case ")
					for i := 0; i < len(re.Rune); i += 2 {
						if i != 0 {
							fmt.Fprintf(w, ",")
						}
						fmt.Fprintf(w, "%q", re.Rune[i])
					}
					fmt.Fprintf(w, ": return p+1\n")
					fmt.Fprintf(w, "}\n")
				}
			} else {
				fmt.Fprintf(w, "switch {\n")
				for i := 0; i < len(re.Rune); i += 2 {
					l, r := re.Rune[i], re.Rune[i+1]
					ln, rn := utf8.RuneLen(l), utf8.RuneLen(r)
					if ln == 1 && rn == 1 {
						if l == r {
							fmt.Fprintf(w, "case rn == %q: return p+1\n", l)
						} else {
							fmt.Fprintf(w, "case rn >= %q && rn <= %q: return p+1\n", l, r)
						}
					} else {
						if l == r {
							fmt.Fprintf(w, "case rn == %q: return p+n\n", l)
						} else {
							fmt.Fprintf(w, "case rn >= %q && rn <= %q: return p+n\n", l, r)
						}
					}
				}
				fmt.Fprintf(w, "}\n")
			}
			fmt.Fprintf(w, "return -1\n")

		case syntax.OpAnyCharNotNL: // matches any character except newline
			fmt.Fprintf(w, "var (rn rune; n int)\n")
			decodeRune(w, "p", "rn", "n")
			fmt.Fprintf(w, "if len(s) <= p+n || rn == '\\n' { return -1 }\n")
			fmt.Fprintf(w, "return p+n\n")

		case syntax.OpAnyChar: // matches any character
			fmt.Fprintf(w, "if p >= len(s) { return -1 }\n")
			fmt.Fprintf(w, "var n int\n")
			fmt.Fprintf(w, "if s[p] < utf8.RuneSelf {\n")
			fmt.Fprintf(w, "  n = 1\n")
			fmt.Fprintf(w, "} else {\n")
			fmt.Fprintf(w, "  _, n = utf8.DecodeRuneInString(s[p:])\n")
			fmt.Fprintf(w, "}\n")
			fmt.Fprintf(w, "if len(s) <= p+n { return -1 }\n")
			fmt.Fprintf(w, "return p+n\n")

		case syntax.OpWordBoundary, syntax.OpNoWordBoundary,
			syntax.OpBeginText, syntax.OpEndText,
			syntax.OpBeginLine, syntax.OpEndLine:
			fmt.Fprintf(w, "var l, u rune = -1, -1\n")
			fmt.Fprintf(w, "if p == 0 {\n")
			fmt.Fprintf(w, "  if p < len(s) {\n")
			decodeRune(w, "0", "u", "_")
			fmt.Fprintf(w, "  }\n")
			fmt.Fprintf(w, "} else if p == len(s) {\n")
			fmt.Fprintf(w, "  l, _ = utf8.DecodeLastRuneInString(s)\n")
			fmt.Fprintf(w, "} else {\n")
			fmt.Fprintf(w, "  l, _ = utf8.DecodeLastRuneInString(s[0:p])\n")
			decodeRune(w, "p", "u", "_")
			fmt.Fprintf(w, "}\n")
			fmt.Fprintf(w, "op := syntax.EmptyOpContext(l, u)\n")
			lut := map[syntax.Op]string{
				syntax.OpWordBoundary:   "EmptyWordBoundary",
				syntax.OpNoWordBoundary: "EmptyNoWordBoundary",
				syntax.OpBeginText:      "EmptyBeginText",
				syntax.OpEndText:        "EmptyEndText",
				syntax.OpBeginLine:      "EmptyBeginLine",
				syntax.OpEndLine:        "EmptyEndLine",
			}
			fmt.Fprintf(w, "if op & syntax.%s != 0 { return p }\n", lut[re.Op])
			fmt.Fprintf(w, "return -1\n")

		case syntax.OpCapture: // capturing subexpression with index Cap, optional name Name
			fmt.Fprintf(w, "np := l%d(s, p)\n", reid(re.Sub0[0]))
			fmt.Fprintf(w, "if np != -1 {\n")
			fmt.Fprintf(w, "  groups[%d] = p\n", re.Cap*2)
			fmt.Fprintf(w, "  groups[%d] = np\n", re.Cap*2+1)
			fmt.Fprintf(w, "}\n")
			fmt.Fprintf(w, "return np")

		case syntax.OpStar, syntax.OpPlus: // matches Sub[0] zero (one) or more times
			if countWork {
				// A loop advances p by each repetition, so the bytes it stepped over are its
				// end minus its start: one addition per loop instead of one per repetition.
				// Spans of uncountedLoopSpan bytes or fewer are not added (see there).
				fmt.Fprintf(w, "start := p\n")
			}
			if re.Op == syntax.OpPlus {
				fmt.Fprintf(w, "if p = l%d(s, p); p == -1 { return -1 }\n", reid(re.Sub0[0]))
			}
			fmt.Fprintf(w, "for len(s) > p {\n")
			if countWork {
				fmt.Fprintf(w, "if np := l%d(s, p); np == -1 { break } else { p = np }\n", reid(re.Sub0[0]))
				fmt.Fprintf(w, "}\n")
				fmt.Fprintf(w, "if n := p - start; n > %d { *work += n }\n", uncountedLoopSpan)
			} else {
				fmt.Fprintf(w, "if np := l%d(s, p); np == -1 { return p } else { p = np }\n", reid(re.Sub0[0]))
				fmt.Fprintf(w, "}\n")
			}
			fmt.Fprintf(w, "return p\n")

		case syntax.OpQuest: // matches Sub[0] zero or one times
			fmt.Fprintf(w, "if np := l%d(s, p); np != -1 { return np }\n", reid(re.Sub0[0]))
			fmt.Fprintf(w, "return p\n")

		case syntax.OpRepeat: // matches Sub[0] at least Min times, at most Max (Max == -1 is no limit)
			panic("??")

		case syntax.OpConcat: // matches concatenation of Subs
			for _, sub := range re.Sub {
				fmt.Fprintf(w, "if p = l%d(s, p); p == -1 { return -1 }\n", reid(sub))
			}
			fmt.Fprintf(w, "return p\n")

		case syntax.OpAlternate: // matches alternation of Subs
			for _, sub := range re.Sub {
				fmt.Fprintf(w, "if np := l%d(s, p); np != -1 { return np }\n", reid(sub))
			}
			fmt.Fprintf(w, "return -1\n")
		}
		fmt.Fprintf(w, "}\n")
	}
	fmt.Fprintf(w, "np := l%d(s, p)\n", reid(re))
	fmt.Fprintf(w, "if np == -1 {\n")
	fmt.Fprintf(w, "  return\n")
	fmt.Fprintf(w, "}\n")
	fmt.Fprintf(w, "groups[0] = p\n")
	fmt.Fprintf(w, "groups[1] = np\n")
	fmt.Fprintf(w, "return\n")
	fmt.Fprintf(w, "}\n")
	return nil
}

// This exists because of https://github.com/golang/go/issues/31666
func decodeRune(w io.Writer, offset string, rn string, n string) {
	// EOS must be a failed match, not a panic. Generated `\\.` (AnyCharNotNL)
	// used to index s[p] with no length check when the last byte was `\`.
	fmt.Fprintf(w, "if %s >= len(s) { return -1 }\n", offset)
	fmt.Fprintf(w, "if s[%s] < utf8.RuneSelf {\n", offset)
	fmt.Fprintf(w, "  %s, %s = rune(s[%s]), 1\n", rn, n, offset)
	fmt.Fprintf(w, "} else {\n")
	fmt.Fprintf(w, "  %s, %s = utf8.DecodeRuneInString(s[%s:])\n", rn, n, offset)
	fmt.Fprintf(w, "}\n")
}

func flatten(re *syntax.Regexp) (out []*syntax.Regexp) {
	for _, sub := range re.Sub {
		out = append(out, flatten(sub)...)
	}
	out = append(out, re)
	return
}

func isSimpleRuneRange(runes []rune) bool {
	for i := 0; i < len(runes); i += 2 {
		if runes[i] != runes[i+1] || utf8.RuneLen(runes[i]) != 1 {
			return false
		}
	}
	return true
}
