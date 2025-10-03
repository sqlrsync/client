# SQLDiff Integration

## Overview

Created a CGO wrapper to integrate SQLite's sqldiff functionality into the auto-merge feature.

## Files Created

### 1. `/bridge/sqldiff_wrapper.h`
C header file defining the interface:
```c
int sqldiff_run(const char *db1, const char *db2, char **result, char **error);
```

### 2. `/bridge/sqldiff_wrapper.c`
C implementation that:
- Opens both databases in read-only mode
- Currently returns a placeholder diff
- **TODO**: Integrate full sqldiff.c logic once `sqlite3_stdio.h` is available

### 3. `/bridge/cgo_sqldiff.go`
Go wrapper providing:

**Main Function:**
```go
func RunSQLDiff(db1Path, db2Path string) (*DiffResult, error)
```

**Data Structures:**
```go
type DiffResult struct {
    SQL        string              // SQL statements to transform db1 to db2
    HasChanges bool                // Whether there are any differences
    Operations []DiffOperation     // Parsed operations from the diff
    Conflicts  []PrimaryKeyConflict // Detected primary key conflicts
}

type DiffOperation struct {
    Type       string                 // INSERT, UPDATE, DELETE
    Table      string                 // Table name
    PrimaryKey map[string]interface{} // Primary key values
    SQL        string                 // The actual SQL statement
}

type PrimaryKeyConflict struct {
    Table      string
    PrimaryKey map[string]interface{}
    Operation1 string // First operation
    Operation2 string // Second operation
}
```

**Helper Function:**
```go
func ApplyDiff(dbPath string, diff *DiffResult) error
```

## Integration with Auto-Merge

### Location: `sync/coordinator.go:1112-1156`

The auto-merge flow now:

1. **Creates temp file** - Copies local database
2. **Pulls latest** - Gets server's version over temp
3. **Generates diff** - Calls `bridge.RunSQLDiff(tempPath, localPath)`
4. **Checks for changes** - Returns early if databases are identical
5. **Detects conflicts** - Checks for primary key conflicts
6. **Sends notification** - If conflicts exist, notifies server
7. **Applies diff** - If no conflicts, applies changes
8. **Retries push** - Pushes merged result

### Usage in Code

```go
diffResult, err := bridge.RunSQLDiff(tempPath, c.config.LocalPath)
if err != nil {
    return fmt.Errorf("failed to generate diff: %w", err)
}

if !diffResult.HasChanges {
    fmt.Println("✅ No changes detected - databases are identical")
    return nil
}

if len(diffResult.Conflicts) > 0 {
    fmt.Printf("❌ Detected %d primary key conflict(s)\n", len(diffResult.Conflicts))
    return c.sendMergeConflictNotification(serverURL, remotePath, latestVersion, []byte(diffResult.SQL))
}

if err := bridge.ApplyDiff(tempPath, diffResult); err != nil {
    // Fallback to simple copy
    c.logger.Warn("Failed to apply diff, using direct copy", zap.Error(err))
}
```

## Current Status

### ✅ Completed
- CGO wrapper infrastructure
- Go type definitions
- Integration with coordinator
- Error handling and fallbacks
- Placeholder diff generation

### 🚧 TODO

#### 1. Full SQLDiff Integration
The `sqldiff.c` file is present but not compiled due to missing `sqlite3_stdio.h`.

**Next Steps:**
- Obtain `sqlite3_stdio.h` and `sqlite3_stdio.c` from SQLite ext/misc
- Remove `//go:build ignore` tag from `sqldiff.c`
- Modify `sqldiff_wrapper.c` to call actual diff functions
- Update CGO build flags if needed

#### 2. SQL Parsing (parseSQL)
Currently stubbed in `cgo_sqldiff.go:75-82`.

**Needs:**
```go
func parseSQL(sql string) []DiffOperation {
    // Parse SQL statements
    // Extract INSERT/UPDATE/DELETE operations
    // Identify table names and primary keys
    // Return structured operations
}
```

#### 3. Conflict Detection (detectConflicts)
Currently stubbed in `cgo_sqldiff.go:85-92`.

**Algorithm:**
```go
func detectConflicts(operations []DiffOperation) []PrimaryKeyConflict {
    // Group operations by table and primary key
    // Find cases where same PK has multiple operations:
    //   - Multiple UPDATEs on same PK
    //   - UPDATE + DELETE on same PK
    //   - INSERT on existing PK
    // Return conflicts
}
```

#### 4. Diff Application (ApplyDiff)
Currently stubbed in `cgo_sqldiff.go:95-105`.

**Needs:**
```go
func ApplyDiff(dbPath string, diff *DiffResult) error {
    // Open database
    // Begin transaction
    // Execute each SQL statement from diff
    // Commit transaction
    // Handle errors with rollback
}
```

## Testing Strategy

### Unit Tests Needed

1. **TestRunSQLDiff** - Test diff generation
   - Identical databases → no changes
   - Simple INSERT → detected
   - UPDATE operation → detected
   - DELETE operation → detected

2. **TestParseSQL** - Test SQL parsing
   - Parse INSERT statements
   - Parse UPDATE statements
   - Parse DELETE statements
   - Extract primary keys correctly

3. **TestDetectConflicts** - Test conflict detection
   - No conflicts case
   - UPDATE + UPDATE conflict
   - INSERT + INSERT conflict
   - UPDATE + DELETE conflict

4. **TestApplyDiff** - Test diff application
   - Apply simple changes
   - Rollback on error
   - Verify final state

### Integration Tests Needed

1. **TestAutoMergeNoConflict** - Full merge flow without conflicts
2. **TestAutoMergeWithConflict** - Merge with conflicts triggers notification
3. **TestAutoMergeFallback** - Falls back to copy on apply failure

## Build Configuration

The sqldiff.c is currently excluded from build:
```c
//go:build ignore
```

Once `sqlite3_stdio.h` is available, remove this tag and ensure:
- CGO can find all required headers
- SQLite library is linked properly
- Build succeeds on all platforms

## Dependencies

- SQLite3 library (`-lsqlite3`)
- C standard library
- `sqlite3_stdio.h` (pending)
- `sqlite3_stdio.c` (pending)

## Error Handling

The wrapper handles errors at multiple levels:

1. **C Level** - Opens databases, checks SQLite errors
2. **CGO Level** - Converts C strings/errors to Go
3. **Go Level** - Returns structured errors with context
4. **Coordinator Level** - Falls back to simple copy if diff fails

## Future Enhancements

1. **Streaming Diffs** - For very large databases
2. **Incremental Parsing** - Parse SQL as it's generated
3. **Custom Conflict Resolution** - User-defined merge strategies
4. **Diff Caching** - Cache diffs for retry scenarios
5. **Progress Reporting** - Show diff generation progress
