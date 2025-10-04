#!/bin/bash

# USGS Earthquake Data Synchronization Script
# Downloads earthquake data every 50 minutes and syncs to SQLRsync

# Configuration
FILE=earthquakes.db
TABLE=earthquakes
UPDATES=50m
URL=https://earthquake.usgs.gov/earthquakes/feed/v1.0/summary/4.5_month.csv
SQLRSYNC_PATH=usgs.gov/earthquakes.db
PRIMARY_KEY=id
MODE="INSERT OR REPLACE INTO"
SCHEMA="time TEXT, latitude REAL, longitude REAL, depth REAL, mag REAL, magType TEXT, nst INTEGER, gap REAL, dmin REAL, rms REAL, net TEXT, id TEXT PRIMARY KEY, updated TEXT, place TEXT, type TEXT, horizontalError REAL, depthError REAL, magError REAL, magNst INTEGER, status TEXT, locationSource TEXT, magSource TEXT"

# Convert time interval to seconds for sleep
convert_to_seconds() {
    local time_str="$1"
    local num="${time_str%[a-zA-Z]*}"
    local unit="${time_str#$num}"
    
    case "$unit" in
        s|sec) echo "$num" ;;
        m|min) echo $((num * 60)) ;;
        h|hour) echo $((num * 3600)) ;;
        d|day) echo $((num * 86400)) ;;
        *) echo 3000 ;; # default to 50 minutes
    esac
}

# Initialize database and table
init_database() {
    echo "Initializing database: $FILE"
    sqlite3 "$FILE" "CREATE TABLE IF NOT EXISTS $TABLE ($SCHEMA);"
    if [ $? -eq 0 ]; then
        echo "Database initialized successfully"
    else
        echo "Error: Failed to initialize database"
        exit 1
    fi
}

# Download and import data with in-memory staging
sync_data() {
    echo "$(date): Downloading earthquake data from USGS..."
    
    # Download CSV data
    local temp_file=$(mktemp)
    if curl -s -f "$URL" -o "$temp_file"; then
        echo "Data downloaded successfully"
        
        # Get record count before import
        local count_before=$(sqlite3 "$FILE" "SELECT COUNT(*) FROM $TABLE;" 2>/dev/null || echo "0")
        
        # Use in-memory database for staging to avoid bloating main database
        sqlite3 "$FILE" <<EOF
-- Attach in-memory database for staging
ATTACH DATABASE ':memory:' AS staging;

-- Create staging table in memory with same schema
CREATE TABLE staging.$TABLE ($SCHEMA);

-- Import CSV data into in-memory staging table
.mode csv
.import $temp_file staging.$TABLE

-- Remove header row if it was imported (check if id column contains 'id')
DELETE FROM staging.$TABLE WHERE id = 'id';

-- Use INSERT OR REPLACE to merge data from in-memory staging to main table
$MODE main.$TABLE 
SELECT * FROM staging.$TABLE;

-- Detach in-memory database (automatically cleans up)
DETACH DATABASE staging;
EOF
        
        if [ $? -eq 0 ]; then
            local count_after=$(sqlite3 "$FILE" "SELECT COUNT(*) FROM $TABLE;")
            local new_records=$((count_after - count_before))
            echo "Data imported successfully. Total records: $count_after (added/updated: $new_records)"
            
            # Show some recent earthquake info
            echo "Recent earthquakes:"
            sqlite3 "$FILE" "SELECT time, mag, place FROM $TABLE ORDER BY time DESC LIMIT 3;" 2>/dev/null | head -3
            
            # Sync to SQLRsync server if path is configured
            if [ -n "$SQLRSYNC_PATH" ] && command -v sqlrsync >/dev/null 2>&1; then
                echo "Syncing to SQLRsync server: $SQLRSYNC_PATH"
                sqlrsync "$FILE" "$SQLRSYNC_PATH"
                if [ $? -eq 0 ]; then
                    echo "Successfully synced to server"
                else
                    echo "Warning: Failed to sync to server"
                fi
            fi
        else
            echo "Error: Failed to import data"
        fi
    else
        echo "Error: Failed to download data from $URL"
    fi
    
    rm -f "$temp_file"
}

# Main execution
main() {
    echo "Fetch CSV to SQLite Data Sync Starting..."
    echo "Configuration:"
    echo "  Database: $FILE"
    echo "  Table: $TABLE" 
    echo "  Update interval: $UPDATES"
    echo "  Data source: $URL"
    echo "  SQLRsync path: $SQLRSYNC_PATH"
    echo ""
    
    # Initialize database
    init_database
    
    # Convert update interval to seconds
    local sleep_seconds=$(convert_to_seconds "$UPDATES")
    echo "Update interval: $sleep_seconds seconds"
    echo ""
    
    # Initial sync
    sync_data
    
    # Continuous sync loop
    echo "Starting continuous sync (Ctrl+C to stop)..."
    while true; do
        echo "Sleeping for $UPDATES ($sleep_seconds seconds)..."
        sleep "$sleep_seconds"
        sync_data
    done
}

# Run main function if script is executed directly
if [ "${BASH_SOURCE[0]}" == "${0}" ]; then
    main "$@"
fi

