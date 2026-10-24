package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

type codedErr int

func (c codedErr) Error() string { return fmt.Sprintf("sqlite code %d", int(c)) }
func (c codedErr) Code() int     { return int(c) }

func TestIsTransient(t *testing.T) {
	// A real unique-constraint violation from the driver, not a stub.
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.db.ExecContext(context.Background(), `CREATE TABLE u (k TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatal(err)
	}
	st.db.ExecContext(context.Background(), `INSERT INTO u VALUES ('a')`)
	_, uniq := st.db.ExecContext(context.Background(), `INSERT INTO u VALUES ('a')`)
	if !isUniqueConstraintError(uniq) {
		t.Fatalf("fixture did not produce a unique violation: %v", uniq)
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"IOERR", codedErr(10), true},
		{"IOERR_READ as logged", codedErr(266), true},
		{"NOMEM", codedErr(7), true},
		{"wrapped NOMEM", fmt.Errorf("insert: %w", codedErr(7)), true},
		{"BUSY", codedErr(5), false},
		{"real unique constraint", uniq, false},
		{"plain allocation error", errors.New("sqlite: cannot allocate 4096 bytes of memory"), true},
		{"wrapped allocation error", fmt.Errorf("flush: %w", errors.New("sqlite: cannot allocate 1 bytes of memory")), true},
		{"other plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, want %v", c.name, got, c.want)
		}
	}
}
