#include "sqlite_rsync.h"
#include "sqlite_rsync_wrapper.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#ifdef _WIN32
#include <process.h>
#else
#include <unistd.h>
#include <sys/wait.h>
#endif

#ifndef SQLITE_RSYNC_WRAPPER_C_INCLUDED
#define SQLITE_RSYNC_WRAPPER_C_INCLUDED

#define USE_DEBUG

// Now we can use the functions directly
// #define sqlite_rsync_originSide originSide
// #define sqlite_rsync_replicaSide replicaSide

// Global I/O context for callbacks
static sqlite_rsync_io_context_t *g_io_ctx = NULL;

// Forward declaration for cleanup function
static void cleanup_websocket_streams(void);

// Override the original I/O functions to use WebSocket callbacks
int sqlite_rsync_read_byte()
{
  uint8_t byte;
  if (g_io_ctx && g_io_ctx->read_func)
  {
    int result = g_io_ctx->read_func(g_io_ctx->user_data, &byte, 1);
    return result == 1 ? byte : -1;
  }
  return -1;
}

int sqlite_rsync_write_byte(uint8_t byte)
{
  if (g_io_ctx && g_io_ctx->write_func)
  {
    return g_io_ctx->write_func(g_io_ctx->user_data, &byte, 1);
  }
  return -1;
}

int sqlite_rsync_read_bytes(uint8_t *buffer, int size)
{
  if (g_io_ctx && g_io_ctx->read_func)
  {
    return g_io_ctx->read_func(g_io_ctx->user_data, buffer, size);
  }
  return -1;
}

int sqlite_rsync_write_bytes(const uint8_t *buffer, int size)
{
  if (g_io_ctx && g_io_ctx->write_func)
  {
    return g_io_ctx->write_func(g_io_ctx->user_data, (uint8_t *)buffer, size);
  }
  return -1;
}

// Initialize SQLite rsync with WebSocket I/O
int sqlite_rsync_init_websocket(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx)
{
  if (!config || !io_ctx)
  {
    return -1;
  }

  g_io_ctx = io_ctx;
  return 0;
}

// Get database information
int sqlite_rsync_get_db_info(const char *db_path, sqlite_db_info_t *info)
{
  if (!db_path || !info)
  {
    return -1;
  }

  sqlite3 *db;
  int rc = sqlite3_open_v2(db_path, &db, SQLITE_OPEN_READONLY, NULL);
  if (rc != SQLITE_OK)
  {
    return -1;
  }

  // Get page size
  sqlite3_stmt *stmt;
  rc = sqlite3_prepare_v2(db, "PRAGMA page_size", -1, &stmt, NULL);
  if (rc == SQLITE_OK && sqlite3_step(stmt) == SQLITE_ROW)
  {
    info->page_size = sqlite3_column_int(stmt, 0);
  }
  else
  {
    info->page_size = 4096; // default
  }
  sqlite3_finalize(stmt);

  // Get page count
  rc = sqlite3_prepare_v2(db, "PRAGMA page_count", -1, &stmt, NULL);
  if (rc == SQLITE_OK && sqlite3_step(stmt) == SQLITE_ROW)
  {
    info->page_count = sqlite3_column_int(stmt, 0);
  }
  else
  {
    info->page_count = 0;
  }
  sqlite3_finalize(stmt);

  // Get journal mode
  rc = sqlite3_prepare_v2(db, "PRAGMA journal_mode", -1, &stmt, NULL);
  if (rc == SQLITE_OK && sqlite3_step(stmt) == SQLITE_ROW)
  {
    const char *mode = (const char *)sqlite3_column_text(stmt, 0);
    strncpy(info->journal_mode, mode ? mode : "delete", sizeof(info->journal_mode) - 1);
    info->journal_mode[sizeof(info->journal_mode) - 1] = '\0';
  }
  else
  {
    strcpy(info->journal_mode, "delete");
  }
  sqlite3_finalize(stmt);

  sqlite3_close(db);
  return 0;
}

// Cleanup resources
void sqlite_rsync_cleanup(void)
{
  cleanup_websocket_streams();
  g_io_ctx = NULL;
}

// Bridge to the actual sqlite_rsync.c implementation
// We need to override the I/O functions that sqlite_rsync.c uses

// These functions are called by sqlite_rsync.c for I/O operations
// We override them to use our WebSocket callbacks

// WebSocket FILE* bridge implementation
// We need to create custom FILE* streams that use WebSocket I/O callbacks

#include <unistd.h>
#include <fcntl.h>
#include <sys/types.h>
#include <sys/stat.h>

// Pipe file descriptors for bridging FILE* to WebSocket I/O
static int g_read_pipe[2] = {-1, -1};  // [read_end, write_end]
static int g_write_pipe[2] = {-1, -1}; // [read_end, write_end]
static FILE *g_websocket_in = NULL;
static FILE *g_websocket_out = NULL;

// Background thread function declarations
static void *websocket_read_thread(void *arg);
static void *websocket_write_thread(void *arg);

#include <pthread.h>
static pthread_t g_read_thread = 0;
static pthread_t g_write_thread = 0;
static volatile int g_threads_running = 0;

// Initialize WebSocket FILE* bridge with pipes
static int init_websocket_streams()
{
  // Create pipes for bidirectional communication
  if (pipe(g_read_pipe) == -1 || pipe(g_write_pipe) == -1)
  {
    return -1;
  }

  // Convert read pipe's read end to FILE* for sqlite_rsync to read from
  g_websocket_in = fdopen(g_read_pipe[0], "rb");
  if (!g_websocket_in)
  {
    close(g_read_pipe[0]);
    close(g_read_pipe[1]);
    close(g_write_pipe[0]);
    close(g_write_pipe[1]);
    return -1;
  }

  // Convert write pipe's write end to FILE* for sqlite_rsync to write to
  g_websocket_out = fdopen(g_write_pipe[1], "wb");
  if (!g_websocket_out)
  {
    fclose(g_websocket_in);
    close(g_read_pipe[1]);
    close(g_write_pipe[0]);
    close(g_write_pipe[1]);
    return -1;
  }

  // Start background threads to bridge WebSocket <-> pipes
  g_threads_running = 1;
  if (pthread_create(&g_read_thread, NULL, websocket_read_thread, NULL) != 0 ||
      pthread_create(&g_write_thread, NULL, websocket_write_thread, NULL) != 0)
  {
    g_threads_running = 0;
    fclose(g_websocket_in);
    fclose(g_websocket_out);
    close(g_read_pipe[1]);
    close(g_write_pipe[0]);
    return -1;
  }

  return 0;
}

// Clean up WebSocket FILE* bridge
static void cleanup_websocket_streams()
{
  g_threads_running = 0;

  // Close FILE* streams (this will also close the underlying fds)
  if (g_websocket_in)
  {
    fclose(g_websocket_in);
    g_websocket_in = NULL;
  }
  if (g_websocket_out)
  {
    fclose(g_websocket_out);
    g_websocket_out = NULL;
  }

  // Close remaining pipe ends
  if (g_read_pipe[1] != -1)
  {
    close(g_read_pipe[1]);
    g_read_pipe[1] = -1;
  }
  if (g_write_pipe[0] != -1)
  {
    close(g_write_pipe[0]);
    g_write_pipe[0] = -1;
  }

  // Wait for threads to finish
  if (g_read_thread)
  {
    pthread_join(g_read_thread, NULL);
    g_read_thread = 0;
  }
  if (g_write_thread)
  {
    pthread_join(g_write_thread, NULL);
    g_write_thread = 0;
  }
}

// Thread that reads from WebSocket and writes to read_pipe
static void *websocket_read_thread(void *arg)
{
  uint8_t buffer[4101];

  while (g_threads_running && g_io_ctx && g_io_ctx->read_func)
  {
    int bytes_read = g_io_ctx->read_func(g_io_ctx->user_data, buffer, sizeof(buffer));
    if (bytes_read > 0)
    {
      // Write to pipe for sqlite_rsync to read
      int written = write(g_read_pipe[1], buffer, bytes_read);
      if (written != bytes_read)
      {
        break; // Error or pipe closed
      }
    }
    else if (bytes_read == 0)
    {
      // No more data, close write end to signal EOF
      break;
    }
    else
    {
      // Error occurred
      break;
    }
  }

  // Close write end to signal EOF to sqlite_rsync
  if (g_read_pipe[1] != -1)
  {
    close(g_read_pipe[1]);
    g_read_pipe[1] = -1;
  }

  return NULL;
}

// Thread that reads from write_pipe and writes to WebSocket
static void *websocket_write_thread(void *arg)
{
  uint8_t buffer[4101];

  while (g_threads_running && g_io_ctx && g_io_ctx->write_func)
  {
    int bytes_read = read(g_write_pipe[0], buffer, sizeof(buffer));
    if (bytes_read > 0)
    {
      // Write to WebSocket
      int result = g_io_ctx->write_func(g_io_ctx->user_data, buffer, bytes_read);
      if (result != bytes_read)
      {
        break; // Error occurred
      }
    }
    else if (bytes_read == 0)
    {
      // Pipe closed, no more data
      break;
    }
    else
    {
      // Error occurred
      break;
    }
  }

  return NULL;
}

// Real implementations using the sqlite_rsync library functions
int sqlite_rsync_run_origin(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx)
{
  if (!config || !io_ctx || !config->origin_path)
  {
    return -1;
  }

  // Set the global I/O context for our I/O callback bridge
  g_io_ctx = io_ctx;

  // Initialize WebSocket FILE* bridge
  if (init_websocket_streams() != 0)
  {
    return -1;
  }

  // Create and configure the SQLiteRsync context
  SQLiteRsync ctx;
  memset(&ctx, 0, sizeof(ctx));

#ifdef USE_DEBUG
  ctx.zDebugFile = config->error_file; // Use error file for debug output
#endif

  ctx.zOrigin = config->origin_path;
  ctx.pIn = g_websocket_in;   // Use our WebSocket input stream
  ctx.pOut = g_websocket_out; // Use our WebSocket output stream
  ctx.isRemote = 1;
  ctx.iProtocol = config->protocol_version;
  ctx.eVerbose = config->verbose_level;
  if (config->dry_run)
    ctx.bWalOnly = 1;

  // Call the actual sqlite_rsync origin-side implementation
  originSide(&ctx);

  // Clean up WebSocket streams
  cleanup_websocket_streams();

  return ctx.nErr == 0 ? 0 : -1;
}

int sqlite_rsync_run_replica(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx)
{
  if (!config || !io_ctx)
  {
    return -1;
  }

  // Set the global I/O context for our I/O callback bridge
  g_io_ctx = io_ctx;

  // Initialize WebSocket FILE* bridge
  if (init_websocket_streams() != 0)
  {
    return -1;
  }

  // Create and configure the SQLiteRsync context
  SQLiteRsync ctx;
  memset(&ctx, 0, sizeof(ctx));

#ifdef USE_DEBUG
  ctx.zDebugFile = config->error_file; // Use error file for debug output
#endif

  ctx.zReplica = config->replica_path;
  ctx.pIn = g_websocket_in;   // Use our WebSocket input stream
  ctx.pOut = g_websocket_out; // Use our WebSocket output stream
  ctx.isRemote = 1;
  ctx.iProtocol = config->protocol_version;
  ctx.eVerbose = config->verbose_level;
  if (config->dry_run)
    ctx.bWalOnly = 1;

  // Call the actual sqlite_rsync replica-side implementation
  replicaSide(&ctx);

  // Clean up WebSocket streams
  cleanup_websocket_streams();

  return ctx.nErr == 0 ? 0 : -1;
}

// Run direct local sync between two SQLite files without WebSocket I/O
int sqlite_rsync_run_direct(sqlite_rsync_config_t *config)
{
  if (!config || !config->origin_path || !config->replica_path)
  {
    return -1;
  }

  // Create pipes for inter-process communication
  int pipeOriginToReplica[2], pipeReplicaToOrigin[2];

  if (pipe(pipeOriginToReplica) == -1 || pipe(pipeReplicaToOrigin) == -1)
  {
    return -1;
  }

  pid_t childPid = fork();
  if (childPid == -1)
  {
    // Fork failed
    close(pipeOriginToReplica[0]);
    close(pipeOriginToReplica[1]);
    close(pipeReplicaToOrigin[0]);
    close(pipeReplicaToOrigin[1]);
    return -1;
  }
  else if (childPid == 0)
  {
    // Child process - run as replica
    close(pipeOriginToReplica[1]); // Close write end
    close(pipeReplicaToOrigin[0]); // Close read end

    // Create and configure the SQLiteRsync context for replica
    SQLiteRsync ctx;
    memset(&ctx, 0, sizeof(ctx));

#ifdef USE_DEBUG
    ctx.zDebugFile = config->error_file;
#endif

    ctx.zReplica = config->replica_path;
    ctx.pIn = fdopen(pipeOriginToReplica[0], "rb");  // Read from origin
    ctx.pOut = fdopen(pipeReplicaToOrigin[1], "wb"); // Write to origin
    ctx.isRemote = 0;                                // Local operation
    ctx.isReplica = 1;                               // This is the replica side
    ctx.iProtocol = config->protocol_version;
    ctx.eVerbose = config->verbose_level;
    if (config->dry_run)
      ctx.bWalOnly = 1;

    // Call the replica-side implementation
    replicaSide(&ctx);

    // Clean up and exit child process
    if (ctx.pIn)
      fclose(ctx.pIn);
    if (ctx.pOut)
      fclose(ctx.pOut);
    exit(ctx.nErr == 0 ? 0 : 1);
  }
  else
  {
    // Parent process - run as origin
    close(pipeOriginToReplica[0]); // Close read end
    close(pipeReplicaToOrigin[1]); // Close write end

    // Create and configure the SQLiteRsync context for origin
    SQLiteRsync ctx;
    memset(&ctx, 0, sizeof(ctx));

#ifdef USE_DEBUG
    ctx.zDebugFile = config->error_file;
#endif

    ctx.zOrigin = config->origin_path;
    ctx.pIn = fdopen(pipeReplicaToOrigin[0], "rb");  // Read from replica
    ctx.pOut = fdopen(pipeOriginToReplica[1], "wb"); // Write to replica
    ctx.isRemote = 0;                                // Local operation
    ctx.isReplica = 0;                               // This is the origin side
    ctx.iProtocol = config->protocol_version;
    ctx.eVerbose = config->verbose_level;
    if (config->dry_run)
      ctx.bWalOnly = 1;

    // Call the origin-side implementation
    originSide(&ctx);

    // Clean up parent resources
    if (ctx.pIn)
      fclose(ctx.pIn);
    if (ctx.pOut)
      fclose(ctx.pOut);

    // Wait for child process to complete
    int status;
    waitpid(childPid, &status, 0);

    // Return success if both parent and child succeeded
    return (ctx.nErr == 0 && WEXITSTATUS(status) == 0) ? 0 : -1;
  }
}

#endif