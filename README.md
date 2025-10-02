This project is the client golang application for a wrapper around a statically linked sqlite3_rsync.c file.

sqlite3_rsync allows a guaranteed replication with no-downtime or blocks on the main thread by using the special
build flag SQLITE_ENABLE_DBPAGE_VTAB when sqlite is compiled into the goproject.

## Features:
- PUSH and PULL rsync of sqlite3 database files to a remote SQLRsync server (defaults to sqlrsync.com)
- Creates a [`-sqlrsync` file](https://sqlrsync.com/help/dash-sqlrsync) file neighboring the replicated database which can be shared to PULL down the database elsewhere.
- Stores the PUSH key in ~/.config/sqlrsync/ to allow unattended re-PUSHing of the database (great for a cron job)
- LOCAL Sync (when sqlrsync is provided 2 arguments, both local file paths) allows for a local-only (no server/network use) rsyncing of a running write-node SQLite database to a running read-only SQLite database with no readlocks.
- The same mechanism works for PUSH and PULL: the ORIGIN can be a running write-node and the REPLICA can be a running read-only node.  It's pretty magical.

### In PUSH, PULL, or LOCAL mode:
- Use `--dry` to dry-run the command and see what mode it will trigger, and an explanation of what it will do

> <img width="600" height="157" alt="image" src="https://github.com/user-attachments/assets/1988770e-e79d-473a-bd3b-58815dfd6864" />

### When PUSHing:
- On initial upload you can use the `--public` or `--unlisted` flag to allow others to view the webpage for your database, and optionally download any version of it.
- Optionally include a commit message (`--message` or `-m`) to keep track of your changes

### When PULLing:
- You can append an `@` sign and a version reference to download old versions of the database.  For example:
```
sqlrsync oregon/elections.db           # Pulls down the latest version
sqlrsync oregon/elections.db@1         # Pulls down the first version
sqlrsync oregon/elections.db@latest-2  # Goes backwards 2 versions from the latest
```

>  <img width="553" height="74" alt="image" src="https://github.com/user-attachments/assets/1a6608d0-0d66-4801-be98-06ce158f8e6f" />

### Authentication and Authorization

There are 3 types of keys used by SQLRsync:
- Account Admin: Allows the creation of a new Database on REMOTE or PULLing any replica on the account 
- Replica Pull: Allows creating a local copy of any version of a specific existing Database on REMOTE to a local file
- Replica Push: Allows the creation of a new version of a specific existing Database on REMOTE

Account Admin keys are never stored locally, and are always interactively prompted or provided as a command line argument.

If you use an Account Admin key, the server will reply with a new replica-specific Pull key (and Push token if you use an Account Create key). The pull key is stored adjacent to the replicated database in a file ending with the suffix `-sqlrsync`, along with other data.  (For a database at /tmp/my-data.sqlite, the pull key would be stored in /tmp/my-data.sqlite-sqlrsync)

The more sensitive Replica Push key is stored in ~/.config/sqlrsync/local-secrets.toml and subsequent pushes will use that key.

## Technical Details and Contributing Guidelines

We're using CGO to directly call into sqlite_rsync.c to use the algorithm explicitly how the sqlite team
implemented the rsync function, however our API uses websockets to communicate between local and remote.

### Building

```
cd sqlite; make build
cd ../bridge; make build
cd ../client; make build
```

### Pre-compiled Binaries

Pre-compiled binaries for various platforms are available in the [releases](https://github.com/sqlrsync/client/releases) section of the GitHub repository.

- Mac (since 2020): sqlrsync-darwin-arm64
- Mac (before 2020): sqlrsync-darwin-amd64
- Linux: sqlrsync-linux-amd64
- Windows: sqlrsync-windows-amd64.exe

### Running

Run ./bin/sqlrsync <params>

### Application Logic and Settings

By default, REMOTE is SQLRsync.com Version Controlled Storage

### Stored Settings

Settings and defaults are stored in your user directory at ~/.config/sqlrsync. Within that directory, there are two files:

1. defaults.toml
   Contains default settings for all sqlrsync databases, like server URL, public/private, to generate a new unique clientSideEncryptionKey
```toml
# An example ~/.config/defaults.toml
[defaults]
server = "wss://sqlrsync.com"
```

2. local-secrets.toml
   Contains this-machine-specific settings, including the path to the local SQLite files, push keys, and encryption keys.

```toml
# An example ~/.config/local-secrets.toml
[local]
# When a new SQLRsync Replica is created on the server, we can use this prefix to identify this machine
hostname = "homelab3"
defaultClientSideEncryptionKey = "riot-camel-pass-flash-cereal-journey"

[[sqlrsync-databases]]
path = "/home/matt/webapps/hedgedoc/data/data.db"
replicaID = "AJK928AK02jidsJA1"
private-push-key = "abcd1234abcd1234"
clientSideEncryptionKey = "riot-camel-pass-flash-cereal-journey"
lastUpdated = "2023-01-01T00:00:00Z"
server = "wss://s9.sqlrsync.com"

[[sqlrsync-databases]]
path = "/home/matt/webapps/wikijs/data/another.db"
private-push-key = "efgh5678efgh5678"
lastUpdated = "2023-01-01T00:00:00Z"
clientSideEncryptionKey = "riot-camel-pass-flash-cereal-journey"
server = "wss://sqlrsync.com"
```
