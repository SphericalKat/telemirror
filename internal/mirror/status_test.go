package mirror

import (
	"path/filepath"
	"testing"

	"github.com/SphericalKat/telemirror/internal/engine"
)

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0B"},
		{512, "512B"},
		{999, "999B"},
		{1000, "0.98KB"},
		{1536, "1.5KB"},
		{1024000, "0.98MB"},
		{1572864, "1.5MB"},
		{1048576000, "0.98GB"},
		{1610612736, "1.5GB"},
	}
	for _, tc := range cases {
		if got := formatSize(tc.bytes); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestProgressBar(t *testing.T) {
	cases := []struct {
		percent int
		want    string
	}{
		{0, "[            ] 0%"},
		{8, "[█           ] 8%"},
		{50, "[██████▎      ] 50%"},
		// The upstream bar keeps a partial block at 100%, because
		// 100 % 8 is 4. Preserve the quirk for compatibility.
		{100, "[████████████▌] 100%"},
	}
	for _, tc := range cases {
		if got := progressBar(tc.percent); got != tc.want {
			t.Errorf("progressBar(%d) = %q, want %q", tc.percent, got, tc.want)
		}
	}
	if got := progressBar(-5); got != "[            ] 0%" {
		t.Errorf("progressBar(-5) = %q, want it clamped to 0%%", got)
	}
	if got := progressBar(120); got != "[████████████▌] 100%" {
		t.Errorf("progressBar(120) = %q, want it clamped to the 100%% bar", got)
	}
}

func TestFormatETA(t *testing.T) {
	cases := []struct {
		name               string
		total, done, speed int64
		want               string
	}{
		{name: "no speed", total: 100, done: 0, speed: 0, want: "-"},
		{name: "under a minute", total: 1000, done: 900, speed: 100, want: "1s"},
		{name: "minutes and seconds", total: 6100, done: 100, speed: 100, want: "1m 0s"},
		{name: "hours minutes seconds", total: 3662000, done: 1000, speed: 1000, want: "1h 1m 1s"},
	}
	for _, tc := range cases {
		if got := formatETA(tc.total, tc.done, tc.speed); got != tc.want {
			t.Errorf("%s: formatETA(%d, %d, %d) = %q, want %q", tc.name, tc.total, tc.done, tc.speed, got, tc.want)
		}
	}
}

func TestNameFromURI(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"http://example.com/file.bin", "file.bin"},
		{"http://example.com/path/file.bin#fragment", "file.bin"},
		{"http://example.com/file.bin?query=1", "file.bin"},
		{"http://example.com/file.bin/?query=1", ""},
		{"http://example.com/", ""},
	}
	for _, tc := range cases {
		if got := nameFromURI(tc.uri); got != tc.want {
			t.Errorf("nameFromURI(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

func TestDownloadName(t *testing.T) {
	dir := "/downloads/uuid"
	cases := []struct {
		name string
		info engine.DownloadInfo
		url  string
		want string
	}{
		{
			name: "single file",
			info: engine.DownloadInfo{Dir: dir, Files: []engine.File{{Path: filepath.Join(dir, "movie.mkv")}}},
			url:  "http://example.com/movie.mkv",
			want: "movie.mkv",
		},
		{
			name: "top level directory",
			info: engine.DownloadInfo{Dir: dir, Files: []engine.File{{Path: filepath.Join(dir, "season", "e1.mkv")}}},
			url:  "http://example.com/torrent",
			want: "season",
		},
		{
			name: "torrent metadata download",
			info: engine.DownloadInfo{Dir: dir, Files: []engine.File{{Path: filepath.Join(dir, "meta.torrent")}}},
			url:  "http://example.com/pack.torrent",
			want: "Metadata",
		},
		{
			name: "no files uses the URI",
			info: engine.DownloadInfo{Dir: dir},
			url:  "http://example.com/queued.bin",
			want: "queued.bin",
		},
	}
	for _, tc := range cases {
		if got := downloadName(tc.info, tc.url); got != tc.want {
			t.Errorf("%s: downloadName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStatusLine(t *testing.T) {
	dir := "/downloads/uuid"
	rec := &record{url: "http://example.com/file.bin"}

	queued := engine.DownloadInfo{Status: engine.StatusWaiting, Dir: dir}
	if got, want := statusLine(rec.view(), queued), "<i>file.bin</i> - Queued"; got != want {
		t.Errorf("queued line = %q, want %q", got, want)
	}

	paused := engine.DownloadInfo{Status: engine.StatusPaused, Dir: dir}
	if got, want := statusLine(rec.view(), paused), "<i>file.bin</i> - paused"; got != want {
		t.Errorf("paused line = %q, want %q", got, want)
	}

	active := engine.DownloadInfo{
		Status:          engine.StatusActive,
		Dir:             dir,
		TotalLength:     1536,
		CompletedLength: 1536,
		DownloadSpeed:   1536,
		Files:           []engine.File{{Path: filepath.Join(dir, "file.bin")}},
	}
	want := "<b>Filename</b>: <code>file.bin</code>\n" +
		"<b>Size</b>: <code>1.5KB</code>\n" +
		"<b>Progress</b>: <code>[████████████▌] 100%</code>\n" +
		"<b>Speed</b>: <code>1.5KBps</code>\n" +
		"<b>ETA</b>: <code>0s</code>"
	if got := statusLine(rec.view(), active); got != want {
		t.Errorf("active line = %q, want %q", got, want)
	}
}
