// Command telemirror runs the Telegram mirror bot. It loads the layered
// configuration, authorizes Google Drive with user OAuth, embeds the
// download engine, and connects the mirror service to Telegram.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SphericalKat/telemirror/internal/config"
	"github.com/SphericalKat/telemirror/internal/disk"
	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
	"github.com/SphericalKat/telemirror/internal/storage"
	"github.com/SphericalKat/telemirror/internal/telegram"
)

// Credential file locations follow the upstream bot: the OAuth client
// secret and the saved token live in the working directory.
const (
	clientSecretPath = "client_secret.json"
	tokenPath        = "token.json"
)

// maxConcurrentDownloads matches the upstream aria2 configuration.
const maxConcurrentDownloads = 3

// openStore opens the storage database. It returns nil when no database
// path is configured or when the database cannot be opened or migrated,
// after logging a warning; the bot then runs with in-memory state only.
func openStore(path string) *storage.Store {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	store, err := storage.Open(path)
	if err != nil {
		log.Printf("telemirror: storage disabled, continuing with in-memory state: %v", err)
		return nil
	}
	return store
}

// mirrorConfig builds the mirror service configuration. The store is set
// only when it is not nil, because a typed-nil store would not compare
// equal to a nil interface value and would enable persistence without a
// working database.
func mirrorConfig(cfg config.Config, store *storage.Store) mirror.Config {
	out := mirror.Config{
		SudoUsers:            cfg.SudoUsers,
		AuthorizedChats:      cfg.AuthorizedChats,
		DownloadDir:          cfg.DownloadDir,
		DiskRoot:             cfg.DownloadRoot,
		FilteredDomains:      cfg.FilteredDomains,
		FilteredFilenames:    cfg.FilteredFilenames,
		StatusUpdateInterval: time.Duration(cfg.StatusUpdateIntervalMS) * time.Millisecond,
		CommandsUseBotName:   cfg.CommandsUseBotName,
		CommandBotName:       cfg.CommandBotName,
		IsTeamDrive:          cfg.IsTeamDrive,
		DiskUsage:            disk.Usage,
	}
	if store != nil {
		out.Store = store
	}
	return out
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load(config.DefaultSystemPath, config.DefaultLocalPath)
	if err != nil {
		return err
	}
	if cfg.TelegramToken == "" {
		return errors.New("telegram_token must be set in the environment or a configuration file")
	}
	if cfg.GDriveParentDirID == "" {
		return errors.New("gdrive_parent_dir_id must be set in the environment or a configuration file")
	}

	store := openStore(cfg.DatabasePath)
	if store != nil {
		defer store.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	auth, err := drive.NewAuthenticatorFromSecret(clientSecretPath, tokenPath, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	httpClient, err := auth.Client(ctx)
	if err != nil {
		return err
	}
	driveService, err := drive.NewAPIService(ctx, httpClient)
	if err != nil {
		return err
	}
	publisher, err := drive.NewPublisher(driveService, drive.Config{
		ParentFolderID: cfg.GDriveParentDirID,
		Private: drive.PrivateSharing{
			Enabled: cfg.DriveFilePrivate,
			Emails:  cfg.DriveFilePrivateEmails,
		},
		SharedDrive: cfg.IsTeamDrive,
	})
	if err != nil {
		return err
	}
	lister, err := drive.NewLister(driveService, drive.Config{ParentFolderID: cfg.GDriveParentDirID})
	if err != nil {
		return err
	}

	eng, err := engine.New(engine.Config{
		DownloadDir:   cfg.DownloadDir,
		MaxConcurrent: maxConcurrentDownloads,
	})
	if err != nil {
		return err
	}

	api := telegram.NewAPI(cfg.TelegramToken, nil)
	service, err := mirror.New(mirrorConfig(cfg, store), api, eng, publisher, lister)
	if err != nil {
		return err
	}

	go func() {
		if err := eng.Run(ctx); err != nil {
			log.Printf("telemirror: download engine stopped: %v", err)
		}
	}()
	go func() {
		if err := service.Run(ctx); err != nil {
			log.Printf("telemirror: mirror service stopped: %v", err)
		}
	}()

	log.Println("telemirror: bot ready")
	return api.Poll(ctx, service.HandleUpdate)
}
