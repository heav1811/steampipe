package connectionwatcher

import (
	"context"
	sdkproto "github.com/turbot/steampipe-plugin-sdk/v4/grpc/proto"
	"log"

	"github.com/fsnotify/fsnotify"
	filehelpers "github.com/turbot/go-kit/files"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe/pkg/cmdconfig"
	"github.com/turbot/steampipe/pkg/constants"
	"github.com/turbot/steampipe/pkg/db/db_local"
	"github.com/turbot/steampipe/pkg/filepaths"
	"github.com/turbot/steampipe/pkg/steampipeconfig"
	"github.com/turbot/steampipe/pkg/utils"
)

type ConnectionWatcher struct {
	fileWatcherErrorHandler   func(error)
	watcher                   *utils.FileWatcher
	onConnectionConfigChanged func(configMap map[string]*sdkproto.ConnectionConfig)
	count                     int
}

func NewConnectionWatcher(onConnectionChanged func(configMap map[string]*sdkproto.ConnectionConfig)) (*ConnectionWatcher, error) {
	w := &ConnectionWatcher{
		onConnectionConfigChanged: onConnectionChanged,
	}

	watcherOptions := &utils.WatcherOptions{
		Directories: []string{filepaths.EnsureConfigDir()},
		Include:     filehelpers.InclusionsFromExtensions([]string{constants.ConfigExtension}),
		ListFlag:    filehelpers.FilesRecursive,

		OnChange: func(events []fsnotify.Event) {
			w.handleFileWatcherEvent(events)
		},
	}
	watcher, err := utils.NewWatcher(watcherOptions)
	if err != nil {
		return nil, err
	}
	w.watcher = watcher

	// set the file watcher error handler, which will get called when there are parsing errors
	// after a file watcher event
	w.fileWatcherErrorHandler = func(err error) {
		log.Printf("[WARN] failed to reload connection config: %s", err.Error())
	}

	watcher.Start()

	log.Printf("[INFO] created ConnectionWatcher")
	return w, nil
}

func (w *ConnectionWatcher) handleFileWatcherEvent(e []fsnotify.Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WARN] ConnectionWatcher caught a panic: %s", helpers.ToError(r).Error())
		}
	}()

	// this is a file system event handler and not bound to any context
	ctx := context.Background()

	// ignore the first event - this is raised as soon as we start the watcher
	// (this is to avoid conflicting calls to refreshConnections between Steampipe and the watcher)
	w.count++
	if w.count == 1 {
		log.Printf("[TRACE] handleFileWatcherEvent ignoring first event")
		return
	}

	log.Printf("[TRACE] ConnectionWatcher handleFileWatcherEvent")
	config, err := steampipeconfig.LoadConnectionConfig()
	if err != nil {
		log.Printf("[WARN] error loading updated connection config: %s", err.Error())
		return
	}
	log.Printf("[TRACE] loaded updated config")

	client, err := db_local.NewLocalClient(ctx, constants.InvokerConnectionWatcher)
	if err != nil {
		log.Printf("[WARN] error creating client to handle updated connection config: %s", err.Error())
	}
	defer client.Close(ctx)

	log.Printf("[TRACE] loaded updated config")

	log.Printf("[TRACE] calling onConnectionConfigChanged")
	// convert config to format expected by plugin manager
	// (plugin manager cannot reference steampipe config to avoid circular deps)
	configMap := NewConnectionConfigMap(config.Connections)
	// call on changed callback
	// (this calls pluginmanager.SetConnectionConfigMap)
	w.onConnectionConfigChanged(configMap)

	log.Printf("[TRACE] calling RefreshConnectionAndSearchPaths")

	// We need to update the viper config and GlobalConfig
	// as these are both used by RefreshConnectionAndSearchPaths

	// set the global steampipe config
	steampipeconfig.GlobalConfig = config
	// update the viper default based on this loaded config
	cmdconfig.SetViperDefaults(config.ConfigMap())
	// now refresh connections and search paths
	refreshResult := client.RefreshConnectionAndSearchPaths(ctx)
	if refreshResult.Error != nil {
		log.Printf("[WARN] error refreshing connections: %s", refreshResult.Error)
		return
	}

	// display any refresh warnings
	refreshResult.ShowWarnings()
}

func (w *ConnectionWatcher) Close() {
	w.watcher.Close()
}
