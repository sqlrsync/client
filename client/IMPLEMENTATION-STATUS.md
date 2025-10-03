# PUSH Subscription Implementation Status

## ✅ Completed Features

### 1. CLI Flags

- `--subscribe`: Enable subscription mode for both PUSH and PULL
- `--waitIdle`: Time to wait for idleness before pushing (e.g., 10m, 1h30m, 3d) - min 10 seconds
- `--maxInterval`: Maximum time between pushes regardless of activity (e.g., 24h, 1w)
- `--minInterval`: Minimum time between subsequent pushes (defaults to 1/2 maxInterval)

### 2. Duration Parsing

- Custom duration parser supporting: s, m, h, d (days), w (weeks)
- Does not support months (as specified)
- Validates waitIdle min (10 seconds) and max (24 days)

### 3. File Watching

- Uses fsnotify to monitor SQLite database and WAL files
- Detects Write and Create events
- Non-blocking change notifications via channel
- Callback sends SQLRSYNC_CHANGED notifications to server for analytics

### 4. PUSH Subscription Core Logic

- **Initial PUSH**: Performs initial PUSH when subscription starts
- **Key type validation**: Checks if PULL key is being used and switches to PULL subscription mode
- **File change detection**: Monitors both .sqlite and .sqlite-wal files
- **Singleton waitIdle timer**: Timer that resets on each file change for another MAX(waitIdle OR lastPush - minInterval). On expiration, performs PUSH and resets sessionStarted=false
- **First write notification**: Sends SQLRSYNC_CHANGED with waitIdle duration, sets sessionStarted=true
- **Subsequent write notifications**: If resend timer isn't running, starts timer for (waitIdle - 10 seconds). When timer fires, resends SQLRSYNC_CHANGED with remaining duration
- **minInterval enforcement**: Ensures minimum spacing between pushes
- **maxInterval enforcement**: Forces push if maxInterval time has elapsed since last push
- **Timer reset on change**: Each file change recalculates and resets the timer

### 5. Error Handling

- Exponential backoff with retry (5 attempts)
- Delays: 5s, 10s, 20s, 40s, 80s
- Error reporting to server stub (HTTPS POST to $server/sapi/$replicaID) - not yet implemented
- Continues watching even after push failures

### 6. User Notifications

- Progress messages for all push operations
- Clear status updates during file watching
- Timer duration logging in verbose mode
- Automatic warning when PULL key is used with --subscribe flag

### 7. Protocol Messages

```go
const (
    SQLRSYNC_CONFIG            = 0x51  // Config with keys, replicaID, keyType
    SQLRSYNC_NEWREPLICAVERSION = 0x52  // New version available
    SQLRSYNC_KEYREQUEST        = 0x53  // Request keys
    SQLRSYNC_COMMITMESSAGE     = 0x54  // Commit message
    SQLRSYNC_CHANGED           = 0x57  // Write detected notification with duration
)
```

**SQLRSYNC_CHANGED Format:**
- `[0x57][duration: 4 bytes as seconds]`
- Sent on first write with waitIdle duration
- Resent 10 seconds before expiration if there were subsequent writes
- Contains remaining time until push will occur

### 8. Key Type Detection

- Server sends `keyType` field in CONFIG message (`"PUSH"` or `"PULL"`)
- Client parses keyType from CONFIG JSON (remote/client.go:1303-1306)
- If PULL key detected with --subscribe flag, client automatically switches to PULL subscription mode
- Prevents misuse of --waitIdle/--maxInterval/--minInterval with PULL keys

## 🚧 TODO: Remaining Work

### 1. Error Reporting to Server

Stub exists at `coordinator.go:912-914`. Need to implement:

```go
func (c *Coordinator) reportErrorToServer(err error) {
    // HTTPS POST to $server/sapi/$replicaID
    // Include error message
    // Only if failures persist for > 5 minutes
}
```

## 📝 Usage Examples

### Basic PUSH subscription

```bash
sqlrsync mydb.sqlite namespace/mydb.sqlite --subscribe --waitIdle 10m --maxInterval 24h
```

- Pushes after 10 minutes of idleness
- Forces push at least every 24 hours even with continuous activity
- Sends SQLRSYNC_CHANGED on first write, resends 10s before expiration if subsequent writes

### Custom intervals

```bash
sqlrsync mydb.sqlite namespace/mydb.sqlite --subscribe \
  --waitIdle 5m --minInterval 10m --maxInterval 1h
```

- Timer = MAX(10m - timeSinceLastPush, 5m)
- If last push was 3 min ago: timer = MAX(7m, 5m) = 7m
- If last push was 12 min ago: timer = MAX(-2m, 5m) = 5m

### Using with PULL key (auto-switches)

```bash
# This will automatically switch to PULL subscription mode
sqlrsync namespace/mydb.sqlite --subscribe --waitIdle 10m
```

- Client detects PULL key from server CONFIG
- Ignores --waitIdle/--maxInterval/--minInterval
- Switches to standard PULL subscription mode

## 🏗️ Architecture

### File Structure

```
client/
├── main.go                    # CLI flags, operation routing
├── sync/
│   ├── coordinator.go         # Main sync orchestration + PUSH subscribe
│   └── duration.go           # Duration parsing utilities
├── watcher/
│   └── watcher.go            # File system monitoring
├── remote/
│   └── client.go             # WebSocket client with CHANGED notifications
└── subscription/
    └── manager.go            # PULL subscription (existing)
```

### Flow for PUSH Subscribe

1. Parse and validate duration parameters (min 10s for waitIdle)
2. Execute initial PUSH
3. Establish persistent WebSocket connection
4. Check key type from CONFIG - switch to PULL subscription if PULL key detected
5. Start file watcher
6. Event loop:
   - Check for maxInterval timeout
   - Wait for file changes
   - On first write: send SQLRSYNC_CHANGED with waitIdle duration, set sessionStarted=true
   - On subsequent writes: schedule resend for 10s before expiration
   - Apply timing logic (waitIdle, minInterval)
   - Execute PUSH with retry on timer expiration
   - Reset sessionStarted=false after successful push

## 🔄 Server-Side Responsibilities

### Conflict Resolution

When a client pushes and is not in sync:

1. Pull down the version the client was aware of → **OLD**
2. Pull down the latest version → **LATEST**
3. Treat client's current copy as → **CANDIDATE**
4. Run `sqlite_diff OLD CANDIDATE` → **second.diff**
5. Run `sqlite_diff OLD LATEST` → **first.diff**
6. If no overlap between primary keys in both diffs:
   - Apply second.diff to LATEST
   - Push result as new version
   - Discard CANDIDATE

### Key Type Validation

- Server sends `keyType` in CONFIG message
- Valid values: `"PUSH"` or `"PULL"`
- Client uses this to validate operation mode
- Prevents PUSH operations with PULL-only keys

### Analytics

- Server receives SQLRSYNC_CHANGED (0x57) messages with duration
- Can track client activity and expected push timing
- Updates on subsequent writes provide refined timing expectations
