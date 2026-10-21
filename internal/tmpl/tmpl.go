// Package tmpl parses the kit's manifest templates with the functions they share.
package tmpl

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Must parses text as the template name, with indent available, and panics on a malformed template.
func Must(name, text string) *template.Template {
	return template.Must(template.New(name).Funcs(template.FuncMap{"indent": Indent}).Parse(text))
}

// Execute renders t with data and returns the bytes.
func Execute(t *template.Template, data any) ([]byte, error) {
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", t.Name(), err)
	}
	return out.Bytes(), nil
}

// Indent prefixes every non-empty line of s with n spaces, so a file sits inside a YAML block scalar.
func Indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n")
}
