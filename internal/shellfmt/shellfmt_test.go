package shellfmt

import (
	"strings"
	"testing"
)

// render shows kinds inline: cmd as <git>, flags as [--x], strings as {..}.
func render(spans []Span) string {
	var b strings.Builder
	for _, s := range spans {
		switch s.Kind {
		case Cmd:
			b.WriteString("<" + s.Text + ">")
		case Flag:
			b.WriteString("[" + s.Text + "]")
		case Str:
			b.WriteString("{" + s.Text + "}")
		case Var:
			b.WriteString("%" + s.Text + "%")
		case Keyword:
			b.WriteString("#" + s.Text + "#")
		case Redir:
			b.WriteString("^" + s.Text + "^")
		case Note:
			b.WriteString("(" + s.Text + ")")
		default:
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

func TestHighlight(t *testing.T) {
	cases := []struct{ in, want string }{
		{`git status`, `<git> status`},
		{`kubectl get pods 2>&1 | head -40`, `<kubectl> get pods ^2>&1^ | <head> [-40]`},
		{"(latest Bash command) cd /Users/joost/repos/remote-access-monorepo; glab mr create --draft --title \"feat(boldore): dual-write\" --description \"$(cat /tmp/x.md)\" --yes 2>&1 | tail -5",
			"(latest Bash command)\n<cd> /Users/joost/repos/remote-access-monorepo;\n<glab> mr create [--draft] [--title] {\"feat(boldore): dual-write\"} [--description] {\"$(cat /tmp/x.md)\"} [--yes] ^2>&1^\n  | <tail> [-5]"},
		{`for p in digital-pvp-prod digital-pvp; do echo "=== $p"; gcloud functions list --project $p 2>&1 | head -80; done`,
			"#for# p #in# digital-pvp-prod digital-pvp; #do#\n  <echo> {\"=== $p\"};\n  <gcloud> functions list [--project] %$p% ^2>&1^\n    | <head> [-80];\n#done#"},
		{`KH=$TMPDIR/kh; GIT_SSH_COMMAND="$GIT_SSH_COMMAND -o X" git push -u origin feature/x 2>&1 | grep -v 'Operation not permitted' | tail -10`,
			"%KH=%%$TMPDIR%/kh;\n%GIT_SSH_COMMAND=%{\"$GIT_SSH_COMMAND -o X\"} <git> push [-u] origin feature/x ^2>&1^\n  | <grep> [-v] {'Operation not permitted'}\n  | <tail> [-10]"},
		{`cd /a/very/long/path/that/goes/on && make test && echo done-and-done-and-done`,
			"<cd> /a/very/long/path/that/goes/on &&\n  <make> test &&\n  <echo> done-and-done-and-done"},
		{`sudo env FOO=1 glab api user`, `<sudo> <env> %FOO=1% <glab> api user`},
		{`echo a > /tmp/x; cat < /tmp/x`, `<echo> a ^>^ /tmp/x; <cat> ^<^ /tmp/x`},
	}
	for _, c := range cases {
		if got := render(Highlight(c.in)); got != c.want {
			t.Errorf("\n in: %s\ngot:\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}

// Nothing but whitespace may change.
func TestPreservesText(t *testing.T) {
	for _, in := range []string{
		`for p in a b; do echo "=== $p"; gcloud functions list --project $p --format='table(name.basename(),state)' 2>&1 | head -80; done`,
		`glab mr view 510 -F json 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["title"])'`,
		"echo `date` ${HOME} $? \\; done unterminated 'quote",
		`if [ -f x ]; then echo yes; else echo no; fi`,
	} {
		norm := func(s string) string { return strings.Join(strings.Fields(s), "") }
		var b strings.Builder
		for _, s := range Highlight(in) {
			b.WriteString(s.Text)
		}
		if norm(b.String()) != norm(in) {
			t.Errorf("text changed:\n in: %s\nout: %s", in, b.String())
		}
	}
}
