AUTO MERGE:

If the server responds that there was a newer version that we didn't know about but we've already PUSHed some changes, the server rejects the PUSH and tells us what the latest version is. If the user provides the --merge flag, then we:

- use LOCAL mode to copy this database to /tmp/sqlrsync-merge-local\*randomstuff
- use PULL mode to grab latest and assert it over that /tmp file
- use sqlrsync --diff to diff the /tmp vs LOCAL and generate a patch file
- if the patch doesn't have any conflicting primary keys, then apply it and push, otherwise send a call to POST serverURL/sapi/notification/account/replicaName/. The server will block this server until a human resolves the conflict.

Message body for post:
{ type: "merge-conflict", the diff file as base64, the versions impacted, hostname, wsID }

Here's the source code for https://raw.githubusercontent.com/sqlite/sqlite/refs/heads/master/tool/sqldiff.c
