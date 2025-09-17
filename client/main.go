package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sqlrsync/sqlrsync.com/sync"
)

var (
	simpleServerURL       string
	simpleVerbose         bool
	simpleDryRun          bool
	simpleSetPublic       bool
	simpleSubscribe       bool
	simplePullKey         string
	simplePushKey         string
	simpleReplicaID       string
	simpleLogger          *zap.Logger
)

var simpleRootCmd = &cobra.Command{
	Use:   "sqlrsync [ORIGIN] [REPLICA] or [LOCAL] or [REMOTE]",
	Short: "SQLite Rsync - Simplified Version",
	Long: `A simplified rsync-like utility for SQLite databases with subscription support.

Usage modes:
1. Pull from server:       sqlrsync REMOTE [LOCAL] [OPTIONS]
2. Pull with subscription: sqlrsync REMOTE [LOCAL] --subscribe [OPTIONS]
3. Push to server:         sqlrsync LOCAL [REMOTE] [OPTIONS]

Examples:
  sqlrsync namespace/db.sqlite                    # Pull to local db.sqlite
  sqlrsync namespace/db.sqlite --subscribe       # Pull and watch for updates
  sqlrsync mydb.sqlite namespace/db.sqlite       # Push local to remote
`,
	Version: "2.0.0-simplified",
	PreRun: func(cmd *cobra.Command, args []string) {
		setupSimpleLogger()
	},
	RunE:          runSimpleSync,
	SilenceErrors: true,
	SilenceUsage:  true,
}

func runSimpleSync(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}

	// Determine operation based on arguments and flags
	operation, localPath, remotePath, err := determineOperation(args)
	if err != nil {
		return err
	}

	// Create sync coordinator
	coordinator := sync.NewCoordinator(&sync.Config{
		ServerURL:         simpleServerURL,
		ProvidedAuthToken: getSimpleAuthToken(),
		ProvidedPullKey:   simplePullKey,
		ProvidedPushKey:   simplePushKey,
		ProvidedReplicaID: simpleReplicaID,
		LocalPath:         localPath,
		RemotePath:        remotePath,
		Version:           "latest", // Could be extended to parse @version syntax
		Operation:         operation,
		SetPublic:         simpleSetPublic,
		DryRun:            simpleDryRun,
		Logger:            simpleLogger,
		Verbose:           simpleVerbose,
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
			if simpleSubscribe {
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
			if simpleSubscribe {
				return sync.OperationSubscribe, replica, origin, nil
			}
			return sync.OperationPull, replica, origin, nil
		} else if originLocal && replicaLocal {
			return sync.Operation(0), "", "", fmt.Errorf("local to local sync not supported in simplified version")
		} else {
			return sync.Operation(0), "", "", fmt.Errorf("remote to remote sync not supported")
		}
	}

	return sync.Operation(0), "", "", fmt.Errorf("invalid arguments")
}

func getSimpleAuthToken() string {
	// Try environment variable first
	if token := os.Getenv("SQLRSYNC_AUTH_TOKEN"); token != "" {
		return token
	}

	// Try pull/push keys
	if simplePullKey != "" {
		return simplePullKey
	}
	if simplePushKey != "" {
		return simplePushKey
	}

	// TODO: Could try to load from config files here

	return ""
}

func setupSimpleLogger() {
	config := zap.NewDevelopmentConfig()

	if simpleVerbose {
		config.Level.SetLevel(zapcore.DebugLevel)
	} else {
		config.Level.SetLevel(zapcore.InfoLevel)
	}

	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	config.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder

	var err error
	simpleLogger, err = config.Build()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
}

func ExecuteSimple() error {
	return simpleRootCmd.Execute()
}

func init() {
	simpleRootCmd.Flags().StringVar(&simplePullKey, "pullKey", "", "Authentication key for PULL operations")
	simpleRootCmd.Flags().StringVar(&simplePushKey, "pushKey", "", "Authentication key for PUSH operations")
	simpleRootCmd.Flags().StringVar(&simpleReplicaID, "replicaID", "", "Replica ID for the remote database")
	simpleRootCmd.Flags().StringVarP(&simpleServerURL, "server", "s", "wss://sqlrsync.com", "Server URL for operations")
	simpleRootCmd.Flags().BoolVar(&simpleSubscribe, "subscribe", false, "Enable subscription to PULL changes")
	simpleRootCmd.Flags().BoolVarP(&simpleVerbose, "verbose", "v", false, "Enable verbose logging")
	simpleRootCmd.Flags().BoolVar(&simpleSetPublic, "public", false, "Enable public access to the replica (PUSH only)")
	simpleRootCmd.Flags().BoolVar(&simpleDryRun, "dry", false, "Perform a dry run without making changes")
}

func main() {
	if err := ExecuteSimple(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}