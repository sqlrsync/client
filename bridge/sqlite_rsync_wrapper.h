#define SQLITE_RSYNC_USE_H

#ifndef SQLITE_RSYNC_WRAPPER_H_INCLUDED
#define SQLITE_RSYNC_WRAPPER_H_INCLUDED

#ifdef __cplusplus
extern "C"
{
#endif

#include <stdint.h>

// Protocol constants
#define PROTOCOL_VERSION 2

// Origin Messages (from client to server)
#define ORIGIN_BEGIN 0x41
#define ORIGIN_END 0x42
#define ORIGIN_ERROR 0x43
#define ORIGIN_PAGE 0x44
#define ORIGIN_TXN 0x45
#define ORIGIN_MSG 0x46
#define ORIGIN_DETAIL 0x47
#define ORIGIN_READY 0x48

// Replica Messages (from server to client)
#define REPLICA_BEGIN 0x61
#define REPLICA_ERROR 0x62
#define REPLICA_END 0x63
#define REPLICA_HASH 0x64
#define REPLICA_READY 0x65
#define REPLICA_MSG 0x66
#define REPLICA_CONFIG 0x67

  // Configuration structure for the rsync operation
  typedef struct
  {
    const char *origin_path;
    const char *replica_path;
    int protocol_version;
    int verbose_level;
    int dry_run;
    int wal_only;
    const char *error_file;
  } sqlite_rsync_config_t;

  // Callback function types for I/O operations
  typedef int (*read_callback_t)(void *user_data, uint8_t *buffer, int size);
  typedef int (*write_callback_t)(void *user_data, uint8_t *buffer, int size);

  // Context for WebSocket-based I/O
  typedef struct
  {
    void *user_data;
    read_callback_t read_func;
    write_callback_t write_func;
  } sqlite_rsync_io_context_t;

  // Initialize SQLite rsync with WebSocket I/O
  int sqlite_rsync_init_websocket(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx);

  // Run as origin (sender)
  int sqlite_rsync_run_origin(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx);

  // Run as replica (receiver)
  int sqlite_rsync_run_replica(sqlite_rsync_config_t *config, sqlite_rsync_io_context_t *io_ctx);

  // Run direct local sync (origin and replica are both local files)
  int sqlite_rsync_run_direct(sqlite_rsync_config_t *config);

  // Get database info
  typedef struct
  {
    int page_size;
    int page_count;
    char journal_mode[16];
  } sqlite_db_info_t;

  int sqlite_rsync_get_db_info(const char *db_path, sqlite_db_info_t *info);

  // Cleanup resources
  void sqlite_rsync_cleanup(void);

#ifdef __cplusplus
}
#endif

#endif // SQLITE_RSYNC_WRAPPER_H
