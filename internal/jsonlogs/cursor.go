package jsonlogs

import (
	"context"
	"fmt"
	"strconv"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// CursorStore is the ingest_state slice the tailer needs, narrowed from
// *store.Store so a test can inject a fake without a second SQLite file.
type CursorStore interface {
	GetIngestState(ctx context.Context, key string) (store.IngestState, bool, error)
	SetIngestState(ctx context.Context, key string, state store.IngestState) error
}

func cursorKey(path string) string { return "jsonl:" + path }

// loadCursor returns path's stored byte offset, 0 when never seen or when
// the stored value doesn't parse -- a corrupt cursor re-reads from the
// start rather than failing the tail.
func loadCursor(ctx context.Context, cs CursorStore, path string) (int64, error) {
	st, ok, err := cs.GetIngestState(ctx, cursorKey(path))
	if err != nil || !ok {
		return 0, err
	}
	off, err := strconv.ParseInt(st.Value, 10, 64)
	if err != nil {
		return 0, nil
	}
	return off, nil
}

// saveCursor persists path's new byte offset.
func saveCursor(ctx context.Context, cs CursorStore, path string, offset int64, status, errMsg string) error {
	err := cs.SetIngestState(ctx, cursorKey(path), store.IngestState{
		Value:  strconv.FormatInt(offset, 10),
		Status: status,
		Error:  errMsg,
	})
	if err != nil {
		return fmt.Errorf("jsonlogs: saveCursor %s: %w", path, err)
	}
	return nil
}
