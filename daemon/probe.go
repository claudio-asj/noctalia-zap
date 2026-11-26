package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"strconv"
	"time"
)

const probeTimeout = 15 * time.Second

// mediaProbe is what ffprobe tells about a file; zero values mean unknown.
type mediaProbe struct {
	W, H    int
	Seconds float64
}

// probeMedia runs ffprobe on path (always a separate argv element). It
// returns a zero value when ffprobe is missing or fails.
func probeMedia(ctx context.Context, path string) mediaProbe {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return mediaProbe{}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-v", "error", "-show_entries",
		"stream=codec_type,width,height:format=duration", "-of", "json", "-i", path).Output()
	if err != nil {
		return mediaProbe{}
	}
	return parseProbe(out)
}

func parseProbe(out []byte) mediaProbe {
	var v struct {
		Streams []struct {
			Type   string `json:"codec_type"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &v) != nil {
		return mediaProbe{}
	}
	var p mediaProbe
	for _, s := range v.Streams {
		if s.Type == "video" && s.Width > 0 && s.Height > 0 {
			p.W, p.H = s.Width, s.Height
			break
		}
	}
	if d, err := strconv.ParseFloat(v.Format.Duration, 64); err == nil && d > 0 && !math.IsInf(d, 0) {
		p.Seconds = d
	}
	return p
}

// wholeSeconds rounds a duration to seconds, at least 1 when it is positive.
func wholeSeconds(d float64) int {
	if d <= 0 {
		return 0
	}
	return max(1, int(math.Round(d)))
}

// probeSeconds returns the duration of a file in seconds, 0 when unknown.
func probeSeconds(ctx context.Context, path string) int {
	return wholeSeconds(probeMedia(ctx, path).Seconds)
}
