package cutter

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Verifieddanny/clipreator-cli/internal/analyzer"
)

func DownloadVideo(url, outputDir string) (string, error) {
	os.MkdirAll(outputDir, 0755)
	outputPath := filepath.Join(outputDir, "video.mp4")

	if _, err := os.Stat(outputPath); err == nil {
		return outputPath, nil
	}

	cmd := exec.Command("yt-dlp",
		"-f", "bestvideo[height<=1080][ext=mp4]+bestaudio[ext=m4a]/best[height<=1080][ext=mp4]/best",
		"--merge-output-format", "mp4",
		"--no-warnings",
		"-o", outputPath,
		url,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to download video: %w", err)
	}
	return outputPath, nil
}

type silentSegment struct {
	Start float64
	End   float64
}

type ClipResult struct {
	Path         string
	KeptSegments []KeptSegment
}

type KeptSegment struct {
	OrigStart float64
	OrigEnd   float64
	NewStart  float64
}

// detectSilence runs ffmpeg silencedetect and returns silent segments
func detectSilence(videoPath string) ([]silentSegment, error) {
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-af", "silencedetect=noise=-30dB:d=0.5",
		"-f", "null", "-",
	)

	// silencedetect outputs to stderr
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var segments []silentSegment
	scanner := bufio.NewScanner(stderr)
	startRe := regexp.MustCompile(`silence_start: ([\d.]+)`)
	endRe := regexp.MustCompile(`silence_end: ([\d.]+)`)

	var currentStart float64
	hasStart := false

	for scanner.Scan() {
		line := scanner.Text()
		if m := startRe.FindStringSubmatch(line); m != nil {
			currentStart, _ = strconv.ParseFloat(m[1], 64)
			hasStart = true
		}
		if m := endRe.FindStringSubmatch(line); m != nil && hasStart {
			end, _ := strconv.ParseFloat(m[1], 64)
			segments = append(segments, silentSegment{Start: currentStart, End: end})
			hasStart = false
		}
	}

	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return nil, err
	}

	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	return segments, nil
}

func getVideoDuration(videoPath string) (float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

func CutClips(videoPath, outputDir string, clips []analyzer.Clip) ([]ClipResult, error) {
	clipsDir := filepath.Join(outputDir, "clips")
	os.MkdirAll(clipsDir, 0755)

	var results []ClipResult

	for i, clip := range clips {
		fmt.Printf("\n  ✂️  Clip %d: %s\n", i+1, clip.Title)
		duration := clip.End - clip.Start

		rawPath := filepath.Join(clipsDir, fmt.Sprintf("raw_%d.mp4", i+1))
		fmt.Printf("    📐 Cutting raw segment (%.0fs)...\n", duration)

		cmd := exec.Command("ffmpeg", "-y",
			"-ss", fmt.Sprintf("%.2f", clip.Start),
			"-i", videoPath,
			"-t", fmt.Sprintf("%.2f", duration),
			"-c", "copy",
			"-avoid_negative_ts", "make_zero",
			rawPath,
		)
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			fmt.Printf("    ⚠️  Failed to cut: %v\n", err)
			continue
		}

		fmt.Printf("    🔇 Detecting dead space...\n")
		silentParts, err := detectSilence(rawPath)
		if err != nil {
			silentParts = nil
		}

		totalDur, _ := getVideoDuration(rawPath)

		type segment struct{ Start, End float64 }
		var keepSegments []segment

		if len(silentParts) > 0 {
			sort.Slice(silentParts, func(a, b int) bool {
				return silentParts[a].Start < silentParts[b].Start
			})

			cursor := 0.0
			for _, s := range silentParts {
				if s.Start-cursor > 0.1 {
					keepSegments = append(keepSegments, segment{cursor, s.Start + 0.05})
				}
				cursor = s.End - 0.05
				if cursor < 0 {
					cursor = 0
				}
			}
			if cursor < totalDur {
				keepSegments = append(keepSegments, segment{cursor, totalDur})
			}

			removed := 0.0
			for _, s := range silentParts {
				removed += s.End - s.Start
			}
			fmt.Printf("    🗑️  Found %d silent gaps (%.1fs of dead space)\n", len(silentParts), removed)
		} else {
			keepSegments = append(keepSegments, segment{0, totalDur})
			fmt.Printf("    ✓ No significant dead space found\n")
		}

		// Build the time mapping for caption sync
		var keptInfo []KeptSegment
		newCursor := 0.0
		for _, seg := range keepSegments {
			segDur := seg.End - seg.Start
			if segDur < 0.2 {
				continue
			}
			keptInfo = append(keptInfo, KeptSegment{
				OrigStart: seg.Start,
				OrigEnd:   seg.End,
				NewStart:  newCursor,
			})
			newCursor += segDur
		}

		outputPath := filepath.Join(clipsDir, fmt.Sprintf("clip_%d.mp4", i+1))

		if len(keepSegments) == 1 && len(silentParts) == 0 {
			fmt.Printf("    📱 Cropping to 9:16 vertical...\n")
			cmd = exec.Command("ffmpeg", "-y",
				"-i", rawPath,
				"-vf", "crop=ih*9/16:ih,scale=1080:1920",
				"-c:v", "libx264", "-preset", "fast", "-crf", "23",
				"-c:a", "aac", "-b:a", "128k",
				outputPath,
			)
			cmd.Stderr = nil
			if err := cmd.Run(); err != nil {
				fmt.Printf("    ⚠️  Crop failed: %v\n", err)
				continue
			}
		} else {
			var segPaths []string
			segDir := filepath.Join(clipsDir, fmt.Sprintf("segments_%d", i+1))
			os.MkdirAll(segDir, 0755)

			for j, seg := range keepSegments {
				segPath := filepath.Join(segDir, fmt.Sprintf("seg_%03d.mp4", j))
				segDur := seg.End - seg.Start
				if segDur < 0.2 {
					continue
				}

				cmd = exec.Command("ffmpeg", "-y",
					"-ss", fmt.Sprintf("%.3f", seg.Start),
					"-i", rawPath,
					"-t", fmt.Sprintf("%.3f", segDur),
					"-vf", "crop=ih*9/16:ih,scale=1080:1920",
					"-c:v", "libx264", "-preset", "fast", "-crf", "23",
					"-c:a", "aac", "-b:a", "128k",
					"-avoid_negative_ts", "make_zero",
					segPath,
				)
				cmd.Stderr = nil
				if err := cmd.Run(); err != nil {
					continue
				}
				segPaths = append(segPaths, segPath)
			}

			if len(segPaths) == 0 {
				continue
			}

			concatPath := filepath.Join(segDir, "concat.txt")
			var concatContent strings.Builder
			for _, sp := range segPaths {
				absPath, _ := filepath.Abs(sp)
				fmt.Fprintf(&concatContent, "file '%s'\n", absPath)
			}
			os.WriteFile(concatPath, []byte(concatContent.String()), 0644)

			fmt.Printf("    📱 Cropping to 9:16 + removing dead space...\n")
			cmd = exec.Command("ffmpeg", "-y",
				"-f", "concat", "-safe", "0",
				"-i", concatPath,
				"-c", "copy",
				outputPath,
			)
			cmd.Stderr = nil
			if err := cmd.Run(); err != nil {
				continue
			}

			os.RemoveAll(segDir)
		}

		os.Remove(rawPath)

		finalDur, _ := getVideoDuration(outputPath)
		fmt.Printf("    ✅ Saved: %s (%.0fs → %.0fs)\n", outputPath, duration, finalDur)
		results = append(results, ClipResult{
			Path:         outputPath,
			KeptSegments: keptInfo,
		})
	}

	return results, nil
}
