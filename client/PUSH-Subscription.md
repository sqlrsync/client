PUSH Subscription mode is a new feature for SQLRsync that allows
you to optionally automatically PUSH changes from the LOCAL to
REMOTE.

The feature works by using OS native file watchers to monitor changes
to the file, and when a change is detected, begin a PUSH operation after
a variable delay. Users specify how long to a fixed amount of time since
the last change.

The subscription mode is activated by adding the --subscribe flag to a
PUSH command. The first time the command is run, it will do a normal
PUSH, and then begin monitoring the file for changes. When a change is
detected, it will wait the specified delay time (or auto mode) and then
begin a PUSH operation. If another change is detected during the wait
time, the wait timer is reset UNLESS it has been X hours since the last
PUSH, in which case it will immediately begin a PUSH operation.

Example:
sqlrsync mydb.sqlite namespace/mydb.sqlite --subscribe --waitIdle 10m --maxInterval 24h

Wait for 10 minutes of idleness before pushing, but in the event that there's
activity every 8 minutes for multiple days straight, it will at least push
24 hours after the last push.

Example:
sqlrsync mydb.sqlite namespace/mydb.sqlite -m "turn on push subscribe" --subscribe --waitIdle 10m --maxInterval 24h

With push subscribe the database is checked to see if PRAGMA busy_timeout is
greater than waitIdle. If not, it asks the user what value they want to increase
it to and sets it.

## Coordination Mode (Optional)

If --coordinate is specified, the client opts into the coordination system. Each client
configures their own waitIdle and maxInterval values independently - these are NOT shared
between clients.

When --coordinate is enabled:
- Client does NOT send SQLRSYNC_COORDINATE to other clients
- Client registers with server as participating in coordination
- First file change detected:
  - Client sends `SQLRSYNC_CLAIM $waitIdle` to server
  - Starts extension timer for `waitIdle - 10 seconds`
- Subsequent writes:
  - Sets flag "had additional writes since CLAIM"
  - Does NOT send CLAIM immediately (minimizes traffic)
- When extension timer fires (10 seconds before claim expires):
  - If flag is set: sends `SQLRSYNC_CLAIM $remainingWaitIdle`
  - Where `remainingWaitIdle = waitIdle - (seconds since last write)`
  - Starts new timer for `remainingWaitIdle - 10 seconds`
  - If no additional writes: lets original claim expire naturally
- Server rebroadcasts CLAIM to all other coordinating clients for this replica
- Other clients suppress their own PUSH attempts during the claim window (polite yield)

Without --coordinate:
- No CLAIM system is used
- Sends `SQLRSYNC_CHANGED` notifications to server for analytics
- All clients push independently based on their local timers
- Risk of concurrent pushes and conflicts (accepted risk)
- Server handles conflicts via SQLite rsync page-level merging

If we are subscribing with these PUSH flags, then we will PUSH when the command starts.

--minInterval, which defaults to 1/2 maxInterval but otherwise specifies the minimum
gap between subsequent PUSHes.

The server will warn and ignore if --waitIdle, --maxInterval, or --minInterval are used with a PULL Key.

--waitIdle, --minInterval, and --maxInterval both allow time durations like 3s, 10m, 1h30m, 2h, 1h45m, 3d, 1w, 4w, etc. They do not support months.

The max for waitIdle is 24 days because busy_time is non-negative integer representing milliseconds, which is 24.8 days. The min is 1 second.

Example:
sqlrsync mydb.sqlite namespace/mydb.sqlite -m "turn on coordinated subscribe" --subscribe --waitIdle 10m --maxInterval 24h --coordinate

## Split-Brain Handling

When two clients send SQLRSYNC_CLAIM simultaneously:
- Server accepts the first CLAIM it receives
- Responds to first client with `SQLRSYNC_CLAIM` (no arguments) = acknowledgment
- Broadcasts to all other coordinating clients: `SQLRSYNC_CLAIM $waitIdle` (using claimer's waitIdle)
- Second client receives the other client's CLAIM instead of acknowledgment
- Second client suppresses its push attempts for that waitIdle duration
- Server ignores subsequent CLAIMs until the current claim's waitIdle expires

Note: CLAIMS do not block local writes - both databases continue accepting writes.
The coordination only affects when pushes are attempted.

In addition to --waitIlde, --maxInterval, another flag is

Areas for Consideration & Additional Scenarios

1. Error Handling & Recovery
   Network failures: What happens if a PUSH fails due to network issues? Should it retry with exponential backoff?
   Matt says: Yes, exponential backoff using the existing exponential backoff code. The client will do an HTTPS POST request to $server/sapi/$replicaID with
   the error message if it has been more than 5 minutes of failure.

Partial sync failures: How should the system handle cases where a PUSH partially completes?
Matt says: that'd be handled the same as a network failure: exponential backoff. The client will do an HTTPS POST request to $server/sapi/$replicaID with
the error message 100% of the time.

Server unavailability: Should the system queue changes locally and sync when the server becomes available?
Matt says: the above network and partial sync cases handle that with existing exponential backoff code.

2. File System Edge Cases
   Temporary files: Should the file watcher ignore temporary files created by editors (e.g., .tmp, .swp files)?
   Matt says: yes, both the name of the file and -wal should be monitored.

Atomic writes: How does it handle applications that write to temporary files and then rename them?
Matt says: The sqlite spec may do that to the local.db-wal file, but that will always trigger a traditional write to the local.db file

Multiple rapid changes: What about scenarios with hundreds of small changes in quick succession?
Matt says: the --waitIdle code quickly ignores it if it hasn't been 75% of the waitIdle time or --maxInterval since the last PUSH

File locking: What happens if the SQLite database is locked when trying to PUSH?
Matt says: The sqlrsync client PUSHes without needing a write lock by using a special control table in the sqlite database that has already been built into sqlrsync.

3. Coordination Protocol Edge Cases
   Split-brain scenarios: What if network partitions cause multiple clients to think they have the claim?
   Matt says: the server is single threaded and we accept that the first claim the server received will win.

Client crashes: How is a stale claim cleaned up if a client crashes while holding it?
Matt says: the claim is time-limited to waitIdle, so if a client crashes while holding it, the claim will expire after waitIdle time.

Clock skew: How does the system handle clients with different system times?
Matt says: figuring in internet latency, since times are relative, other systems might be off by a few seconds.

Late joiners: What happens when a new client subscribes to a database that already has active claims? 4. Performance & Resource Management
Matt says: The server will send a CLAIM when it connects if one already exists.

Resource cleanup: How are file watchers cleaned up when subscription ends?
Matt says: These are long running permanent processes that should be added to systemd or similar to ensure they are restarted if they crash.

Memory usage: For long-running subscriptions, how is memory managed?
Matt says: The file watcher uses minimal memory - mostly waiting, and not using any dynamic memory.

CPU usage: What's the impact of continuous file watching on system resources? 5. User Experience
Matt says: I believe there are hooks into the OS that use no resources until a change is detected.

Status visibility: How can users see the current subscription status, active claims, etc.?
Matt says: This is handled serverside.

Graceful shutdown: How should users stop subscription mode?
Matt says: This is already handled by ctrl-c

Logging: What level of logging should be provided for debugging coordination issues?
Matt says: Verbose logging is already available and can be enabled with the --verbose flag.

# Specific documentation for the server side

CLAIMS are sent from the worker to the durable for the account, which makes decisions and coordinates for that since it will be less resource constrained.

CLAIMS do not block other writes. Both will be saved.

# Questions for Claude

1. How do concurrent writes work?

**Answer**: Based on the specification, here's how concurrent writes are handled:

## Without --coordinate flag (Default, No Coordination)
- Each client operates independently with no coordination
- The timer logic (`MAX(minInterval - timeSinceLastPush, waitIdle)`) prevents too-frequent pushes from same client
- All clients can push whenever their timers expire
- Server accepts all pushes in order received
- Risk of concurrent pushes and conflicts (accepted risk)
- Server handles conflicts via SQLite rsync page-level merging

## With --coordinate flag (Optional Coordination)
- **--coordinate is optional** - each client opts in individually
- **Each client configures own waitIdle/maxInterval** - these are NOT shared between clients
- **CLAIMS do not block local writes** - all local databases continue accepting writes

When Client A (with --coordinate) detects a change:
1. First write:
   - Sends `SQLRSYNC_CLAIM $waitIdle` to server
   - Starts extension timer for `waitIdle - 10 seconds`
2. Subsequent writes:
   - Sets flag "had additional writes"
   - Does NOT send CLAIM yet
3. When extension timer fires (10s before claim expires):
   - If flag set: sends `SQLRSYNC_CLAIM $remainingWaitIdle`
   - Where `remainingWaitIdle = waitIdle - (seconds since last write)`
   - Starts new timer for `remainingWaitIdle - 10 seconds`

Server (via durable object) handles CLAIM:
- Accepts first CLAIM for a replica
- Responds to Client A: `SQLRSYNC_CLAIM` (no args) = acknowledgment
- Broadcasts to other coordinating clients: `SQLRSYNC_CLAIM $waitIdle` (using A's waitIdle)

Other coordinating clients receiving the CLAIM:
- **Do NOT block local writes** (application continues writing)
- Set a timer for received $waitIdle duration
- Suppress their own PUSH and CLAIM attempts during this window (polite yield)
- After timer expires, can push accumulated changes

Client A completes its PUSH, then other clients can push their accumulated changes.

## Conflict Resolution
Since CLAIMS don't block writes and coordination is optional:
1. Client A (coordinating) writes locally + claims
2. Client B (coordinating) writes locally but yields on push
3. Client C (no --coordinate) writes and pushes independently
4. Server accepts pushes in order received
5. Conflicts resolved using SQLite rsync protocol (page-level merging)

The coordination is about **reducing push collisions** and **giving polite turn-taking** among coordinating clients, not preventing concurrent local writes. Non-coordinating clients can still push anytime.
