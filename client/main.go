package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sqlrsync/sqlrsync.com/sync"
)

var VERSION = "0.0.1"
var (
	serverURL   string
	verbose     bool
	dryRun      bool
	SetPublic   bool
	subscribing bool
	pullKey     string
	pushKey     string
	replicaID   string
	logger      *zap.Logger
	showVersion bool
)

var rootCmd = &cobra.Command{
	Use:   "sqlrsync " + VERSION + " [ORIGIN] [REPLICA] or [LOCAL] or [REMOTE]",
	Short: "SQLite Rsync - Simplified Version",
	Long: `A web-enabled rsync-like utility for SQLite databases with subscription support.

Usage modes:
1. Pull from server:       sqlrsync REMOTE [LOCAL] [OPTIONS]
2. Pull with subscription: sqlrsync REMOTE [LOCAL] --subscribe [OPTIONS]
3. Push to server:         sqlrsync LOCAL [REMOTE] [OPTIONS]

Examples:
  sqlrsync namespace/db.sqlite                    # Pull to local db.sqlite
  sqlrsync namespace/db.sqlite --subscribe       # Pull and watch for updates
  sqlrsync mydb.sqlite namespace/db.sqlite       # Push local to remote
`,
	Version: VERSION,
	PreRun: func(cmd *cobra.Command, args []string) {
		setupLogger()
	},
	RunE:          runSync,
	SilenceErrors: true,
	SilenceUsage:  true,
}

func runSync(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}

	// Preprocess variables
	serverURL = strings.TrimRight(serverURL, "/")

	// Determine operation based on arguments and flags
	operation, localPath, remotePath, err := determineOperation(args)
	if err != nil {
		return err
	}

	versionRaw := strings.SplitN(remotePath, "@", 2)
	version := "latest"
	if len(versionRaw) == 2 {
		version = strings.TrimPrefix(strings.ToLower(versionRaw[1]), "v")
		remotePath = versionRaw[0]
	}

	versionCheck, _ := strconv.Atoi(version)
	if strings.HasPrefix(version, "latest") && versionCheck <= 0 {
		return fmt.Errorf("invalid version specified: %s (must be `latest`, `latest-<number>`, or  `<number>` where the number is greater than 0)", version)
	}

	// Create sync coordinator
	coordinator := sync.NewCoordinator(&sync.Config{
		ServerURL:         serverURL,
		ProvidedAuthToken: getAuthToken(),
		ProvidedPullKey:   pullKey,
		ProvidedPushKey:   pushKey,
		ProvidedReplicaID: replicaID,
		LocalPath:         localPath,
		RemotePath:        remotePath,
		Version:           version, // Could be extended to parse @version syntax
		Operation:         operation,
		SetPublic:         SetPublic,
		DryRun:            dryRun,
		Logger:            logger,
		Verbose:           verbose,
	})

	// Execute the operation
	return coordinator.Execute()
}

func determineOperation(args []string) (sync.Operation, string, string, error) {
	isLocal := func(path string) bool {
		return strings.HasPrefix(path, "/") || strings.HasPrefix(path, "./") ||
			strings.HasPrefix(path, "../") || strings.HasPrefix(path, "~/") ||
			!strings.Contains(path, "/")
	}

	if len(args) == 1 {
		path := args[0]
		if isLocal(path) {
			// LOCAL -> push to default remote
			return sync.OperationPush, path, "", nil
		} else {
			// REMOTE -> pull to default local
			dbname := filepath.Base(path)
			if subscribing {
				return sync.OperationSubscribe, dbname, path, nil
			}
			return sync.OperationPull, dbname, path, nil
		}
	}

	if len(args) == 2 {
		origin, replica := args[0], args[1]
		originLocal := isLocal(origin)
		replicaLocal := isLocal(replica)

		if originLocal && !replicaLocal {
			// LOCAL REMOTE -> push
			return sync.OperationPush, origin, replica, nil
		} else if !originLocal && replicaLocal {
			// REMOTE LOCAL -> pull (or subscribe)
			if subscribing {
				return sync.OperationSubscribe, replica, origin, nil
			}
			return sync.OperationPull, replica, origin, nil
		} else if originLocal && replicaLocal {
			return sync.Operation(0), "", "", fmt.Errorf("local to local sync not yet supported")
		} else {
			return sync.Operation(0), "", "", fmt.Errorf("remote to remote sync not supported")
		}
	}

	return sync.Operation(0), "", "", fmt.Errorf("invalid arguments")
}

func getAuthToken() string {
	// Try environment variable first
	if token := os.Getenv("SQLRSYNC_AUTH_TOKEN"); token != "" {
		return token
	}

	// Try pull/push keys
	if pullKey != "" {
		return pullKey
	}
	if pushKey != "" {
		return pushKey
	}

	// TODO: Could try to load from config files here

	return ""
}

func setupLogger() {
	//config := zap.NewDevelopmentConfig()
	config := zap.Config{
		Level:             zap.NewAtomicLevelAt(zap.InfoLevel),
		Development:       false,
		DisableStacktrace: true, // This disables stack traces
		Encoding:          "console",
		EncoderConfig:     zap.NewProductionEncoderConfig(),
		OutputPaths:       []string{"stdout"},
		ErrorOutputPaths:  []string{"stderr"},
	}

	// zapcore Levels: DebugLevel, InfoLevel, WarnLevel, ErrorLevel, DPanicLevel, PanicLevel, FatalLevel

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

func init() {
	rootCmd.Flags().StringVar(&pullKey, "pullKey", "", "Authentication key for PULL operations")
	rootCmd.Flags().StringVar(&pushKey, "pushKey", "", "Authentication key for PUSH operations")
	rootCmd.Flags().StringVar(&replicaID, "replicaID", "", "Replica ID for the remote database")
	rootCmd.Flags().StringVarP(&serverURL, "server", "s", "wss://sqlrsync.com", "Server URL for operations")
	rootCmd.Flags().BoolVar(&subscribing, "subscribe", false, "Enable subscription to PULL changes")
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	rootCmd.Flags().BoolVar(&SetPublic, "public", false, "Enable public access to the replica (PUSH only)")
	rootCmd.Flags().BoolVar(&dryRun, "dry", false, "Perform a dry run without making changes")
	rootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "Show version information")

}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
