package i18n

import "errors"

// Error is a failure explained for the user: a message key and its
// arguments, so it can be shown in their language, an optional hint (what
// to do about it, also a key), and what the tool behind it actually said,
// for anyone who wants the details.
type Error struct {
	Key    string            `json:"key"`
	Args   map[string]string `json:"args,omitempty"`
	Hint   string            `json:"hint,omitempty"`
	Detail string            `json:"detail,omitempty"`
}

// Error is the message in English with the detail, for logs.
func (e *Error) Error() string {
	msg := T(Default, e.Key, e.Args)
	if e.Hint != "" {
		msg += " " + T(Default, e.Hint, e.Args)
	}
	if e.Detail != "" && e.Detail != msg {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

// In returns the message and hint in lang.
func (e *Error) In(lang string) string {
	msg := T(lang, e.Key, e.Args)
	if e.Hint != "" {
		msg += " " + T(lang, e.Hint, e.Args)
	}
	return msg
}

// New is an Error with a key and arguments given as name, value pairs.
func New(key string, args ...string) *Error {
	e := &Error{Key: key}
	if len(args) > 0 {
		e.Args = map[string]string{}
		for i := 0; i+1 < len(args); i += 2 {
			e.Args[args[i]] = args[i+1]
		}
	}
	return e
}

// WithHint adds a hint key to e.
func (e *Error) WithHint(hint string) *Error { e.Hint = hint; return e }

// WithDetail adds what the tool said to e.
func (e *Error) WithDetail(detail string) *Error { e.Detail = detail; return e }

// As finds an Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// From explains any error: an Error as it is, anything else as its own
// text (which is what a tool said that Whydah doesn't recognise).
func From(err error) *Error {
	if e, ok := As(err); ok {
		return e
	}
	return New("error.other", "detail", err.Error()).WithDetail(err.Error())
}
