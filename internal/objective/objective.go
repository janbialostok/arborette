// Package objective is the legacy name for the question package.
//
// Deprecated: Use github.com/arborette/arborette/internal/question instead.
package objective

import "github.com/arborette/arborette/internal/question"

type Objective = question.Question

var (
	ErrNoObjective         = question.ErrNoQuestion
	ErrMissingAggregation  = question.ErrMissingAggregation
	ErrNonNumericValue     = question.ErrNonNumericValue
)

var Pin               = question.Pin
var ExecuteRequestFor = question.ExecuteRequestFor
var NumericValue      = question.NumericValue
var Improves          = question.Improves
