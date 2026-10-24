package store

import (
	"errors"
	"strings"
)

// IsTransient reports whether err is a SQLite failure worth retrying: I/O
// (SQLITE_IOERR, primary code 10, which covers extended codes such as 266
// IOERR_READ) or out of memory (SQLITE_NOMEM, 7). Both are resource bursts
// that clear on their own.
//
// BUSY is deliberately not here: the DSN's busy_timeout already blocks up to
// five seconds inside one attempt, so retrying it would multiply that wait.
// Constraint violations are permanent and never match.
//
// The driver's error is matched by its Code method rather than its type, so
// this package keeps the driver as a blank import and tests can use a stub.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 10, 7:
			return true
		}
		return false
	}
	// modernc.org/sqlite returns a failed allocation as a plain fmt.Errorf
	// from conn.malloc (conn.go:615), with no result code to match.
	msg := err.Error()
	return strings.Contains(msg, "sqlite: cannot allocate ") && strings.Contains(msg, " bytes of memory")
}
