package bridge

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo LDFLAGS: -lsqlite3

#include <stdlib.h>
#include <string.h>
#include "sqldiff_wrapper.h"
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// DiffResult contains the result of a diff operation
type DiffResult struct {
	SQL        string              // SQL statements to transform db1 to db2
	HasChanges bool                // Whether there are any differences
	Operations []DiffOperation     // Parsed operations from the diff
	Conflicts  []PrimaryKeyConflict // Detected primary key conflicts
}

// DiffOperation represents a single SQL operation from the diff
type DiffOperation struct {
	Type       string                 // INSERT, UPDATE, DELETE
	Table      string                 // Table name
	PrimaryKey map[string]interface{} // Primary key values
	SQL        string                 // The actual SQL statement
}

// PrimaryKeyConflict represents a conflict on the same primary key
type PrimaryKeyConflict struct {
	Table      string
	PrimaryKey map[string]interface{}
	Operation1 string // First operation (from db1)
	Operation2 string // Second operation (from db2)
}

// RunSQLDiff compares two SQLite databases and returns the differences
// db1Path: path to first database (baseline/old version)
// db2Path: path to second database (new version with changes)
// Returns SQL statements that would transform db1 into db2
func RunSQLDiff(db1Path, db2Path string) (*DiffResult, error) {
	cDb1 := C.CString(db1Path)
	cDb2 := C.CString(db2Path)
	defer C.free(unsafe.Pointer(cDb1))
	defer C.free(unsafe.Pointer(cDb2))

	var cResult *C.char
	var cError *C.char

	// Call the C wrapper function
	rc := C.sqldiff_run(cDb1, cDb2, &cResult, &cError)

	if rc != 0 {
		if cError != nil {
			errMsg := C.GoString(cError)
			C.free(unsafe.Pointer(cError))
			return nil, fmt.Errorf("sqldiff failed: %s", errMsg)
		}
		return nil, fmt.Errorf("sqldiff failed with code %d", rc)
	}

	result := &DiffResult{
		HasChanges: false,
		Operations: []DiffOperation{},
		Conflicts:  []PrimaryKeyConflict{},
	}

	if cResult != nil {
		result.SQL = C.GoString(cResult)
		result.HasChanges = len(result.SQL) > 0
		C.free(unsafe.Pointer(cResult))

		// Parse the SQL to extract operations
		result.Operations = parseSQL(result.SQL)

		// Detect primary key conflicts
		result.Conflicts = detectConflicts(result.Operations)
	}

	return result, nil
}

// parseSQL parses SQL diff output into individual operations
func parseSQL(sql string) []DiffOperation {
	// TODO: Implement SQL parsing
	// For now, return empty slice
	// This would parse INSERT, UPDATE, DELETE statements
	// and extract table names and primary keys
	return []DiffOperation{}
}

// detectConflicts finds operations that conflict on the same primary key
func detectConflicts(operations []DiffOperation) []PrimaryKeyConflict {
	// TODO: Implement conflict detection
	// This would find cases where:
	// - Same PK has multiple UPDATE/DELETE operations
	// - INSERT on existing PK
	// etc.
	return []PrimaryKeyConflict{}
}

// ApplyDiff applies a diff to a database
// This executes the SQL statements from a DiffResult
func ApplyDiff(dbPath string, diff *DiffResult) error {
	if !diff.HasChanges {
		return nil // Nothing to apply
	}

	if len(diff.Conflicts) > 0 {
		return fmt.Errorf("cannot apply diff with %d conflicts", len(diff.Conflicts))
	}

	// TODO: Open database and execute SQL
	// For now, we'll need to implement this using the bridge
	return fmt.Errorf("ApplyDiff not yet implemented")
}
