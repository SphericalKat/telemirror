package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// envKeys are the environment variable names the loader reads.
var envKeys = []string{
	"TELEGRAM_TOKEN",
	"SUDO_USERS",
	"AUTHORIZED_CHATS",
	"DOWNLOAD_DIR",
	"DOWNLOAD_ROOT",
	"FILTERED_DOMAINS",
	"FILTERED_FILENAMES",
	"STATUS_UPDATE_INTERVAL_MS",
	"GDRIVE_PARENT_DIR_ID",
	"DRIVE_FILE_PRIVATE",
	"DRIVE_FILE_PRIVATE_EMAILS",
	"COMMANDS_USE_BOT_NAME",
	"COMMANDS_BOT_NAME",
	"IS_TEAM_DRIVE",
	"DATABASE_PATH",
}

// clearConfigEnv removes every Telemirror environment variable for the
// duration of the test and restores the previous values afterwards.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range envKeys {
		old, had := os.LookupEnv(key)
		os.Unsetenv(key)
		if had {
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}
}

// writeFile creates a configuration file with the given permissions.
func writeFile(t *testing.T, name, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureLogs returns the default logger output for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Default().Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
	})
	return &buf
}

func defaultConfig() Config {
	return Config{
		TelegramToken:          "",
		SudoUsers:              []int64{},
		AuthorizedChats:        []int64{},
		DownloadDir:            "downloads",
		DownloadRoot:           "/",
		FilteredDomains:        []string{},
		FilteredFilenames:      []string{},
		StatusUpdateIntervalMS: 12000,
		GDriveParentDirID:      "",
		DriveFilePrivate:       false,
		DriveFilePrivateEmails: []string{},
		CommandsUseBotName:     false,
		CommandBotName:         "",
		IsTeamDrive:            false,
		DatabasePath:           "",
	}
}

func TestLoadReturnsBuiltinDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load with no sources: %v", err)
	}
	if want := defaultConfig(); !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults mismatch\ngot:  %+v\nwant: %+v", cfg, want)
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TELEGRAM_TOKEN", "env-token")
	t.Setenv("SUDO_USERS", "10, 20")
	t.Setenv("AUTHORIZED_CHATS", "-100200300")
	t.Setenv("DOWNLOAD_DIR", "/env-downloads")
	t.Setenv("STATUS_UPDATE_INTERVAL_MS", "5000")
	t.Setenv("FILTERED_DOMAINS", "blocked.example,other.example")
	t.Setenv("DRIVE_FILE_PRIVATE", "true")
	t.Setenv("IS_TEAM_DRIVE", "1")

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load with environment values: %v", err)
	}
	if cfg.TelegramToken != "env-token" {
		t.Errorf("token = %q, want %q", cfg.TelegramToken, "env-token")
	}
	if want := []int64{10, 20}; !reflect.DeepEqual(cfg.SudoUsers, want) {
		t.Errorf("sudo users = %v, want %v", cfg.SudoUsers, want)
	}
	if want := []int64{-100200300}; !reflect.DeepEqual(cfg.AuthorizedChats, want) {
		t.Errorf("authorized chats = %v, want %v", cfg.AuthorizedChats, want)
	}
	if cfg.DownloadDir != "/env-downloads" {
		t.Errorf("download dir = %q, want %q", cfg.DownloadDir, "/env-downloads")
	}
	if cfg.StatusUpdateIntervalMS != 5000 {
		t.Errorf("interval = %d, want 5000", cfg.StatusUpdateIntervalMS)
	}
	if want := []string{"blocked.example", "other.example"}; !reflect.DeepEqual(cfg.FilteredDomains, want) {
		t.Errorf("filtered domains = %v, want %v", cfg.FilteredDomains, want)
	}
	if !cfg.DriveFilePrivate {
		t.Error("drive file private = false, want true")
	}
	if !cfg.IsTeamDrive {
		t.Error("team drive = false, want true")
	}
}

func TestSystemFileOverridesEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TELEGRAM_TOKEN", "env-token")
	t.Setenv("DOWNLOAD_DIR", "/env-downloads")
	system := writeFile(t, "config.toml", `
telegram_token = "system-token"
gdrive_parent_dir_id = "system-folder"
`, 0o600)

	cfg, err := Load(system, "")
	if err != nil {
		t.Fatalf("Load with system file: %v", err)
	}
	if cfg.TelegramToken != "system-token" {
		t.Errorf("token = %q, want the system file value %q", cfg.TelegramToken, "system-token")
	}
	if cfg.DownloadDir != "/env-downloads" {
		t.Errorf("download dir = %q, want the environment value %q", cfg.DownloadDir, "/env-downloads")
	}
	if cfg.GDriveParentDirID != "system-folder" {
		t.Errorf("drive folder = %q, want %q", cfg.GDriveParentDirID, "system-folder")
	}
}

func TestLocalFileOverridesSystemFile(t *testing.T) {
	clearConfigEnv(t)
	system := writeFile(t, "system.toml", `
telegram_token = "system-token"
download_dir = "/system-downloads"
`, 0o600)
	local := writeFile(t, "telemirror.toml", `
download_dir = "/local-downloads"
`, 0o600)

	cfg, err := Load(system, local)
	if err != nil {
		t.Fatalf("Load with both files: %v", err)
	}
	if cfg.DownloadDir != "/local-downloads" {
		t.Errorf("download dir = %q, want the local file value %q", cfg.DownloadDir, "/local-downloads")
	}
	if cfg.TelegramToken != "system-token" {
		t.Errorf("token = %q, want the system file value %q", cfg.TelegramToken, "system-token")
	}
}

func TestMissingFilesDoNotStopStartup(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TELEGRAM_TOKEN", "env-token")
	missing := filepath.Join(t.TempDir(), "missing.toml")

	cfg, err := Load(missing, missing)
	if err != nil {
		t.Fatalf("Load with missing files: %v", err)
	}
	if cfg.TelegramToken != "env-token" {
		t.Errorf("token = %q, want the environment value %q", cfg.TelegramToken, "env-token")
	}
	if want := defaultConfig(); cfg.StatusUpdateIntervalMS != want.StatusUpdateIntervalMS {
		t.Errorf("interval = %d, want the default %d", cfg.StatusUpdateIntervalMS, want.StatusUpdateIntervalMS)
	}
}

func TestLoadSupportsEverySettingFromOneFile(t *testing.T) {
	clearConfigEnv(t)
	local := writeFile(t, "telemirror.toml", `
telegram_token = "file-token"
sudo_users = [7, 8]
authorized_chats = [-1001234567890, -1009876543210]
download_dir = "/data/downloads"
download_root = "/data"
filtered_domains = ["blocked.example", "YTS"]
filtered_filenames = ["YIFY"]
status_update_interval_ms = 9000
gdrive_parent_dir_id = "parent-id"
drive_file_private = true
drive_file_private_emails = ["reader@example.com", "other@example.com"]
commands_use_bot_name = true
commands_bot_name = "@telemirror_bot"
is_team_drive = true
database_path = "/var/lib/telemirror/telemirror.db"
`, 0o600)

	cfg, err := Load("", local)
	if err != nil {
		t.Fatalf("Load with a complete file: %v", err)
	}
	want := Config{
		TelegramToken:          "file-token",
		SudoUsers:              []int64{7, 8},
		AuthorizedChats:        []int64{-1001234567890, -1009876543210},
		DownloadDir:            "/data/downloads",
		DownloadRoot:           "/data",
		FilteredDomains:        []string{"blocked.example", "YTS"},
		FilteredFilenames:      []string{"YIFY"},
		StatusUpdateIntervalMS: 9000,
		GDriveParentDirID:      "parent-id",
		DriveFilePrivate:       true,
		DriveFilePrivateEmails: []string{"reader@example.com", "other@example.com"},
		CommandsUseBotName:     true,
		CommandBotName:         "@telemirror_bot",
		IsTeamDrive:            true,
		DatabasePath:           "/var/lib/telemirror/telemirror.db",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("loaded config mismatch\ngot:  %+v\nwant: %+v", cfg, want)
	}
}

func TestInvalidTomlProducesErrorWithFileName(t *testing.T) {
	clearConfigEnv(t)
	broken := writeFile(t, "broken.toml", "telegram_token = ", 0o600)

	_, err := Load(broken, "")
	if err == nil {
		t.Fatal("Load with invalid TOML succeeded, want an error")
	}
	if !strings.Contains(err.Error(), broken) {
		t.Errorf("error %q does not mention the file %q", err, broken)
	}
}

func TestInvalidValuesProduceClearErrors(t *testing.T) {
	clearConfigEnv(t)
	cases := map[string]struct {
		env    map[string]string
		file   string
		wantIn string
	}{
		"zero interval": {
			file:   "status_update_interval_ms = 0",
			wantIn: "status_update_interval_ms",
		},
		"negative interval": {
			file:   "status_update_interval_ms = -1",
			wantIn: "status_update_interval_ms",
		},
		"non-numeric sudo user": {
			file:   "sudo_users = [\"not-a-number\"]",
			wantIn: "sudo_users",
		},
		"non-numeric sudo user from environment": {
			env:    map[string]string{"SUDO_USERS": "not-a-number"},
			wantIn: "sudo_users",
		},
		"non-numeric interval from environment": {
			env:    map[string]string{"STATUS_UPDATE_INTERVAL_MS": "soon"},
			wantIn: "status_update_interval_ms",
		},
		"empty download dir": {
			file:   "download_dir = \"\"",
			wantIn: "download_dir",
		},
		"bot username required when commands use bot name": {
			file:   "commands_use_bot_name = true",
			wantIn: "commands_bot_name",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for key, val := range tc.env {
				t.Setenv(key, val)
			}
			path := ""
			if tc.file != "" {
				path = writeFile(t, "invalid.toml", tc.file, 0o600)
			}
			_, err := Load(path, "")
			if err == nil {
				t.Fatalf("Load succeeded, want an error mentioning %q", tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestWarnsWhenConfigFileIsReadableByOthers(t *testing.T) {
	clearConfigEnv(t)
	logs := captureLogs(t)
	worldReadable := writeFile(t, "shared.toml", "telegram_token = \"x\"\n", 0o644)
	ownerOnly := writeFile(t, "private.toml", "telegram_token = \"y\"\n", 0o600)

	if _, err := Load(ownerOnly, worldReadable); err != nil {
		t.Fatalf("Load with a readable file: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, worldReadable) {
		t.Errorf("warning %q does not mention the readable file %q", out, worldReadable)
	}
	if strings.Contains(out, ownerOnly) {
		t.Errorf("warning %q mentions the owner-only file %q, want no warning for it", out, ownerOnly)
	}
}

func TestNoPermissionWarningForOwnerOnlyFiles(t *testing.T) {
	clearConfigEnv(t)
	logs := captureLogs(t)
	private := writeFile(t, "private.toml", "telegram_token = \"x\"\n", 0o600)

	if _, err := Load(private, private); err != nil {
		t.Fatalf("Load with owner-only files: %v", err)
	}
	if out := logs.String(); out != "" {
		t.Errorf("logged %q, want no warnings for owner-only files", out)
	}
}

func TestSecretsLoadFromEitherSource(t *testing.T) {
	clearConfigEnv(t)

	t.Run("from environment", func(t *testing.T) {
		t.Setenv("TELEGRAM_TOKEN", "secret-env")
		cfg, err := Load("", "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.TelegramToken != "secret-env" {
			t.Errorf("token = %q, want the environment value", cfg.TelegramToken)
		}
	})

	t.Run("from file", func(t *testing.T) {
		file := writeFile(t, "telemirror.toml", "telegram_token = \"secret-file\"\n", 0o600)
		cfg, err := Load("", file)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.TelegramToken != "secret-file" {
			t.Errorf("token = %q, want the file value", cfg.TelegramToken)
		}
	})
}

func TestEmptyEnvironmentListValueYieldsEmptyList(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("SUDO_USERS", "")

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load with an empty list value: %v", err)
	}
	if len(cfg.SudoUsers) != 0 {
		t.Errorf("sudo users = %v, want an empty list", cfg.SudoUsers)
	}
}
