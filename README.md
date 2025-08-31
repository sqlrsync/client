This project is the client golang application for a wrapper around a statically linked sqlite3_rsync.c file.

sqlite3_rsync allows a guaranteed replication with no-downtime or blocks on the main thread by using the special
build flag SQLITE_ENABLE_DBPAGE_VTAB when sqlite is compiled into the goproject.

We're using CGO to directly call into sqlite_rsync.c to use the algorithm explicitly how the sqlite team
implemented the rsync function, however our API uses websockets to communicate between local and remote.

## Building

```
cd sqlite; make build
cd ../bridge; make build
cd ../client; make build
```

## Pre-compiled Binaries

Pre-compiled binaries for various platforms are available in the [releases](https://github.com/sqlrsync/client/releases) section of the GitHub repository.

* Mac (since 2020): sqlrsync-darwin-arm64
* Mac (before 2020): sqlrsync-darwin-amd64
* Linux: sqlrsync-linux-amd64
* Windows: sqlrsync-windows-amd64.exe

## Running

Run ./bin/sqlrsync <params>

## Stored Settings

Settings and defaults are stored in your user directory at ~/.config/sqlrsync.  Within that directory, there are two files:

1) defaults.toml
   Contains default settings for all sqlrsync databases, like server URL, public/private, to generate a new unique clientSideEncryptionKey

2) local-secrets.toml
   Contains this-machine-specific settings, including the path to the local SQLite files, push keys, and encryption keys.

```toml
[local]
# When a new SQLRsync Replica is created on the server, we can use this prefix to identify this machine
hostname = "homelab3"
defaultClientSideEncryptionKey = ""

[[sqlrsync-databases]]
path = "/home/matt/webapps/hedgedoc/data/data.db"
private-push-key = "abcd1234abcd1234"
lastUpdated = "2023-01-01T00:00:00Z"
clientSideEncryptionKey = ""

[[sqlrsync-databases]]
path = "/home/matt/webapps/wikijs/data/another.db"
private-push-key = "efgh5678efgh5678"
lastUpdated = "2023-01-01T00:00:00Z"
clientSideEncryptionKey = ""
```

