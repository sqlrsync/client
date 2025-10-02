//go:build cgo

package bridge

/*
#cgo CFLAGS: -I. -I../sqlite/install/include -DSQLITE_ENABLE_DBPAGE_VTAB=1
#cgo LDFLAGS: -L. -L../sqlite/install/lib -lsqlite_rsync -lsqlite3 -lm -lz -lpthread

#define SQLITE_RSYNC_NO_MAIN
#define SQLITE_RSYNC_USE_H

#include "sqlite3.h"
#include "sqlite_rsync.h"
#include "sqlite_rsync_wrapper.h"

#include <stdlib.h>
#include <string.h>

// Forward declarations for Go callbacks
extern int go_local_read_callback(void* user_data, uint8_t* buffer, int size);
extern int go_local_write_callback(void* user_data, uint8_t* buffer, int size);
*/
import "C"
import (
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"

	"go.uber.org/zap"
)

var (
	handleCounter int64
	handleMap     = make(map[int64]cgo.Handle)
	handleMutex   sync.RWMutex
)

const DEBUG_FILE = "sqlrsync-debug"

// GetDatabaseInfo wraps the C function to get database info
func GetDatabaseInfo(dbPath string) (*DatabaseInfo, error) {
	return cgoGetDatabaseInfo(dbPath)
}

// cgoGetDatabaseInfo wraps the C function to get database info
func cgoGetDatabaseInfo(dbPath string) (*DatabaseInfo, error) {
	cDbPath := C.CString(dbPath)
	defer C.free(unsafe.Pointer(cDbPath))

	var cInfo C.sqlite_db_info_t
	result := C.sqlite_rsync_get_db_info(cDbPath, &cInfo)
	if result != 0 {
		return nil, &SQLiteRsyncError{
			Code:    int(result),
			Message: "failed to get database info",
		}
	}

	info := &DatabaseInfo{
		PageSize:    int(cInfo.page_size),
		PageCount:   int(cInfo.page_count),
		JournalMode: C.GoString(&cInfo.journal_mode[0]),
	}

	return info, nil
}

// RunOriginSync wraps the C function to run origin synchronization
func RunOriginSync(dbPath string, dryRun bool, client *BridgeClient) error {
	return cgoRunOriginSync(dbPath, dryRun, client)
}

// cgoRunOriginSync wraps the C function to run origin synchronization
func cgoRunOriginSync(dbPath string, dryRun bool, client *BridgeClient) error {
	// Prepare C configuration
	cDbPath := C.CString(dbPath)
	defer C.free(unsafe.Pointer(cDbPath))

	config := C.sqlite_rsync_config_t{
		origin_path:      cDbPath,
		replica_path:     nil,
		protocol_version: C.PROTOCOL_VERSION,
		verbose_level:    0,
		dry_run:          0,
		wal_only:         0,
		error_file:       nil,
	}
	// Set debugFile if debugFile is defined (non-empty)
	if client.Config.EnableSQLiteRsyncLogging {
		cDebugFile := C.CString(DEBUG_FILE + "-push.log")
		defer C.free(unsafe.Pointer(cDebugFile))
		config.error_file = cDebugFile
	}

	if dryRun {
		config.dry_run = 1
	}

	// Create cgo handle for the client and store it in map
	handle := cgo.NewHandle(client)
	handleID := atomic.AddInt64(&handleCounter, 1)

	handleMutex.Lock()
	handleMap[handleID] = handle
	handleMutex.Unlock()

	defer func() {
		handleMutex.Lock()
		delete(handleMap, handleID)
		handleMutex.Unlock()
		handle.Delete()
	}()

	// Setup I/O context with Go callbacks
	// Allocate C memory to store the handleID safely
	cHandleID := C.malloc(C.sizeof_long)
	defer C.free(cHandleID)
	*(*C.long)(cHandleID) = C.long(handleID)

	ioCtx := C.sqlite_rsync_io_context_t{
		user_data:  cHandleID,
		read_func:  C.read_callback_t(C.go_local_read_callback),
		write_func: C.write_callback_t(C.go_local_write_callback),
	}

	// Run the origin synchronization
	result := C.sqlite_rsync_run_origin(&config, &ioCtx)
	if result != 0 {
		return &SQLiteRsyncError{
			Code:    int(result),
			Message: "origin sync failed",
		}
	}

	return nil
}

// Cleanup wraps the C cleanup function
func Cleanup() {
	cgoCleanup()
}

// cgoCleanup wraps the C cleanup function
func cgoCleanup() {
	C.sqlite_rsync_cleanup()
}

//export go_local_read_callback
func go_local_read_callback(userData unsafe.Pointer, buffer *C.uint8_t, size C.int) C.int {

	handleID := int64(*(*C.long)(userData))

	handleMutex.RLock()
	handle, ok := handleMap[handleID]
	handleMutex.RUnlock()

	if !ok {
		return -1
	}

	client := handle.Value().(*BridgeClient)

	if client.ReadFunc == nil {
		client.Logger.Error("Read function not set")
		return -1
	}

	// Convert C buffer to Go slice
	goBuffer := (*[1 << 30]byte)(unsafe.Pointer(buffer))[:size:size]

	bytesRead, err := client.ReadFunc(goBuffer)
	if err != nil {
		if err.Error() != "connection lost" && err.Error() != "sync completed" {
			client.Logger.Error("Connection to server had a failure.  Are you online?  Read callback error", zap.Error(err))
		}
		// For sync completion errors, return 0 to signal EOF gracefully
		// This allows sqlite_rsync to finish processing any buffered data
		return 0
	}

	client.Logger.Debug("Read callback", zap.Int("bytesRead", bytesRead))
	return C.int(bytesRead)
}

//export go_local_write_callback
func go_local_write_callback(userData unsafe.Pointer, buffer *C.uint8_t, size C.int) C.int {
	handleID := int64(*(*C.long)(userData))

	handleMutex.RLock()
	handle, ok := handleMap[handleID]
	handleMutex.RUnlock()

	if !ok {
		return -1
	}

	client := handle.Value().(*BridgeClient)

	if client.WriteFunc == nil {
		client.Logger.Error("Write function not set")
		return -1
	}

	// Convert C buffer to Go slice (safe to cast away const since we're reading)
	data := C.GoBytes(unsafe.Pointer(buffer), size)

	err := client.WriteFunc(data)
	if err != nil {
		client.Logger.Error("Write callback error", zap.Error(err))
		return -1
	}

	client.Logger.Debug("Write callback", zap.Int("bytesWritten", int(size)))
	return size
}

// RunReplicaSync wraps the C function to run replica synchronization
func RunReplicaSync(originDbPath, replicaDbPath string, client *BridgeClient) error {
	return cgoRunReplicaSync(originDbPath, replicaDbPath, client)
}

// RunDirectSync wraps the C function to run direct local synchronization
func RunDirectSync(originDbPath, replicaDbPath string, dryRun bool, verbose int) error {
	return cgoRunDirectSync(originDbPath, replicaDbPath, dryRun, verbose)
}

// cgoRunReplicaSync wraps the C function to run replica synchronization
func cgoRunReplicaSync(originDbPath, replicaDbPath string, client *BridgeClient) error {
	// Prepare C configuration
	cOriginPath := C.CString(originDbPath)
	defer C.free(unsafe.Pointer(cOriginPath))

	cReplicaPath := C.CString(replicaDbPath)
	defer C.free(unsafe.Pointer(cReplicaPath))

	config := C.sqlite_rsync_config_t{
		origin_path:      cOriginPath,
		replica_path:     cReplicaPath,
		protocol_version: C.PROTOCOL_VERSION,
		verbose_level:    0,
		dry_run:          0,
		wal_only:         0,
		error_file:       nil,
	}

	// Set debugFile if debugFile is defined
	if client.Config.EnableSQLiteRsyncLogging {
		cDebugFile := C.CString(DEBUG_FILE + "-push.log")
		defer C.free(unsafe.Pointer(cDebugFile))
		config.error_file = cDebugFile
	}

	// Create cgo handle for the client
	handle := cgo.NewHandle(client)
	handleID := atomic.AddInt64(&handleCounter, 1)

	handleMutex.Lock()
	handleMap[handleID] = handle
	handleMutex.Unlock()

	defer func() {
		handleMutex.Lock()
		delete(handleMap, handleID)
		handleMutex.Unlock()
		handle.Delete()
	}()

	// Setup I/O context with Go callbacks
	cHandleID := C.malloc(C.sizeof_long)
	defer C.free(cHandleID)
	*(*C.long)(cHandleID) = C.long(handleID)

	ioCtx := C.sqlite_rsync_io_context_t{
		user_data:  cHandleID,
		read_func:  C.read_callback_t(C.go_local_read_callback),
		write_func: C.write_callback_t(C.go_local_write_callback),
	}

	// Run the replica synchronization
	result := C.sqlite_rsync_run_replica(&config, &ioCtx)
	if result != 0 {
		return &SQLiteRsyncError{
			Code:    int(result),
			Message: "replica sync failed",
		}
	}

	return nil
}

// cgoRunDirectSync wraps the C function to run direct local synchronization
func cgoRunDirectSync(originDbPath, replicaDbPath string, dryRun bool, verbose int) error {
	// Prepare C configuration
	cOriginPath := C.CString(originDbPath)
	defer C.free(unsafe.Pointer(cOriginPath))

	cReplicaPath := C.CString(replicaDbPath)
	defer C.free(unsafe.Pointer(cReplicaPath))

	config := C.sqlite_rsync_config_t{
		origin_path:      cOriginPath,
		replica_path:     cReplicaPath,
		protocol_version: C.PROTOCOL_VERSION,
		verbose_level:    C.int(verbose),
		dry_run:          0,
		wal_only:         0,
		error_file:       nil,
	}

	// Set debugFile if debugFile is defined
	if verbose > 0 {
		cDebugFile := C.CString(DEBUG_FILE + "-push.log")
		defer C.free(unsafe.Pointer(cDebugFile))
		config.error_file = cDebugFile
	}

	if dryRun {
		config.dry_run = 1
	}

	// Run direct sync without WebSocket I/O - use the original C implementation
	result := C.sqlite_rsync_run_direct(&config)
	if result != 0 {
		return &SQLiteRsyncError{
			Code:    int(result),
			Message: "direct sync failed",
		}
	}

	return nil
}
