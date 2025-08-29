/*
** 2024-09-10
**
** The author disclaims copyright to this source code.  In place of
** a legal notice, here is a blessing:
**
**    May you do good and not evil.
**    May you find forgiveness for yourself and forgive others.
**    May you share freely, never taking more than you give.
**
*************************************************************************
**
** This is the header file for sqlite_rsync.c - a utility that makes a
** copy of a live SQLite database using a bandwidth-efficient protocol,
** similar to "rsync".
*/

#ifndef SQLITE_RSYNC_H_INCLUDED
#define SQLITE_RSYNC_H_INCLUDED

#include <stdio.h>
#include <stdlib.h>
#include "sqlite3.h"

#ifdef __cplusplus
extern "C"
{
#endif

  /* Type definitions */
  typedef unsigned char u8;
  typedef sqlite3_uint64 u64;

/* Protocol version */
#define PROTOCOL_VERSION 2

/* Magic numbers for messages sent over the wire */
/**** Baseline: protocol version 1 ****/
#define ORIGIN_BEGIN 0x41 /* Initial message */
#define ORIGIN_END 0x42   /* Time to quit */
#define ORIGIN_ERROR 0x43 /* Error message from the remote */
#define ORIGIN_PAGE 0x44  /* New page data */
#define ORIGIN_TXN 0x45   /* Transaction commit */
#define ORIGIN_MSG 0x46   /* Informational message */
/**** Added in protocol version 2 ****/
#define ORIGIN_DETAIL 0x47 /* Request finer-grain hash info */
#define ORIGIN_READY 0x48  /* Ready for next round of hash exchanges */

/**** Baseline: protocol version 1 ****/
#define REPLICA_BEGIN 0x61 /* Welcome message */
#define REPLICA_ERROR 0x62 /* Error. Report and quit. */
#define REPLICA_END 0x63   /* Replica wants to stop */
#define REPLICA_HASH 0x64  /* One or more pages hashes to report */
#define REPLICA_READY 0x65 /* Ready to receive page content */
#define REPLICA_MSG 0x66   /* Informational message */
/**** Added in protocol version 2 ****/
#define REPLICA_CONFIG 0x67 /* Hash exchange configuration */

  /* Context structure for the sync operation */
  typedef struct SQLiteRsync SQLiteRsync;
  struct SQLiteRsync
  {
    const char *zOrigin;    /* Name of the origin */
    const char *zReplica;   /* Name of the replica */
    const char *zErrFile;   /* Append error messages to this file */
    const char *zDebugFile; /* Append debugging messages to this file */
    FILE *pOut;             /* Transmit to the other side */
    FILE *pIn;              /* Receive from the other side */
    FILE *pLog;             /* Duplicate output here if not NULL */
    FILE *pDebug;           /* Write debug info here if not NULL */
    sqlite3 *db;            /* Database connection */
    int nErr;               /* Number of errors encountered */
    int nWrErr;             /* Number of failed attempts to write on the pipe */
    u8 eVerbose;            /* Bigger for more output. 0 means none. */
    u8 bCommCheck;          /* True to debug the communication protocol */
    u8 isRemote;            /* On the remote side of a connection */
    u8 isReplica;           /* True if running on the replica side */
    u8 iProtocol;           /* Protocol version number */
    u8 wrongEncoding;       /* ATTACH failed due to wrong encoding */
    u8 bWalOnly;            /* Require WAL mode */
    sqlite3_uint64 nOut;    /* Bytes transmitted */
    sqlite3_uint64 nIn;     /* Bytes received */
    unsigned int nPage;     /* Total number of pages in the database */
    unsigned int szPage;    /* Database page size */
    u64 nHashSent;          /* Hashes sent (replica to origin) */
    unsigned int nRound;    /* Number of hash batches (replica to origin) */
    unsigned int nPageSent; /* Page contents sent (origin to replica) */
  };

  /* Hash context structure */
  typedef struct HashContext HashContext;
  struct HashContext
  {
    union
    {
      u64 s[25];             /* Keccak state. 5x5 lines of 64 bits each */
      unsigned char x[1600]; /* ... or 1600 bytes */
    } u;
    unsigned nRate;   /* Bytes of input accepted per Keccak iteration */
    unsigned nLoaded; /* Input bytes loaded into u.x[] so far this cycle */
    unsigned ixMask;  /* Insert next input into u.x[nLoaded^ixMask]. */
    unsigned iSize;   /* 224, 256, 358, or 512 */
  };

  /* Function declarations */

  /* Core sync functions */
  void originSide(SQLiteRsync *p);
  void replicaSide(SQLiteRsync *p);

  /* Hash functions */
  static int hashRegister(sqlite3 *db);
  static void hashFunc(sqlite3_context *context, int argc, sqlite3_value **argv);
  static void agghashStep(sqlite3_context *context, int argc, sqlite3_value **argv);
  static void agghashFinal(sqlite3_context *context);

  /* Utility functions */
  const char *file_tail(const char *z);
  sqlite3_int64 currentTime(void);
  int append_escaped_arg(sqlite3_str *pStr, const char *zIn, int isFilename);
  void add_path_argument(sqlite3_str *pStr);

  /* Communication functions */
  static int readUint32(SQLiteRsync *p, unsigned int *pU);
  static int writeUint32(SQLiteRsync *p, unsigned int x);
  int readByte(SQLiteRsync *p);
  void writeByte(SQLiteRsync *p, int c);
  int readPow2(SQLiteRsync *p);
  void writePow2(SQLiteRsync *p, int c);
  void readBytes(SQLiteRsync *p, int nByte, void *pData);
  void writeBytes(SQLiteRsync *p, int nByte, const void *pData);

  /* Process management */
  static int popen2(const char *zCmd, FILE **ppIn, FILE **ppOut, int *pChildPid, int bDirect);
  static void pclose2(FILE *pIn, FILE *pOut, int childPid);

/* Platform-specific functions */
#ifdef _WIN32
  int win32_create_child_process(wchar_t *zCmd, void *hIn, void *hOut, void *hErr, unsigned long *pChildPid);
  void *win32_utf8_to_unicode(const char *zUtf8);
#endif

/* Pointer/integer conversion macros */
#if defined(__PTRDIFF_TYPE__)
#define INT_TO_PTR(X) ((void *)(__PTRDIFF_TYPE__)(X))
#define PTR_TO_INT(X) ((int)(__PTRDIFF_TYPE__)(X))
#elif !defined(__GNUC__)
#define INT_TO_PTR(X) ((void *)&((char *)0)[X])
#define PTR_TO_INT(X) ((int)(((char *)X) - (char *)0))
#elif defined(HAVE_STDINT_H)
#define INT_TO_PTR(X) ((void *)(intptr_t)(X))
#define PTR_TO_INT(X) ((int)(intptr_t)(X))
#else
#define INT_TO_PTR(X) ((void *)(X))
#define PTR_TO_INT(X) ((int)(X))
#endif

/* Main function (can be disabled with SQLITE_RSYNC_NO_MAIN) */
#ifndef SQLITE_RSYNC_NO_MAIN
  int main(int argc, char const *const *argv);
#endif // SQLITE_RSYNC_NO_MAIN

#ifdef __cplusplus
}
#endif

#endif // SQLiteRsync
