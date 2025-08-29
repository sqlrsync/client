# usage: make sqlite_rsync_combined.o

# Compiler settings
CC = gcc
CFLAGS = -c -fPIC -O2 -g -Wall -Wextra
INCLUDES = -I. -I../sqlite/install/include
DEFINES = -DSQLITE_ENABLE_DBPAGE_VTAB=1 -DSQLITE_RSYNC_NO_MAIN -DSQLITE_RSYNC_USE_H

# SQLite library location (adjust path as needed)
SQLITE_LIBDIR = ../sqlite/install/lib
SQLITE_LIBS = -L$(SQLITE_LIBDIR) -lsqlite3

# Additional libraries needed
LIBS = $(SQLITE_LIBS) -lm -lz -lpthread

# Source files
SQLITE_RSYNC_SRC = sqlite_rsync.c
WRAPPER_SRC = sqlite_rsync_wrapper.c

# Object files
SQLITE_RSYNC_OBJ = sqlite_rsync.o
WRAPPER_OBJ = sqlite_rsync_wrapper.o
COMBINED_OBJ = sqlite_rsync_combined.o

# Static library
OBJECTS = $(SQLITE_RSYNC_OBJ) $(WRAPPER_OBJ)
STATIC_LIB = libsqlite_rsync.a

# Default target
all: $(STATIC_LIB)

$(STATIC_LIB): $(OBJECTS)
	@echo "Creating static library libsqlite_rsync.a..."
	ar rcs $(STATIC_LIB) $(OBJECTS)
	@echo "✓ Static library created: $(STATIC_LIB)"


# Build individual object files
$(SQLITE_RSYNC_OBJ): $(SQLITE_RSYNC_SRC) sqlite_rsync.h
	$(CC) $(CFLAGS) $(INCLUDES) $(DEFINES) $(SQLITE_RSYNC_SRC) -o $(SQLITE_RSYNC_OBJ)

$(WRAPPER_OBJ): $(WRAPPER_SRC) sqlite_rsync_wrapper.h sqlite_rsync.h
	$(CC) $(CFLAGS) $(INCLUDES) $(DEFINES) $(WRAPPER_SRC) -o $(WRAPPER_OBJ)

# Combine object files into a single relocatable object
$(COMBINED_OBJ): $(SQLITE_RSYNC_OBJ) $(WRAPPER_OBJ)
	ld -r $(SQLITE_RSYNC_OBJ) $(WRAPPER_OBJ) -o $(COMBINED_OBJ)

# Clean up
clean:
	rm -f *.o *.a

# Test the object file by creating a shared library
test-shared: $(COMBINED_OBJ)
	$(CC) -shared $(COMBINED_OBJ) $(LIBS) -o libsqlite_rsync.so

# Display object file information
info: $(COMBINED_OBJ)
	@echo "=== Object file size ==="
	ls -lh $(COMBINED_OBJ)
	@echo "=== Exported symbols ==="
	nm $(COMBINED_OBJ) | grep " T " | head -10
	@echo "=== External dependencies ==="
	nm -u $(COMBINED_OBJ) | head -10

.PHONY: all clean test-shared info
