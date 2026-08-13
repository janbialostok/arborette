// Package provides shared utilities used across all refactored services.

package utils

import "errors"

var (
	ErrNotFound   = errors.New("resource not found")
	ErrInvalidID  = errors.New("invalid ID")
	ErrCascadeFail = errors.New("cascading deletion failed")
)
