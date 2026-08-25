package mirror

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/SphericalKat/telemirror/internal/engine"
)

// progressMaxSize is the width of a progress bar in full blocks.
const progressMaxSize = 12

// progressIncomplete holds the partial block characters, from one eighth to
// seven eighths of a block.
var progressIncomplete = []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// formatSize renders a byte count the way the upstream bot does: decimal
// units with binary thresholds and two fractional digits.
func formatSize(size int64) string {
	formatNumber := func(n float64) string {
		return strconv.FormatFloat(round2(n), 'f', -1, 64)
	}
	switch {
	case size < 1000:
		return formatNumber(float64(size)) + "B"
	case size < 1024000:
		return formatNumber(float64(size)/1024) + "KB"
	case size < 1048576000:
		return formatNumber(float64(size)/1048576) + "MB"
	default:
		return formatNumber(float64(size)/1073741824) + "GB"
	}
}

// round2 rounds to two fractional digits, like the upstream formatNumber.
func round2(n float64) float64 {
	return math.Round(n*100) / 100
}

// progressBar renders a percent value as a block bar.
func progressBar(p int) string {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	full := p / 8
	part := p%8 - 1
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(strings.Repeat("█", full))
	if part >= 0 {
		b.WriteString(progressIncomplete[part])
	}
	b.WriteString(strings.Repeat(" ", progressMaxSize-full))
	fmt.Fprintf(&b, "] %d%%", p)
	return b.String()
}

// formatETA renders the estimated remaining download time.
func formatETA(total, completed, speed int64) string {
	if speed == 0 {
		return "-"
	}
	remaining := (total - completed) / speed
	seconds := remaining % 60
	minutes := (remaining / 60) % 60
	hours := remaining / 3600
	switch {
	case hours == 0 && minutes == 0:
		return fmt.Sprintf("%ds", seconds)
	case hours == 0:
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
}

// nameFromURI extracts a display name from a download URI by removing the
// fragment or query and keeping the last path segment. It returns an empty
// string when the URI ends without a name.
func nameFromURI(uri string) string {
	if i := strings.Index(uri, "#"); i >= 0 {
		uri = uri[:i]
	}
	if i := strings.Index(uri, "?"); i >= 0 {
		uri = uri[:i]
	}
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		uri = uri[i+1:]
	}
	return uri
}

// downloadName returns the file or top-level directory name of a download,
// as the upstream bot derives it from the aria2 file list. It falls back to
// the request URI when the engine has no usable file path, and reports
// "Metadata" for torrent metadata downloads.
func downloadName(info engine.DownloadInfo, url string) string {
	var filePath string
	if len(info.Files) > 0 && info.Files[0].Path != "" {
		p := info.Files[0].Path
		if info.Dir != "" && strings.HasPrefix(p, info.Dir+"/") && !strings.HasSuffix(p, ".torrent") {
			filePath = p
		}
	}
	if filePath != "" {
		rel := strings.TrimPrefix(filePath, info.Dir+"/")
		if i := strings.Index(rel, "/"); i >= 0 {
			return rel[:i]
		}
		return rel
	}
	if len(info.Files) > 0 && info.Files[0].Path != "" {
		if p := info.Files[0].Path; strings.HasPrefix(p, "[METADATA]") {
			return p[len("[METADATA]"):]
		}
		return "Metadata"
	}
	return nameFromURI(url)
}

// downloadRoot returns the path to publish: the downloaded file itself, or
// the top-level directory when the download produced nested content.
func downloadRoot(info engine.DownloadInfo) string {
	p := info.Files[0].Path
	if info.Dir == "" {
		return p
	}
	rel := strings.TrimPrefix(p, info.Dir+"/")
	if i := strings.Index(rel, "/"); i >= 0 {
		return info.Dir + "/" + rel[:i]
	}
	return p
}

// statusLine renders one download for a status message.
// Queued and paused downloads show a short line; active and uploading
// downloads show the progress block from the upstream bot.
func statusLine(view statusView, info engine.DownloadInfo) string {
	name := downloadName(info, view.url)
	if view.uploading {
		return progressMessage("Uploading", name, info.TotalLength, view.uploadedBytes, view.speed)
	}
	switch info.Status {
	case engine.StatusActive:
		return progressMessage("Filename", name, info.TotalLength, info.CompletedLength, info.DownloadSpeed)
	case engine.StatusWaiting:
		return fmt.Sprintf("<i>%s</i> - Queued", name)
	default:
		return fmt.Sprintf("<i>%s</i> - %s", name, info.Status)
	}
}

// progressMessage renders the multi-line progress block. The speed is given
// in bytes per second and the message appends "ps" for "per second".
func progressMessage(kind, name string, total, completed, speed int64) string {
	percent := 0
	if total > 0 {
		percent = int((completed*100 + total/2) / total)
	}
	return fmt.Sprintf(
		"<b>%s</b>: <code>%s</code>\n<b>Size</b>: <code>%s</code>\n<b>Progress</b>: <code>%s</code>\n<b>Speed</b>: <code>%sps</code>\n<b>ETA</b>: <code>%s</code>",
		kind, name, formatSize(total), progressBar(percent), formatSize(speed), formatETA(total, completed, speed))
}
