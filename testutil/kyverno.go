package testutil

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
)

// Admits evaluates a Kyverno ValidatingPolicy's variables and validations, as Kyverno does, for one object in one namespace.
func Admits(t *testing.T, policy, object, namespace map[string]any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.OptionalTypes(), cel.Variable("object", cel.DynType), cel.Variable("namespaceObject", cel.DynType),
		cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)))
	require.NoError(t, err)
	variables := map[string]any{}
	eval := func(item any) any {
		expression := Field(t, item, "expression").(string)
		ast, issues := env.Compile(expression)
		require.NoError(t, issues.Err(), expression)
		program, err := env.Program(ast)
		require.NoError(t, err)
		out, _, err := program.Eval(map[string]any{"object": object, "namespaceObject": namespace, "variables": variables})
		require.NoError(t, err, expression)
		return out.Value()
	}
	declared, _ := Field(t, policy, "spec", "variables").([]any)
	for _, v := range declared {
		variables[Field(t, v, "name").(string)] = eval(v)
	}
	for _, v := range Field(t, policy, "spec", "validations").([]any) {
		if eval(v) != true {
			return false
		}
	}
	return true
}
