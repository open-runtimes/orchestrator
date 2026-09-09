// Package callback holds the payload schema shared by every callback the
// orchestrator emits. The CloudEvents envelope around it is transport; these
// are the types a subscriber actually reads.
package callback

import (
	"unicode"
	"unicode/utf8"
)

// Failure is the error field of any callback that reports one: a stable
// snake_case code to branch on, plus human-readable detail for this specific
// occurrence. Consumers must treat an unknown code as a generic failure and
// show the message rather than parsing it.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Fail builds a Failure, casing the message as a sentence. Go error strings
// are lowercase by convention; a subscriber shows the message to a person.
func Fail(code, message string) Failure {
	if r, n := utf8.DecodeRuneInString(message); n > 0 {
		message = string(unicode.ToUpper(r)) + message[n:]
	}
	return Failure{Code: code, Message: message}
}
