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
