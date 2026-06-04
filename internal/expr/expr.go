// Package expr is a thin wrapper around act's pkg/exprparser so the rest of
// actl evaluates GitHub Actions ${{ }} expressions through one seam.
package expr

import (
	"fmt"

	"github.com/nektos/act/pkg/exprparser"
)

// Evaluate computes a single GitHub Actions expression against env.
//
// input may be wrapped in ${{ }} or bare (act's interpreter strips the prefix
// itself). env supplies the contexts available to the expression (github, env,
// secrets, ...); pass an empty &exprparser.EvaluationEnvironment{} when none
// are needed.
func Evaluate(input string, env *exprparser.EvaluationEnvironment) (interface{}, error) {
	interp := exprparser.NewInterpeter(env, exprparser.Config{})
	v, err := interp.Evaluate(input, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return nil, fmt.Errorf("evaluate %q: %w", input, err)
	}
	return v, nil
}
