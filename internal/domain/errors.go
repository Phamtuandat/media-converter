package domain

import "fmt"

const (
	CodeRequestInvalid = "request_invalid"
	CodeConflict       = "conflict"
)

type Error struct {
	Code      string
	Message   string
	Retryable bool
	Stage     string
	Err       error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code, message, stage string, retryable bool, err error) *Error {
	return &Error{Code: code, Message: message, Stage: stage, Retryable: retryable, Err: err}
}
