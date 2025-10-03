#ifndef SQLDIFF_WRAPPER_H
#define SQLDIFF_WRAPPER_H

#ifdef __cplusplus
extern "C" {
#endif

// sqldiff_run compares two databases and returns SQL to transform db1 to db2
// db1: path to first database
// db2: path to second database
// result: pointer to receive SQL diff output (caller must free)
// error: pointer to receive error message if any (caller must free)
// returns: 0 on success, non-zero on error
int sqldiff_run(const char *db1, const char *db2, char **result, char **error);

#ifdef __cplusplus
}
#endif

#endif // SQLDIFF_WRAPPER_H
