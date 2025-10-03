# Auto-Merge Implementation Status

## ✅ Completed Features

### 1. CLI Flag

- `--merge`: Automatically merge changes when server has newer version

### 2. Version Conflict Detection

**Location:** `remote/client.go:1279-1297`

- Detects `ABORT=VERSION_CONFLICT:versionNumber` message from server
- Stores conflict state and latest version number
- Provides methods:
  - `HasVersionConflict() bool` - Check if conflict occurred
  - `GetLatestVersion() string` - Get server's latest version
  - `ResetVersionConflict()` - Clear conflict state

### 3. Auto-Merge Flow

**Location:** `sync/coordinator.go:537-557`

When PUSH fails with version conflict and `--merge` is enabled:

1. ✅ **Create temp file** - `/tmp/sqlrsync-merge-local-*.sqlite`
2. ✅ **Copy local to temp** - Uses LOCAL mode (`RunDirectSync`)
3. ✅ **PULL latest over temp** - Fetches server version and applies to temp
4. ⚠️ **Generate diff** - Currently stubbed, needs sqldiff integration
5. ⚠️ **Check conflicts** - Currently stubbed, needs primary key conflict detection
6. ✅ **Apply changes** - Copies merged result back to local
7. ✅ **Retry PUSH** - Attempts push again with merged data

### 4. Conflict Notification

**Location:** `sync/coordinator.go:1141-1158`

When conflicts are detected:
- Prepares notification with:
  - Type: `"merge-conflict"`
  - Diff data (base64 encoded)
  - Version information
  - Hostname and wsID
- ⚠️ **TODO**: Implement HTTP POST to `$server/sapi/notification/account/$replicaName/`

## 🚧 TODO: Remaining Work

### 1. SQLDiff Integration

Need to implement actual diff generation and parsing:

```go
// Generate diff between temp (latest) and local (our changes)
// Using sqldiff tool or equivalent
diffOutput := runSQLDiff(tempPath, c.config.LocalPath)

// Parse diff output to extract primary key operations
conflicts := parseDiffForConflicts(diffOutput)
```

**Reference:** https://raw.githubusercontent.com/sqlite/sqlite/refs/heads/master/tool/sqldiff.c

### 2. Primary Key Conflict Detection

Parse diff output to detect conflicting operations:

```go
type DiffOperation struct {
    Type       string // INSERT, UPDATE, DELETE
    Table      string
    PrimaryKey map[string]interface{}
}

func parseDiffForConflicts(diffOutput []byte) ([]DiffOperation, error) {
    // Parse SQL diff output
    // Identify operations on same primary keys
    // Return conflicting operations
}
```

### 3. HTTP Notification Implementation

Implement the POST request to notify server of conflicts:

```go
func (c *Coordinator) sendMergeConflictNotification(serverURL, replicaName, version string, diffData []byte) error {
    payload := map[string]interface{}{
        "type":     "merge-conflict",
        "diff":     base64.StdEncoding.EncodeToString(diffData),
        "versions": []string{c.config.Version, version},
        "hostname": hostname,
        "wsID":     wsID,
    }

    url := fmt.Sprintf("%s/sapi/notification/account/%s/", serverURL, replicaName)
    // POST JSON payload
}
```

## 📝 Usage Examples

### Basic Auto-Merge

```bash
sqlrsync mydb.sqlite namespace/mydb.sqlite --merge
```

When pushing and server has newer version:
1. Automatically pulls latest version
2. Merges changes (if no conflicts)
3. Retries push with merged data

### Auto-Merge with Subscription

```bash
sqlrsync mydb.sqlite namespace/mydb.sqlite --subscribe --waitIdle 10m --merge
```

- Watches for local changes
- Pushes after 10 minutes idle
- Auto-merges if version conflict occurs
- Continues watching after successful merge

### Expected Behavior

#### No Conflicts
```
🔄 Performing PUSH...
⚠️  Version conflict: Server has newer version 42
🔄 Auto-merge enabled - attempting to merge with server version 42...
📋 Step 1/5: Copying local database to temp file...
📥 Step 2/5: Pulling latest version 42 from server over temp file...
🔍 Step 3/5: Generating diff between temp (latest) and local (your changes)...
✅ Step 4/5: No conflicts detected
📝 Step 5/5: Applying changes to local database...
✅ Merge completed successfully
✅ Auto-merge successful - retrying PUSH...
✅ PUSH completed
```

#### With Conflicts
```
🔄 Performing PUSH...
⚠️  Version conflict: Server has newer version 42
🔄 Auto-merge enabled - attempting to merge with server version 42...
📋 Step 1/5: Copying local database to temp file...
📥 Step 2/5: Pulling latest version 42 from server over temp file...
🔍 Step 3/5: Generating diff between temp (latest) and local (your changes)...
❌ Merge conflict detected - server blocking until manual resolution
   Server: wss://sqlrsync.com
   Replica: namespace/mydb.sqlite
   Version: 42
   Hostname: my-laptop
   wsID: abc123
Error: merge conflict requires manual resolution
```

## 🏗️ Architecture

### Flow Diagram

```
┌─────────────┐
│ PUSH Failed │
│  (version   │
│  conflict)  │
└──────┬──────┘
       │
       ▼
   ┌───────────┐
   │ --merge?  │
   └─────┬─────┘
         │ yes
         ▼
   ┌──────────────────┐
   │ Copy LOCAL       │
   │ to /tmp/file     │
   └────────┬─────────┘
            │
            ▼
   ┌──────────────────┐
   │ PULL latest      │
   │ over /tmp/file   │
   └────────┬─────────┘
            │
            ▼
   ┌──────────────────┐
   │ Generate diff    │
   │ (sqldiff)        │
   └────────┬─────────┘
            │
            ▼
   ┌──────────────────┐
   │ Check PK         │
   │ conflicts?       │
   └─────┬────────────┘
         │
    ┌────┴────┐
    │         │
   no        yes
    │         │
    ▼         ▼
┌────────┐  ┌──────────────┐
│ Apply  │  │ POST to      │
│ diff   │  │ /sapi/notify │
└───┬────┘  └──────┬───────┘
    │              │
    ▼              ▼
┌────────┐  ┌──────────────┐
│ Retry  │  │ Error:       │
│ PUSH   │  │ Manual fix   │
└────────┘  └──────────────┘
```

### Server Requirements

The server needs to:

1. **Detect version conflicts** - When client pushes with old version number
2. **Send ABORT message** - Format: `ABORT=VERSION_CONFLICT:latestVersion`
3. **Handle notification endpoint** - `POST /sapi/notification/account/{replicaName}/`
   - Accept JSON payload with conflict details
   - Block replica until manual resolution
   - Notify account owner of conflict

### Client State

During auto-merge, the client maintains:
- Original local database (unchanged until merge succeeds)
- Temp file with server's latest version
- Diff between temp and local
- Conflict detection results
- Version numbers (old and new)

## 🔐 Security Considerations

1. **Temp file cleanup** - Always cleaned up via `defer os.Remove(tempPath)`
2. **Authentication** - Uses same auth as regular PULL/PUSH
3. **Version validation** - Server must validate version numbers to prevent replay attacks
4. **Conflict data** - Diff data may contain sensitive information, ensure HTTPS for notifications
