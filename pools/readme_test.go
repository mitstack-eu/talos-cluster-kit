package pools_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The README's pool example is the body of ExampleParse, which go test builds and runs.
func TestREADME_PoolExampleIsTheBodyOfExampleParse(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	require.NoError(t, err)
	section := regexp.MustCompile("(?s)## How a block takes node pools.*?```go\n(.*?)```").FindSubmatch(readme)
	require.NotNil(t, section, "the README has no Go example under \"How a block takes node pools\"")
	source, err := os.ReadFile("example_test.go")
	require.NoError(t, err)
	body := regexp.MustCompile(`(?s)func ExampleParse\(\) \{\n(.*?)\n\}\n`).FindSubmatch(source)
	require.NotNil(t, body, "example_test.go has no ExampleParse")

	got := strings.ReplaceAll(strings.TrimPrefix(string(body[1]), "\t"), "\n\t", "\n") + "\n"

	assert.Equal(t, got, string(section[1]), "the README's example differs from ExampleParse")
}
