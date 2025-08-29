package bridge

import "fmt"

// SQLiteRsyncError represents an error from the SQLite rsync C library
type SQLiteRsyncError struct {
	Code    int
	Message string
}

func (e *SQLiteRsyncError) Error() string {
	return fmt.Sprintf("%s (error code: %d)", e.Message, e.Code)
}
