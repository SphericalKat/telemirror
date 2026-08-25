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
	"syscall"
	"time"

	"github.com/SphericalKat/telemirror/internal/config"
	"github.com/SphericalKat/telemirror/internal/drive"
	"github.com/SphericalKat/telemirror/internal/engine"
	"github.com/SphericalKat/telemirror/internal/mirror"
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
	service, err := mirror.New(mirror.Config{
		SudoUsers:            cfg.SudoUsers,
		AuthorizedChats:      cfg.AuthorizedChats,
		DownloadDir:          cfg.DownloadDir,
		FilteredDomains:      cfg.FilteredDomains,
		FilteredFilenames:    cfg.FilteredFilenames,
		StatusUpdateInterval: time.Duration(cfg.StatusUpdateIntervalMS) * time.Millisecond,
		CommandsUseBotName:   cfg.CommandsUseBotName,
		CommandBotName:       cfg.CommandBotName,
		IsTeamDrive:          cfg.IsTeamDrive,
	}, api, eng, publisher, lister)
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
