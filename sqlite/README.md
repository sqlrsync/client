# sqlrsync/client/sqlite

This is a custom build of the sqlite3 amalgamation for use with the sqlrsync project.

Specifically, we build with the SQLITE_ENABLE_DBPAGE_VTAB flag enabled to support the rsync functionality.
