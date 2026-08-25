// Package config loads the Telemirror settings.
//
// Load reads built-in defaults, environment variables, a system TOML file,
// and a local TOML file in that order.
// Each later source overrides the earlier values.
// A missing TOML file does not stop startup.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/confmap"
	envprovider "github.com/knadh/koanf/providers/env"
	fileprovider "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// DefaultSystemPath is the system configuration file path.
const DefaultSystemPath = "/etc/telemirror/config.toml"

// DefaultLocalPath is the local configuration file path in the working directory.
const DefaultLocalPath = "./telemirror.toml"

// Config holds every setting Telemirror needs.
type Config struct {
	// TelegramToken is the bot token for the Telegram API.
	TelegramToken string `koanf:"telegram_token"`

	// SudoUsers lists the Telegram users who can use the bot in any chat.
	SudoUsers []int64 `koanf:"sudo_users"`

	// AuthorizedChats lists the Telegram chats where all users can use the bot.
	AuthorizedChats []int64 `koanf:"authorized_chats"`

	// DownloadDir is the directory that holds the local downloads.
	DownloadDir string `koanf:"download_dir"`

	// DownloadRoot is the mount point that /disk reports.
	DownloadRoot string `koanf:"download_root"`

	// FilteredDomains lists the blocked domain substrings.
	FilteredDomains []string `koanf:"filtered_domains"`

	// FilteredFilenames lists the blocked file-name substrings.
	FilteredFilenames []string `koanf:"filtered_filenames"`

	// StatusUpdateIntervalMS is the delay between two status message updates.
	StatusUpdateIntervalMS int64 `koanf:"status_update_interval_ms"`

	// GDriveParentDirID is the Google Drive folder that holds the published results.
	GDriveParentDirID string `koanf:"gdrive_parent_dir_id"`

	// DriveFilePrivate makes the published results private.
	DriveFilePrivate bool `koanf:"drive_file_private"`

	// DriveFilePrivateEmails lists the accounts that may read private results.
	DriveFilePrivateEmails []string `koanf:"drive_file_private_emails"`

	// CommandsUseBotName makes group commands require the bot username.
	CommandsUseBotName bool `koanf:"commands_use_bot_name"`

	// CommandBotName is the bot username that group commands require.
	CommandBotName string `koanf:"commands_bot_name"`

	// IsTeamDrive publishes the results to a Shared Drive.
	IsTeamDrive bool `koanf:"is_team_drive"`

	// DatabasePath is the SQLite database file path.
	// An empty path disables persistence.
	DatabasePath string `koanf:"database_path"`
}

// Load reads the configuration sources in order and returns the result.
//
// The sources are the built-in defaults, the environment, systemPath, and
// localPath.
// Each later source overrides the earlier values.
// A missing file is not an error.
// An empty path loads no file.
func Load(systemPath, localPath string) (Config, error) {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(builtinDefaults(), "."), nil); err != nil {
		return Config{}, fmt.Errorf("load built-in defaults: %w", err)
	}

	if err := k.Load(envprovider.ProviderWithValue("", ".", environmentValue), nil); err != nil {
		return Config{}, fmt.Errorf("load environment variables: %w", err)
	}

	if err := loadTOMLFile(k, systemPath); err != nil {
		return Config{}, err
	}
	if err := loadTOMLFile(k, localPath); err != nil {
		return Config{}, err
	}

	var cfg Config
	err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook:       mapstructure.StringToSliceHookFunc(","),
			WeaklyTypedInput: true,
		},
	})
	if err != nil {
		return Config{}, fmt.Errorf("invalid configuration values: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// environmentKeys maps environment variable names to configuration keys.
var environmentKeys = map[string]string{
	"TELEGRAM_TOKEN":            "telegram_token",
	"SUDO_USERS":                "sudo_users",
	"AUTHORIZED_CHATS":          "authorized_chats",
	"DOWNLOAD_DIR":              "download_dir",
	"DOWNLOAD_ROOT":             "download_root",
	"FILTERED_DOMAINS":          "filtered_domains",
	"FILTERED_FILENAMES":        "filtered_filenames",
	"STATUS_UPDATE_INTERVAL_MS": "status_update_interval_ms",
	"GDRIVE_PARENT_DIR_ID":      "gdrive_parent_dir_id",
	"DRIVE_FILE_PRIVATE":        "drive_file_private",
	"DRIVE_FILE_PRIVATE_EMAILS": "drive_file_private_emails",
	"COMMANDS_USE_BOT_NAME":     "commands_use_bot_name",
	"COMMANDS_BOT_NAME":         "commands_bot_name",
	"IS_TEAM_DRIVE":             "is_team_drive",
	"DATABASE_PATH":             "database_path",
}

// listKeys names the configuration keys that hold a list.
// Their environment values are comma-separated.
var listKeys = map[string]bool{
	"sudo_users":                true,
	"authorized_chats":          true,
	"filtered_domains":          true,
	"filtered_filenames":        true,
	"drive_file_private_emails": true,
}

// environmentValue converts one environment variable into a configuration
// key and value.
// It returns an empty key for an unknown variable, so the loader ignores it.
func environmentValue(name, value string) (string, any) {
	key, known := environmentKeys[name]
	if !known {
		return "", nil
	}
	if listKeys[key] {
		return key, splitList(value)
	}
	return key, value
}

// splitList splits a comma-separated value and removes the spaces around
// each item.
func splitList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{}
	}
	items := strings.Split(value, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	return items
}

// builtinDefaults returns the built-in configuration values.
func builtinDefaults() map[string]any {
	return map[string]any{
		"telegram_token":            "",
		"sudo_users":                []int64{},
		"authorized_chats":          []int64{},
		"download_dir":              "downloads",
		"download_root":             "/",
		"filtered_domains":          []string{},
		"filtered_filenames":        []string{},
		"status_update_interval_ms": int64(12000),
		"gdrive_parent_dir_id":      "",
		"drive_file_private":        false,
		"drive_file_private_emails": []string{},
		"commands_use_bot_name":     false,
		"commands_bot_name":         "",
		"is_team_drive":             false,
		"database_path":             "",
	}
}

// loadTOMLFile reads one optional TOML file into k.
// A missing file is not an error.
// The function logs a warning when other users can read the file.
func loadTOMLFile(k *koanf.Koanf, path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read configuration file %s: %w", path, err)
	}
	if err := k.Load(fileprovider.Provider(path), toml.Parser()); err != nil {
		return fmt.Errorf("parse configuration file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o044 != 0 {
		log.Printf("config: %s is readable by other users; stored secrets may be exposed", path)
	}
	return nil
}

// validate rejects configuration values that Telemirror cannot use.
func (c Config) validate() error {
	if strings.TrimSpace(c.DownloadDir) == "" {
		return errors.New("invalid configuration: download_dir must not be empty")
	}
	if c.StatusUpdateIntervalMS <= 0 {
		return fmt.Errorf("invalid configuration: status_update_interval_ms must be greater than zero, got %d", c.StatusUpdateIntervalMS)
	}
	if c.CommandsUseBotName && strings.TrimSpace(c.CommandBotName) == "" {
		return errors.New("invalid configuration: commands_bot_name must be set when commands_use_bot_name is enabled")
	}
	return nil
}
