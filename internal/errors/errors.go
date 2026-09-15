package errors

import (
	"fmt"
)

// Kind represents the type of an error.
type Kind string

const (
	KindNotFound    Kind = "not_found"
	KindConflict    Kind = "conflict"
	KindInvalid     Kind = "invalid"
	KindUnavailable Kind = "unavailable"
	KindInternal    Kind = "internal"
	KindPermission  Kind = "permission"
	KindTimeout     Kind = "timeout"
	// KindResourceExhausted: a finite resource (VIP pool addresses,
	// storage quota) has run out; retrying will not help until capacity
	// is freed.
	KindResourceExhausted Kind = "resource_exhausted"
)

// Error is a typed error used cluster-wide.
type Error struct {
	Kind    Kind
	Op      string
	Message string
	Err     error
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Op != "" {
		msg = fmt.Sprintf("%s:%s", e.Op, msg)
	}
	return string(e.Kind) + ": " + msg
}

func (e *Error) Unwrap() error {
	return e.Err
}

// New creates a typed error.
func New(kind Kind, op, msg string) *Error {
	return &Error{Kind: kind, Op: op, Message: msg}
}

// Wrap wraps an existing error with a typed layer.
func Wrap(err error, kind Kind, op, msg string) *Error {
	if err == nil {
		return New(kind, op, msg)
	}
	return &Error{Kind: kind, Op: op, Message: msg, Err: err}
}

// KindOf walks the entire error chain and returns the deepest non-empty Kind found, or "" if none.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var deepest Kind
	for err != nil {
		if e, ok := err.(*Error); ok && e.Kind != "" {
			deepest = e.Kind
		}
		err = unwrap(err)
	}
	return deepest
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// Is reports whether any error in the chain has kind k.
func Is(err error, k Kind) bool {
	for err != nil {
		if e, ok := err.(*Error); ok && e.Kind == k {
			return true
		}
		err = unwrap(err)
	}
	return false
}
