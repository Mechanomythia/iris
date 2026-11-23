// Package shellfmt makes a shell command readable in the approval popup: it
// splits it into highlighted spans and breaks long commands into lines at
// `;`, `&&`, `||` and `|`, outside quotes and substitutions. It only adds
// line breaks and indentation; every other character is shown as written.
package shellfmt

import (
	"strings"
)

// Span kinds (the popup maps them to colours).
const (
	Cmd     = "cmd"     // the program run by a pipeline segment
	Arg     = "arg"     // plain argument
	Flag    = "flag"    // -x, --long, --long=value
	Str     = "string"  // '...' or "..."
	Var     = "var"     // $X, ${X}, $(...), `...`, X=... assignments
	Op      = "op"      // ; && || | & ( ) { }
	Redir   = "redir"   // > >> < 2>&1 &> ...
	Keyword = "keyword" // for in do done if then else elif fi while until case esac
	Comment = "comment"
	Note    = "note" // "(latest Bash command)": iris's guess, not Claude's report
	Space   = "space"
	Break   = "break" // a newline (plus indentation in the next Space)
)

type Span struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

// Wrap: commands up to this many characters stay on one line.
const Wrap = 60

const notePrefix = "(latest Bash command) "

var keywords = map[string]bool{"for": true, "in": true, "do": true, "done": true, "if": true, "then": true,
	"else": true, "elif": true, "fi": true, "while": true, "until": true, "case": true, "esac": true, "function": true}

// cmdAfter: keywords after which the next word is a command.
var cmdAfter = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true}

// wrappers whose next word is still the command (sudo git ..., env X=1 glab ...).
var wrappers = map[string]bool{"sudo": true, "env": true, "time": true, "exec": true, "command": true, "nohup": true, "xargs": true}

type token struct {
	text string
	kind string
}

// Highlight returns the command as spans, line-broken when long.
func Highlight(cmd string) []Span {
	var out []Span
	if strings.HasPrefix(cmd, notePrefix) {
		out = append(out, Span{"latest Bash command", Note}, Span{"\n", Break})
		cmd = cmd[len(notePrefix):]
	}
	toks := classify(lex(cmd))
	long := len(cmd) > Wrap || strings.Contains(cmd, "\n")
	return append(out, layout(toks, long)...)
}

// Plain is Highlight as text (for the dialog fallback).
func Plain(cmd string) string {
	var b strings.Builder
	for _, s := range Highlight(cmd) {
		if s.Kind == Note {
			b.WriteString("(" + s.Text + ")")
			continue
		}
		b.WriteString(s.Text)
	}
	return b.String()
}

// lex splits into words, quoted strings, substitutions, operators and
// whitespace. Adjacent pieces of one shell word (--title="x") stay separate
// tokens but are glued with no space.
func lex(s string) []token {
	var toks []token
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '\n':
			toks = append(toks, token{"\n", Break})
			i++
		case c == ' ' || c == '\t':
			j := i
			for j < n && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			toks = append(toks, token{s[i:j], Space})
			i = j
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = n - i - 2
			}
			toks = append(toks, token{s[i : i+j+2], Str})
			i += j + 2
		case c == '"':
			j := i + 1
			for j < n && s[j] != '"' {
				if s[j] == '\\' {
					j++
				} else if s[j] == '$' && j+1 < n && s[j+1] == '(' {
					j = matchParen(s, j+1)
					continue
				}
				j++
			}
			j = min(j+1, n)
			toks = append(toks, token{s[i:j], Str})
			i = j
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				j = n - i - 2
			}
			toks = append(toks, token{s[i : i+j+2], Var})
			i += j + 2
		case c == '$':
			j := i + 1
			switch {
			case j < n && s[j] == '(':
				j = matchParen(s, j)
			case j < n && s[j] == '{':
				if k := strings.IndexByte(s[j:], '}'); k >= 0 {
					j += k + 1
				} else {
					j = n
				}
			case j < n && strings.IndexByte("?!#@*$-0123456789", s[j]) >= 0:
				j++
			default:
				for j < n && isWordByte(s[j]) {
					j++
				}
			}
			toks = append(toks, token{s[i:j], Var})
			i = j
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' || s[i-1] == ';'):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = n - i
			}
			toks = append(toks, token{s[i : i+j], Comment})
			i += j
		case c == '&' && i+1 < n && s[i+1] == '&', c == '|' && i+1 < n && s[i+1] == '|':
			toks = append(toks, token{s[i : i+2], Op})
			i += 2
		case c == '|' && i+1 < n && s[i+1] == '&':
			toks = append(toks, token{"|&", Op})
			i += 2
		case c == '&' && i+1 < n && s[i+1] == '>':
			j := i + 2
			if j < n && s[j] == '>' {
				j++
			}
			toks = append(toks, token{s[i:j], Redir})
			i = j
		case c == '>' || c == '<':
			toks = append(toks, token{s[i:redirEnd(s, i)], Redir})
			i = redirEnd(s, i)
		case c >= '0' && c <= '9' && i+1 < n && (s[i+1] == '>' || s[i+1] == '<') && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			j := redirEnd(s, i+1)
			toks = append(toks, token{s[i:j], Redir})
			i = j
		case strings.IndexByte(";|&(){}", c) >= 0:
			toks = append(toks, token{s[i : i+1], Op})
			i++
		case c == '\\' && i+1 < n:
			toks = append(toks, token{s[i : i+2], Arg})
			i += 2
		default:
			j := i
			for j < n && !isSpecial(s[j]) {
				if s[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			if j == i { // lone special char not handled above
				j++
			}
			toks = append(toks, token{s[i:j], Arg})
			i = j
		}
	}
	return toks
}

// redirEnd: end of a redirection operator at i ('>' or '<'), including
// >>, >&N, <<, <<<.
func redirEnd(s string, i int) int {
	j := i + 1
	for j < len(s) && (s[j] == '>' || s[j] == '<') && j-i < 3 {
		j++
	}
	if j < len(s) && s[j] == '&' {
		j++
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '-') {
			j++
		}
	}
	return j
}

// matchParen returns the index after the ')' matching s[open] == '('.
func matchParen(s string, open int) int {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\'':
			if k := strings.IndexByte(s[j+1:], '\''); k >= 0 {
				j += k + 1
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isSpecial(c byte) bool {
	return strings.IndexByte(" \t\n'\"`$;|&(){}<>", c) >= 0
}

// classify marks commands, flags, keywords and assignments.
func classify(toks []token) []token {
	expectCmd := true
	afterFor := false
	inWrapper := false // expecting the command after sudo/env/xargs: its flags come first
	glued := false     // previous token is part of the same shell word
	for i := range toks {
		t := &toks[i]
		switch t.kind {
		case Space, Break:
			glued = false
			if t.kind == Break {
				expectCmd = true
			}
			continue
		case Op:
			if t.text != ")" && t.text != "}" {
				expectCmd = true
			}
			glued = false
			continue
		case Redir, Comment:
			glued = false
			continue
		}
		if glued {
			continue // rest of a word like --title="x": keep its own colour
		}
		glued = true
		if t.kind != Arg {
			expectCmd = false
			inWrapper = false
			continue
		}
		w := t.text
		switch {
		case afterFor:
			afterFor = false
		case keywords[w] && (expectCmd || w == "in" || w == "do" || w == "done" || w == "then" || w == "fi" || w == "esac"):
			t.kind = Keyword
			afterFor = w == "for"
			expectCmd = cmdAfter[w]
			continue
		case expectCmd && isAssignment(w):
			t.kind = Var
			continue // still expecting the command
		case expectCmd && inWrapper && strings.HasPrefix(w, "-"):
			t.kind = Flag // xargs -n1 kubectl ...: still expecting the command
			continue
		case expectCmd:
			t.kind = Cmd
			expectCmd = wrappers[w]
			inWrapper = expectCmd
			continue
		case strings.HasPrefix(w, "-") && len(w) > 1:
			t.kind = Flag
		}
	}
	return toks
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		if !isWordByte(w[i]) {
			return false
		}
	}
	return !(w[0] >= '0' && w[0] <= '9')
}

// layout turns tokens into spans, breaking long commands at operators:
//
//	cd repo &&
//	  git push origin main 2>&1
//	  | tail -5
//
// `;` ends a line; `do`/`then` indent until `done`/`fi`.
func layout(toks []token, long bool) []Span {
	var out []Span
	depth := 0
	cont := false // the current line continues the previous one (indent +1)
	atLineStart := true
	indent := func() string {
		d := depth
		if cont {
			d++
		}
		return strings.Repeat("  ", d)
	}
	newline := func() {
		// drop trailing spaces before the break
		for len(out) > 0 && out[len(out)-1].Kind == Space {
			out = out[:len(out)-1]
		}
		out = append(out, Span{"\n", Break})
		atLineStart = true
	}
	emit := func(t token) {
		if atLineStart {
			if t.kind == Space {
				return // replaced by our indentation
			}
			if in := indent(); in != "" {
				out = append(out, Span{in, Space})
			}
			atLineStart = false
		}
		out = append(out, Span{t.text, t.kind})
	}
	for i, t := range toks {
		if t.kind == Break {
			newline()
			cont = false
			continue
		}
		if !long {
			emit(t)
			continue
		}
		switch {
		case t.kind == Op && (t.text == "|" || t.text == "|&"):
			// pipes start the next line: "| tail -5"
			newline()
			cont = true
			emit(t)
		case t.kind == Op && (t.text == "&&" || t.text == "||"):
			emit(t)
			newline()
			cont = true
		case t.kind == Op && t.text == ";":
			emit(t)
			if !nextIsKeyword(toks[i+1:], "do", "then") { // keep "for x in y; do"
				newline()
				cont = false
			}
		case t.kind == Keyword && (t.text == "do" || t.text == "then" || t.text == "else"):
			if t.text == "else" {
				depth = max(depth-1, 0)
				if !atLineStart {
					newline()
				}
			}
			emit(t)
			depth++
			newline()
			cont = false
		case t.kind == Keyword && (t.text == "done" || t.text == "fi" || t.text == "esac"):
			depth = max(depth-1, 0)
			if !atLineStart {
				newline()
			}
			cont = false
			emit(t)
		default:
			emit(t)
		}
	}
	for len(out) > 0 && (out[len(out)-1].Kind == Space || out[len(out)-1].Kind == Break) {
		out = out[:len(out)-1]
	}
	return out
}

func nextIsKeyword(toks []token, kws ...string) bool {
	for _, t := range toks {
		if t.kind == Space {
			continue
		}
		if t.kind != Keyword {
			return false
		}
		for _, k := range kws {
			if t.text == k {
				return true
			}
		}
		return false
	}
	return false
}

// Word is one shell word of a simple command (adjacent pieces joined).
type Word struct {
	Text string
	Kind string // kind of its first piece: Cmd, Arg, Flag, Str, Var, Keyword
}

// Commands splits cmd into simple commands (split at ; && || | & ( ) { }
// and newlines), including those inside $(...) and `...`, so a caller can
// see every program a command line runs. Redirection targets are dropped.
func Commands(cmd string) [][]Word {
	cmd = strings.TrimPrefix(cmd, notePrefix)
	var out [][]Word
	var cur []Word
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
		}
		cur = nil
	}
	toks := classify(lex(cmd))
	glued := false
	skipNext := false
	for _, t := range toks {
		switch t.kind {
		case Space, Comment:
			glued = false
			continue
		case Break, Op:
			flush()
			glued = false
			skipNext = false
			continue
		case Redir:
			glued = false
			// "2>&1" carries its target; "> file" takes the next word.
			skipNext = !strings.Contains(t.text, "&") || strings.HasSuffix(t.text, "&") || strings.HasPrefix(t.text, "&")
			continue
		}
		// Programs inside substitutions run too.
		for _, inner := range substitutions(t.text, t.kind) {
			out = append(out, Commands(inner)...)
		}
		if skipNext && !glued {
			skipNext = false
			glued = true
			continue
		}
		if glued && len(cur) > 0 {
			cur[len(cur)-1].Text += t.text
			continue
		}
		if glued { // glued to a skipped redirection target
			continue
		}
		cur = append(cur, Word{Text: t.text, Kind: t.kind})
		glued = true
	}
	flush()
	return out
}

// substitutions returns the bodies of $(...) and `...` in a token (not in
// single-quoted strings).
func substitutions(text, kind string) []string {
	if kind == Str && strings.HasPrefix(text, "'") {
		return nil
	}
	var out []string
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '$' && i+1 < len(text) && text[i+1] == '(':
			end := matchParen(text, i+1)
			if end-1 > i+2 {
				out = append(out, text[i+2:end-1])
			}
			i = end - 1
		case text[i] == '`':
			j := strings.IndexByte(text[i+1:], '`')
			if j < 0 {
				return out
			}
			out = append(out, text[i+1:i+1+j])
			i += j + 1
		}
	}
	return out
}
