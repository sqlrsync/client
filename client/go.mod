module github.com/sqlrsync/sqlrsync.com

go 1.24.5

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/fatih/color v1.18.0
	github.com/gorilla/websocket v1.5.0
	github.com/spf13/cobra v1.8.0
	github.com/sqlrsync/sqlrsync.com/bridge v0.0.0-00010101000000-000000000000
	go.uber.org/zap v1.27.0
)

require (
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/sys v0.25.0 // indirect
)

replace github.com/sqlrsync/sqlrsync.com/bridge => ../bridge
