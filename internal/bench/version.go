package bench

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// FFmpegVersion returns the version from the first line of
// "ffmpeg -version", such as "8.1".
func FFmpegVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-version").Output()
	if err != nil {
		return "", fmt.Errorf("running ffmpeg -version: %w", err)
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	if f := strings.Fields(string(line)); len(f) >= 3 && f[0] == "ffmpeg" && f[1] == "version" {
		return f[2], nil
	}
	return strings.TrimSpace(string(line)), nil
}
