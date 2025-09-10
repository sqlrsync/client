package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sqlrsync/sqlrsync.com/bridge"
	"github.com/sqlrsync/sqlrsync.com/remote"
)

var (
	serverURL       string
	verbose         bool
	dryRun          bool
	setPublic       bool
	timeout         int
	logger          *zap.Logger
	inspectTraffic  bool
	inspectionDepth int
	newReadToken    bool
	pullKey         string
	pushKey         string
	replicaID       string
)

var rootCmd = &cobra.Command{
	Use:   "sqlrsync [ORIGIN] [REPLICA] or [LOCAL] or [REMOTE]",
	Short: "SQLite Rsync - ",
	Long: `A rsync-like utility built specifically to replicate SQLite databases
to sqlrsync.com for features such as backup, version control, and distribution.

Using the page hashing algorithm designed by the authors of SQLite, only
changed pages are communicated between ORIGIN and REPLICA, allowing for 
efficient synchronization.

REPLICA becomes a copy of a snapshot of ORIGIN as it existed when the sqlrsync
command started. If other processes change the content of ORIGIN while this
command is running, those changes will be applied to ORIGIN, but they are not
transferred to REPLICA. Thus, REPLICA ends up as a fully-consistent snapshot
of ORIGIN at an instant in time. 

Learn about SQLite Pages: sqlite.org/fileformat2.html
Learn about sqlite3_rsync: sqlite.org/rsync.html

This utility, a wrapper around sqlite3_rsync, uses sqlrsync.com as the REMOTE
server to allow specific benefits over simply using the utility the developers
of the SQLite project provide.

ORIGIN and REPLICA can be LOCAL or REMOTE.  Both cannot be REMOTE.

LOCAL is this local machine in the current working directory (or prefixed with 
./, ../, or /).
REMOTE is a database hosted on sqlrsync.com, and must have at least one / in its
path.

If REPLICA does not already exist, it is created.

Local databases may be "live" while this utility is running. Other programs can have
active connections to the local database (in either role) without any disruption.
Other programs can write to/read from ORIGIN, and can read from REPLICA while this
utility runs.

All of the table (and index) content will be byte-for-byte identical in the
replica. However, there can be some minor changes in the database header.  See
Limitations at sqlite.org/rsync.html

A REMOTE ORIGIN database may be specified with an appended @<VERSION>, such as:
  mynamespace/mydb.sqlite             # Requests the latest uploaded version
	mynamespace/mydb.sqlite@<VERSION>   # VERSION is a number greater than 0 and
	   identifies the nth version uploaded
	mynamespace/mydb.sqlite@latest.     # Redundant to leaving the value unspecified
	mynamespace/mydb.sqlite@latest-<N> # N is a number greater than 0 and
	   will cause the version N uploads prior to the latest version to be used.

When ORIGIN is LOCAL and REPLICA is LOCAL, a local transfer (no network) causes
REPLICA to become a copy of ORIGIN.

When ORIGIN is LOCAL and REPLICA is REMOTE, a secure websocket connects to
sqlrsync.com and then any pages REPLICA needs synchronized are transferred to
the remote database.

When ORIGIN is LOCAL and REPLICA is unspecified, the remote REPLICA is created
at sqlrsync.com using the default namespace and database name derived from ORIGIN.

When ORIGIN is REMOTE and REPLICA is LOCAL, the local REPLICA becomes a complete
copy of ORIGIN.  

When ORIGIN is REMOTE and REPLICA is unspecified, a local REPLICA is created
at using the database name derived from ORIGIN.	

Usage modes:
1. Direct local sync:      sqlrsync LOCAL LOCAL [OPTIONS]
	 Example: sqlrsync mydb.sqlite ./mydb2.sqlite

2. Push to sqlrsync.com:   sqlrsync LOCAL [REMOTE] [OPTIONS]
	 Example: sqlrsync mydb.sqlite mynamespace/mydb.sqlite

3. Pull from sqlrsync.com: sqlrsync REMOTE [LOCAL] [OPTIONS]
          or sqlrsync REMOTE@<VERSION> [LOCAL] [OPTIONS]
	 Example: sqlrsync mynamespace/mydb.sqlite
	 Example: sqlrsync mynamespace/mydb.sqlite@latest-1 /overhere/mydb.sqlite
	 Example: sqlrsync mynamespace/mydb.sqlite@7
	 
Eternal gratitude to the authors of the SQLite Project for their contributions
to the world of data storage.
`,

	Version: "1.0.0",
	PreRun: func(cmd *cobra.Command, args []string) {
		setupLogger()
	},
	RunE:          runSync,
	SilenceErrors: true,
	SilenceUsage:  true,
}

func showLocalError(message string) {
	fmt.Println(color.RedString("[error]"), message)
}

func runSync(cmd *cobra.Command, args []string) error {
	// Determine the sync mode based on arguments

	// The two arguments are ORIGIN REPLICA.
	// Either can be LOCAL or REMOTE.
	// LOCAL is determined if the path begins with /, ./, or ../ OR doesn't have a / anywhere in it
	// REMOTE is determined by !LOCAL
	// DBNAME is string after the final / of the REMOTE path
	//
	// Examples:
	// IF ORIGIN:LOCAL REPLICA:LOCAL
	//	runDirectSync(ORIGIN,REPLICA);
	// IF ORIGIN:LOCAL REPLICA:REMOTE
	//	runPushSync(ORIGIN,REPLICA);
	// IF ORIGIN:REMOTE REPLICA:LOCAL
	//	runPullSync(ORIGIN,REPLICA);
	// IF ORIGIN:LOCAL (no REPLICA)
	//	runPushSync(ORIGIN, PREFIX/<DBNAME>)
	// IF (no ORIGIN) REPLICA:REMOTE
	//	runPullSync(REPLICA, <DBNAME>)
	// IF (no ORIGIN) REPLICA:LOCAL
	//	This cannot happen

	isLocal := func(path string) bool {
		return strings.HasPrefix(path, "/") || strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "~/") || !strings.Contains(path, "/")
	}

	if len(args) == 0 {
		return cmd.Help()
	} else if len(args) == 2 {
		// Two arguments: ORIGIN REPLICA
		origin, replica := args[0], args[1]
		originLocal := isLocal(origin)
		replicaLocal := isLocal(replica)

		if originLocal && replicaLocal {
			// IF ORIGIN:LOCAL REPLICA:LOCAL
			return runDirectSync(origin, replica)
		} else if originLocal && !replicaLocal {
			// IF ORIGIN:LOCAL REPLICA:REMOTE
			return runPushSync(origin, replica)
		} else if !originLocal && replicaLocal {
			// IF ORIGIN:REMOTE REPLICA:LOCAL
			return runPullSync(origin, replica)
		} else {
			return fmt.Errorf("remote to remote sync not supported")
		}
	} else if len(args) == 1 {
		// One argument: either ORIGIN (push/pull depends on ~.config & -sqlrsync) or REPLICA (for pull)
		path := args[0]
		if isLocal(path) {
			// IF ORIGIN:LOCAL (no REPLICA) - varies
			localSecretsConfig, err := LoadLocalSecretsConfig()
			if err != nil {
				return fmt.Errorf("failed to load local secrets config: %w", err)
			}
			// Get absolute path for the local database
			absPath, err := filepath.Abs(path)
			if err == nil {
				// If we have a push key for this database, use it to push
				pushedDBInfo := localSecretsConfig.FindDatabaseByPath(absPath)
				if pushedDBInfo != nil && pushedDBInfo.PushKey != "" && pushedDBInfo.Server == serverURL {
					pushKey = pushedDBInfo.PushKey
					return runPushSync(absPath, pushedDBInfo.RemotePath)
				}
			}

			// else if there is a -sqlrsync file, do a pull instead
			dashSQLRsync := NewDashSQLRsync(path)
			if dashSQLRsync.Exists() {
				if err := dashSQLRsync.Read(); err != nil {
					return fmt.Errorf("failed to read -sqlrsync file: %w", err)
				}
				if dashSQLRsync.RemotePath == "" {
					return fmt.Errorf("invalid -sqlrsync file: missing remote path")
				}
				if dashSQLRsync.Server == serverURL {
					localPath := ""
					version := "latest"
					localPath, version, _ = strings.Cut(path, "@")

					pullKey = dashSQLRsync.PullKey
					replicaID = dashSQLRsync.ReplicaID
					serverURL = dashSQLRsync.Server
					return runPullSync(dashSQLRsync.RemotePath+"@"+version, localPath)
				}
			}

			// else push this file up
			return runPushSync(path, "")
		} else {
			// IF REPLICA:REMOTE (no ORIGIN) - pull to default local name
			dbname := filepath.Base(path)
			return runPullSync(path, dbname)
		}

	} else {
		return fmt.Errorf("invalid arguments. Usage:\n1. Direct local sync:      sqlrsync ORIGIN REPLICA [OPTIONS]\n2. Push to sqlrsync.com:   sqlrsync ORIGIN [REPLICA] [OPTIONS]\n3. Pull from sqlrsync.com: sqlrsync REPLICA [OPTIONS] or sqlrsync REPLICA@<VERSION> [OPTIONS]")
	}
}

func runDirectSync(originPath, replicaPath string) error {
	// Validate that origin database file exists
	if _, err := os.Stat(originPath); os.IsNotExist(err) {
		return fmt.Errorf("origin database file does not exist: %s", originPath)
	}

	logger.Info("Starting direct SQLite Rsync synchronization",
		zap.String("origin", originPath),
		zap.String("replica", replicaPath),
		zap.Bool("dryRun", dryRun))

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.Config{
		DatabasePath: originPath,
		DryRun:       dryRun,
		Logger:       logger.Named("local"),
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	// Get database info
	dbInfo, err := localClient.GetDatabaseInfo()
	if err != nil {
		return fmt.Errorf("failed to get database info: %w", err)
	}

	logger.Info("Database information",
		zap.Int("pageSize", dbInfo.PageSize),
		zap.Int("pageCount", dbInfo.PageCount),
		zap.String("journalMode", dbInfo.JournalMode))

	// Perform direct sync
	if err := localClient.RunDirectSync(replicaPath); err != nil {
		return fmt.Errorf("direct synchronization failed: %w", err)
	}

	logger.Info("Direct synchronization completed successfully")
	fmt.Println("✅ Locally replicated", originPath, "to", replicaPath+".")
	return nil
}

func runPushSync(localPath string, remotePath string) error {
	logger.Info("Running a PUSH sync",
		zap.String("local", localPath),
		zap.String("remote", remotePath))
	// Validate that database file exists
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		return fmt.Errorf("database file does not exist: %s", localPath)
	}

	// Load local secrets config
	localSecretsConfig, err := LoadLocalSecretsConfig()
	if err != nil {
		return fmt.Errorf("failed to load local secrets config: %w", err)
	}

	// Get absolute path for the local database
	absLocalPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Find or create database entry
	dbConfig := localSecretsConfig.FindDatabaseByPath(absLocalPath)
	if dbConfig == nil {
		// Create new database entry
		dbConfig = &SQLRsyncDatabase{
			LocalPath: absLocalPath,
			Server:    serverURL,
		}
	} else {
		if serverURL == "" {
			serverURL = dbConfig.Server
		}
		if pushKey == "" {
			pushKey = dbConfig.PushKey
		}
		if remotePath == "" {
			remotePath = dbConfig.RemotePath
		}
	}

	if remotePath == "" {
		// Check for -sqlrsync file
		dashSQLRsync := NewDashSQLRsync(absLocalPath)
		if !dashSQLRsync.Exists() {
			fmt.Println("No -sqlrsync file found.  This database hasn't been pushed to SQLRsync Server before.")
			fmt.Println("No REMOTE name provided.  Will use Account Admin Key's default Replica name.")
		} else {
			logger.Info("Found -sqlrsync file.")
		}
	}

	// Check if we have a push key for this database
	if os.Getenv("SQLRSYNC_ADMIN_KEY") == "" && pushKey == "" {
		httpServer := strings.Replace(serverURL, "ws", "http", 1)
		fmt.Println("No Key provided.  Creating a new Replica?  Get a key at " + httpServer + "/namespaces")
		fmt.Print("   Enter an Account Admin Key to create a new Replica: ")
		reader := bufio.NewReader(os.Stdin)
		token, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read push key: %w", err)
		}
		token = strings.TrimSpace(token)

		if token == "" {
			return fmt.Errorf("push key cannot be empty")
		}
		pushKey = token
		fmt.Println()
	}

	logger.Info("Starting push synchronization to sqlrsync.com",
		zap.String("local", localPath),
		zap.String("remote", remotePath),
		zap.String("server", serverURL),
		zap.Bool("dryRun", dryRun))

	fmt.Println("PUSHing up to " + serverURL + " ...")

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.Config{
		DatabasePath: localPath,
		DryRun:       dryRun,
		Logger:       logger.Named("local"),
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	localHostname, _ := os.Hostname()

	// Create remote client for WebSocket transport
	remoteClient, err := remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/push/" + remotePath,
		PingPong:                false,
		Timeout:                 timeout,
		AuthToken:               pushKey,
		Logger:                  logger.Named("remote"),
		EnableTrafficInspection: inspectTraffic,
		LocalHostname:           localHostname,
		LocalAbsolutePath:       absLocalPath,
		InspectionDepth:         inspectionDepth,
		SendConfigCmd:           needsToBuildDashSQLRSyncFile(localPath, remotePath),
		SetPublic:               setPublic,
	})

	if err != nil {
		return fmt.Errorf("failed to create remote client: %w", err)
	}
	defer remoteClient.Close()

	// Connect to remote server
	if err := remoteClient.Connect(); err != nil {
		return fmt.Errorf("%w", err)
	}

	// Get database info
	dbInfo, err := localClient.GetDatabaseInfo()
	if err != nil {
		return fmt.Errorf("failed to get database info: %w", err)
	}

	logger.Info("Database information",
		zap.Int("pageSize", dbInfo.PageSize),
		zap.Int("pageCount", dbInfo.PageCount),
		zap.String("journalMode", dbInfo.JournalMode))

	// Perform the sync by bridging local and remote
	if err := performPushSync(localClient, remoteClient); err != nil {
		return fmt.Errorf("push synchronization failed: %w", err)
	}

	logger.Info("Push synchronization completed successfully")

	dbConfig.LastPush = time.Now()
	if remoteClient.GetNewPushKey() != "" {
		fmt.Println("🔑 This database is now PUSH-enabled on this system.")
		fmt.Println("   A new, replica-specific PUSH key has been stored at ~/.config/sqlrsync/local-secrets.toml")
		dbConfig.ReplicaID = remoteClient.GetReplicaID()
		dbConfig.RemotePath = remoteClient.GetReplicaPath()
		dbConfig.PushKey = remoteClient.GetNewPushKey()
	}
	localSecretsConfig.UpdateOrAddDatabase(*dbConfig)
	// Save the updated config
	if err := SaveLocalSecretsConfig(localSecretsConfig); err != nil {
		logger.Warn("Failed to save local secrets config", zap.Error(err))
	}

	if setPublic {
		fmt.Println("🌐 This replica is now publicly accessible.")
		fmt.Println("   Share this database with sqlrsync.com/" + remoteClient.GetReplicaPath())
	}

	if needsToBuildDashSQLRSyncFile(localPath, remotePath) {
		token := remoteClient.GetNewPullKey()
		replicaID := remoteClient.GetReplicaID()
		replicaPath := remoteClient.GetReplicaPath()

		dashSQLRsync := NewDashSQLRsync(localPath)
		if err := dashSQLRsync.Write(replicaPath, localPath, replicaID, token, serverURL); err != nil {
			return fmt.Errorf("failed to create shareable config file: %w", err)
		}
		fmt.Println("🔑 Shareable config file created:", dashSQLRsync.FilePath())
		fmt.Println("   Anyone with this file will be able to PULL any version of this database from sqlrsync.com")
	}

	return nil
}

func isValidVersion(version string) bool {
	// Check if the version is a number
	if num, err := strconv.Atoi(version); err == nil {
		return num > 0
	}

	// Check for "latest" or "latest-<number>"
	if version == "latest" || strings.HasPrefix(version, "latest-") {
		_, after, _ := strings.Cut(version, "-")
		if after != "" {
			if num, err := strconv.Atoi(after); err == nil {
				return num > 0
			}
		} else {
			return true
		}
	}

	return false
}

func needsToBuildDashSQLRSyncFile(filepath string, remotePath string) bool {
	if !newReadToken {
		return false
	}

	dashSQLRsync := NewDashSQLRsync(filepath)
	dashSQLRsync.Read()
	// check if the {path}-sqlrsync file exists
	return !(dashSQLRsync.Exists() && dashSQLRsync.RemotePath == remotePath)
}

func runPullSync(remotePath string, localPath string) error {
	logger.Info("Starting pull synchronization from sqlrsync.com",
		zap.String("remote", remotePath),
		zap.String("local", localPath),
		zap.String("server", serverURL),
		zap.Bool("dryRun", dryRun))

	version := "latest"
	// if remotePath has an @, then we want to pass that version through
	if strings.Contains(remotePath, "@") {
		remotePath, version, _ = strings.Cut(remotePath, "@")
		if version == "" {
			version = "latest"
		}

		// if version is not a number, `latest`, or `latest-<number>` then error
		if !isValidVersion(version) {
			return fmt.Errorf("invalid version format: %s", version)
		}
	}

	fmt.Println("PULLing down from " + serverURL + "/" + remotePath + "@" + version + " ...")

	// Create remote client for WebSocket transport
	remoteClient, err := remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/pull/" + remotePath,
		AuthToken:               pullKey,
		ReplicaID:               replicaID,
		Timeout:                 timeout,
		PingPong:                false,
		Logger:                  logger.Named("remote"),
		EnableTrafficInspection: inspectTraffic,
		InspectionDepth:         inspectionDepth,
		Version:                 version,
		SendConfigCmd:           needsToBuildDashSQLRSyncFile(localPath, remotePath),
	})
	if err != nil {
		return fmt.Errorf("failed to create remote client: %w", err)
	}
	defer remoteClient.Close()

	// Connect to remote server
	if err := remoteClient.Connect(); err != nil {
		return fmt.Errorf("%w", err)
	}

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.Config{
		DatabasePath: localPath,
		DryRun:       dryRun,
		Logger:       logger.Named("local"),
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	// Perform the sync by bridging remote and local (reverse direction for pull)
	if err := performPullSync(localClient, remoteClient); err != nil {
		return fmt.Errorf("pull synchronization failed: %w", err)
	}

	if needsToBuildDashSQLRSyncFile(localPath, remotePath) {
		token := remoteClient.GetNewPullKey()
		dashSQLRsync := NewDashSQLRsync(localPath)
		replicaID := remoteClient.GetReplicaID()
		if err := dashSQLRsync.Write(remotePath, localPath, replicaID, token, serverURL); err != nil {
			return fmt.Errorf("failed to create shareable config file: %w", err)
		}
	}

	logger.Info("Pull synchronization completed successfully")
	return nil
}

func performPushSync(localClient *bridge.Client, remoteClient *remote.Client) error {
	// Create I/O bridge between local and remote clients
	readFunc := func(buffer []byte) (int, error) {
		return remoteClient.Read(buffer)
	}

	writeFunc := func(data []byte) error {
		return remoteClient.Write(data)
	}

	// Run the origin sync through the bridge
	err := localClient.RunPushSync(readFunc, writeFunc)

	// After sync completes, signal remote to close gracefully
	// Give a moment for any final messages to be sent
	time.Sleep(500 * time.Millisecond)

	return err
}

func performPullSync(localClient *bridge.Client, remoteClient *remote.Client) error {
	// Create I/O bridge between remote and local clients (reverse direction for pull)
	readFunc := func(buffer []byte) (int, error) {
		return remoteClient.Read(buffer)
	}

	writeFunc := func(data []byte) error {
		return remoteClient.Write(data)
	}

	// Run the replica sync through the bridge (local acts as replica for pull)
	err := localClient.RunPullSync(readFunc, writeFunc)

	// After sync completes, signal remote to close gracefully
	// Give a moment for any final messages to be sent
	time.Sleep(500 * time.Millisecond)

	return err
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.Flags().StringVar(&pullKey, "pullKey", "", "Authentication key for pull operations")
	rootCmd.Flags().StringVar(&pushKey, "pushKey", "", "Authentication key for push operations")
	rootCmd.Flags().StringVar(&replicaID, "replicaID", "", "Replica ID for the remote database (overwrites the REMOTE path)")
	rootCmd.Flags().StringVarP(&serverURL, "server", "s", "wss://sqlrsync.com", "Server URL for push/pull operations")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
	rootCmd.Flags().BoolVar(&setPublic, "public", false, "Enable public access to the replica (only for push operations)")
	rootCmd.Flags().BoolVar(&newReadToken, "storeNewReadToken", true, "After syncing, the server creates a new read-only token that is stored in the -sqlrsync file adjacent to the local database")
	rootCmd.Flags().BoolVar(&dryRun, "dry", false, "Perform a dry run without making changes")
	rootCmd.Flags().IntVarP(&timeout, "timeout", "t", 8000, "Connection timeout in milliseconds (Max 10 seconds)")
	rootCmd.Flags().BoolVar(&inspectTraffic, "inspect-traffic", false, "Enable traffic inspection between Go and Bridge layers")
	rootCmd.Flags().IntVar(&inspectionDepth, "inspection-depth", 5, "Number of bytes to inspect from each message (default: 5)")
}

func setupLogger() {
	config := zap.NewDevelopmentConfig()

	if verbose {
		config.Level.SetLevel(zapcore.DebugLevel)
	} else {
		config.Level.SetLevel(zapcore.WarnLevel)
	}

	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	config.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder

	var err error
	logger, err = config.Build()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
}

func main() {
	if err := Execute(); err != nil {
		showLocalError(err.Error())
		os.Exit(1)
	}
}
