package main

// external decode: converter reads the file, its stdout
// (PAM/PNG) becomes the picture — webp/avif/heic/jxl and the long tail.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtui"
)

const (
	// externalImageDecoder is the name the interface shows and the name the
	// DecoderPriority setting uses.
	externalImageDecoder = "external"

	// externalImagePriority puts the converter below every built-in
	// decoder: starting a process is dear, so it only runs when nothing
	// inside f4 can read the file.
	externalImagePriority = -10

	// defaultImageExternalTimeout bounds one conversion, in seconds. A raw
	// photograph on a slow machine is a few seconds; a converter that has
	// gone to sleep on a malformed file is forever.
	defaultImageExternalTimeout = 20

	// externalImageStderrLimit is how much of the converter's complaint
	// ends up in the error message the viewer shows in its title.
	externalImageStderrLimit = 200

	// stderr tail capped; a flooding tool cannot grow memory.
	externalImageStderrCap = 512 << 10

	// externalImagePathPlaceholder stands for the input file in a command
	// line.
	externalImagePathPlaceholder = "{}"
)

// externalImageTool is one converter: the binary, the command line that makes
type externalImageTool struct {
	Bin     string
	Args    []string
	Formats []string
	Pam     bool
	Label   string
}

var baseFormats = []string{
	"jpg", "jpeg", "png",
	"webp", "avif", "heic", "heif", "hif",
	"jxl", "jp2", "j2k",
	"tif", "tiff", "exr", "hdr",
	"ppm", "pgm", "pbm", "pnm", "pam", "pfm",
}

func formats(extra []string) []string {
	out := make([]string, 0, len(baseFormats)+len(extra))
	out = append(out, baseFormats...)
	return append(out, extra...)
}

func tool(bin, label string, pam bool, args, fmts []string) externalImageTool {
	return externalImageTool{Bin: bin, Args: args, Formats: fmts, Pam: pam, Label: label}
}

var (
	// raw rides on dcraw/libraw inside ImageMagick, not worth reimplementing.
	imageMagickFormats = formats([]string{
		"jpf", "jpx", "psd", "xcf", "ico", "cur",
		"tga", "pcx", "xpm", "svg", "svgz", "dds", "wbmp",
		"cr2", "cr3", "nef", "arw", "dng", "orf", "raf", "rw2", "pef", "srw",
	})

	// ffmpeg claims only what it reads, so a miss shows an error, not a hex dump.
	ffmpegFormats = formats([]string{"dds", "tga", "pcx", "xpm", "wbmp"})

	vipsFormats = formats([]string{"svg"})
)

var imageMagickArgs = []string{
	externalImagePathPlaceholder,
	"-colorspace", "sRGB",
	"pam:-",
}

var (
	// vips has no PAM writer: P6 stdout; alpha premultiplies onto
	// black (the viewer is already dark).
	vipsArgs = []string{"copy", externalImagePathPlaceholder, ".ppm"}

	ffmpegArgs = []string{
		"-nostdin", "-v", "error",
		"-i", externalImagePathPlaceholder,
		"-frames:v", "1",
		"-f", "image2pipe", "-vcodec", "pam", "-",
	}
)

var (
	vipsTool    = tool("vips", "vips", true, vipsArgs, vipsFormats)
	magickTool  = tool("magick", "im", true, imageMagickArgs, imageMagickFormats)
	convertTool = tool("convert", "im", true, imageMagickArgs, imageMagickFormats)
	ffmpegTool  = tool("ffmpeg", "ffmpeg", true, ffmpegArgs, ffmpegFormats)
)

// Fastest readers first, convert last; unknown formats drive externalImageFallback.
var externalImageTools = []externalImageTool{vipsTool, magickTool, ffmpegTool, convertTool}
var externalImageFallback = []externalImageTool{magickTool, ffmpegTool, vipsTool, convertTool}

var (
	externalImageLookPath     = exec.LookPath
	externalImageDecode       = streamExternalImage
	externalImageDecodeScaled = streamExternalImageScaled
	externalImageTimeout      = configuredExternalImageTimeout
)

// Windows' convert.exe is the system file converter, never ImageMagick.
func installedExternalImageTool(tool externalImageTool) (externalImageTool, bool) {
	if tool.Bin == "convert" && runtime.GOOS == "windows" {
		return externalImageTool{}, false
	}
	path, err := externalImageLookPath(tool.Bin)
	if err != nil || path == "" {
		return externalImageTool{}, false
	}
	tool.Bin = path
	return tool, true
}

// Probe once (LookPath walks the whole PATH); tests reset it.
var (
	installedMu    sync.Mutex
	installedOnce  sync.Once
	installedByBin map[string]externalImageTool
)

func resetInstalledExternalImageTools() {
	installedMu.Lock()
	installedOnce = sync.Once{}
	installedByBin = nil
	installedMu.Unlock()
}

func installedExternalImageToolsLookup() map[string]externalImageTool {
	installedMu.Lock()
	installedOnce.Do(func() {
		m := make(map[string]externalImageTool, len(externalImageTools))
		for _, t := range externalImageTools {
			inst, ok := installedExternalImageTool(t)
			if !ok {
				continue
			}
			m[t.Bin] = inst
		}
		installedByBin = m
	})
	out := installedByBin
	installedMu.Unlock()
	return out
}

func findExternalImageTool(ext string) (externalImageTool, bool) {
	installed := installedExternalImageToolsLookup()
	list := externalImageTools
	if ext == "" {
		list = externalImageFallback
	}
	for _, tool := range list {
		if ext != "" && !slices.Contains(tool.Formats, ext) {
			continue
		}
		if tool, ok := installed[tool.Bin]; ok {
			return tool, true
		}
	}
	return externalImageTool{}, false
}

func externalImageToolFor(data []byte) (externalImageTool, bool) {
	return findExternalImageTool(strings.TrimPrefix(sniffImageSuffix(data), "."))
}

func externalImageToolLabel(data []byte) string {
	tool, ok := externalImageToolFor(data)
	if !ok {
		return ""
	}
	return externalImageDecoder + " (" + tool.Label + ")"
}

// configuredExternalImageTimeout reads the [Images] ExternalTimeout setting.
func configuredExternalImageTimeout() time.Duration {
	seconds := AppConfig.ImageExternalTimeout
	if seconds <= 0 {
		seconds = defaultImageExternalTimeout
	}
	return time.Duration(seconds) * time.Second
}

// command line from externalImageArgs; hideConsoleWindow on Windows.
func newExternalCmd(ctx context.Context, tool externalImageTool, path string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, tool.Bin, externalImageArgs(tool, path)...)
	hideConsoleWindow(cmd)
	return cmd
}

// keeps the tail of stderr; a flooding tool is dropped, not buffered.
type stderrRing struct {
	buf bytes.Buffer
}

func (r *stderrRing) Write(p []byte) (int, error) {
	if len(p) >= externalImageStderrCap {
		r.buf.Reset()
		r.buf.Write(p[len(p)-externalImageStderrCap:])
		return len(p), nil
	}
	if excess := r.buf.Len() + len(p) - externalImageStderrCap; excess > 0 {
		r.buf.Next(excess)
	}
	r.buf.Write(p)
	return len(p), nil
}

func streamExternalImage(ctx context.Context, tool externalImageTool, path string) (*vtui.ImageSurface, error) {
	return streamExternalImageCmd(ctx, tool, newExternalCmd(ctx, tool, path))
}

// tile-sized decode: the converter shrinks while reading.
func streamExternalImageScaled(ctx context.Context, tool externalImageTool, path string, w, h int) (*vtui.ImageSurface, error) {
	cmd := exec.CommandContext(ctx, tool.Bin, externalImageScaledArgs(tool, path, w, h)...)
	hideConsoleWindow(cmd)
	return streamExternalImageCmd(ctx, tool, cmd)
}

func streamExternalImageCmd(ctx context.Context, tool externalImageTool, cmd *exec.Cmd) (*vtui.ImageSurface, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := new(stderrRing)
	cmd.Stdin = nil
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, externalImageCmdError(tool, err, stderr.buf.Bytes())
	}

	var (
		surf      *vtui.ImageSurface
		decodeErr error
	)
	if tool.Pam {
		surf, decodeErr = decodeNetpbmSurfaceStream(stdout)
	} else {
		surf, decodeErr = decodeStdImageStream(stdout)
	}

	// drain stdout before Wait — a full pipe would deadlock.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	runErr := cmd.Wait()

	if runErr != nil {
		return nil, externalImageCmdError(tool, runErr, stderr.buf.Bytes())
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("external tool returned invalid output: %w", decodeErr)
	}
	return surf, nil
}

func externalImageArgs(tool externalImageTool, path string) []string {
	args := make([]string, len(tool.Args))
	for i, arg := range tool.Args {
		args[i] = strings.ReplaceAll(arg, externalImagePathPlaceholder, path)
	}
	return args
}

// scaled command range: tiles are ~hundreds of px.
// vips thumbnail + --size=down never decodes the whole fram;
// IM -thumbnail with "<" only shrinks; ffmpeg -lowres 2 then fits.
func externalImageScaledArgs(tool externalImageTool, path string, w, h int) []string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	switch tool.Label {
	case "vips":
		// vips: width positional, height an option; "W H" fails.
		return []string{
			"thumbnail", path, ".ppm",
			strconv.Itoa(w), "--height", strconv.Itoa(h), "--size=down",
		}
	case "im":
		return []string{
			path, "-thumbnail", fmt.Sprintf("%dx%d>", w, h), "-colorspace", "sRGB", "pam:-",
		}
	default: // ffmpeg
		return []string{
			"-nostdin", "-v", "error",
			"-lowres", "2", "-flags2", "+fast",
			"-i", path,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", w, h),
			"-f", "image2pipe", "-vcodec", "pam", "-",
		}
	}
}

func externalImageCmdError(tool externalImageTool, err error, stderr []byte) error {
	return fmt.Errorf("%s: %v%s", filepath.Base(tool.Bin), err, externalImageStderrTail(stderr))
}

// for benchmarks and capture paths; the viewer streams instead.
func runExternalImageTool(ctx context.Context, tool externalImageTool, path string) ([]byte, error) {
	cmd := newExternalCmd(ctx, tool, path)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = nil
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, externalImageCmdError(tool, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// externalImageStderrTail returns the last line of what the converter
// complained about, short enough to fit in a title bar.
func externalImageStderrTail(stderr []byte) string {
	text := bytes.TrimSpace(stderr)
	if len(text) == 0 {
		return ""
	}
	if i := bytes.LastIndexByte(text, '\n'); i >= 0 {
		text = bytes.TrimSpace(text[i+1:])
		if len(text) == 0 {
			return ""
		}
	}
	return ": " + string(trimRunes(text, externalImageStderrLimit))
}

func trimRunes(s []byte, max int) []byte {
	off := 0
	for i := 0; i < max && off < len(s); i++ {
		_, size := utf8.DecodeRune(s[off:])
		off += size
	}
	if off >= len(s) {
		return s
	}
	return s[:off]
}

// Tries each tool; first success wins, else the next — only the
// automatic chain falls over; a pinned F4 choice stays on one tool.
func decodeImageExternally(ctx context.Context, data []byte) (*vtui.ImageSurface, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("there is nothing to convert")
	}
	ext := strings.TrimPrefix(sniffImageSuffix(data), ".")
	list := externalImageTools
	if ext == "" {
		list = externalImageFallback
	}
	installed := installedExternalImageToolsLookup()
	var firstErr error
	for _, tool := range list {
		if ext != "" && !slices.Contains(tool.Formats, ext) {
			continue
		}
		inst, ok := installed[tool.Bin]
		if !ok {
			continue
		}
		surf, err := decodeImageExternallyTool(ctx, inst, data)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return surf, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("no external image converter on the PATH")
}

func decodeImageExternallyTool(ctx context.Context, tool externalImageTool, data []byte) (*vtui.ImageSurface, error) {
	return decodeImageExternallyVia(ctx, tool, data, externalImageDecode)
}

func decodeImageExternallyScaled(ctx context.Context, tool externalImageTool, data []byte, w, h int) (*vtui.ImageSurface, error) {
	return decodeImageExternallyVia(ctx, tool, data, func(ctx context.Context, tool externalImageTool, path string) (*vtui.ImageSurface, error) {
		return externalImageDecodeScaled(ctx, tool, path, w, h)
	})
}

func decodeImageExternallyVia(ctx context.Context, tool externalImageTool, data []byte, decode func(context.Context, externalImageTool, string) (*vtui.ImageSurface, error)) (*vtui.ImageSurface, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("there is nothing to convert")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// The bytes go through a file rather than through a pipe. heic, avif
	// and jxl are containers that are read by seeking around them, and both
	// ImageMagick's delegates and ffmpeg refuse a stream they cannot
	// rewind. The whole file is in memory already, so this costs one write.
	file, err := os.CreateTemp("", "f4img-*"+sniffImageSuffix(data))
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}

	limit := externalImageTimeout()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	surf, err := decode(ctx, tool, name)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %s", filepath.Base(tool.Bin), limit)
		}
		return nil, err
	}
	if surf == nil {
		return nil, fmt.Errorf("%s produced nothing", filepath.Base(tool.Bin))
	}
	return surf, nil
}

// shrink-while-reading: never a full decode of RAW/AVIF for a tile.
func loadImageScaled(ctx context.Context, v vfs.VFS, path string, w, h int) (*vtui.ImageSurface, string, error) {
	data, err := imageFileBytes(ctx, v, path)
	if err != nil {
		return nil, "", err
	}
	tool, ok := externalImageToolFor(data)
	if !ok {
		return nil, "", fmt.Errorf("no external image converter on the PATH")
	}
	surf, err := decodeImageExternallyScaled(ctx, tool, data, w, h)
	if err != nil {
		return nil, "", err
	}
	return surf, externalImageDecoder + " (" + tool.Label + ")", nil
}

// installedExternalImageTools spreads one tool: magick and convert
// are the same binary under two names.
func installedExternalImageTools() []externalImageTool {
	installed := installedExternalImageToolsLookup()
	var out []externalImageTool
	seen := make(map[string]bool)
	for _, tool := range externalImageTools {
		inst, ok := installed[tool.Bin]
		if !ok || seen[inst.Label] {
			continue
		}
		seen[inst.Label] = true
		out = append(out, inst)
	}
	return out
}

func externalImageToolByLabel(label string) (externalImageTool, bool) {
	for _, tool := range installedExternalImageTools() {
		if tool.Label == label {
			return tool, true
		}
	}
	return externalImageTool{}, false
}

// sniffImageSuffix guesses an extension from the first bytes. The converters
// recognise their formats by content in the end, but ImageMagick picks a
// delegate by the name before it looks inside, so a name that agrees with the
// content saves it a guess. An unrecognised header gets no suffix rather than
// a wrong one.
func sniffImageSuffix(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		// A container. mp4 and mov start the same way, and calling one of
		// those a heic would send ImageMagick to the wrong delegate, so
		// only the brands that really are pictures are named.
		switch string(data[8:12]) {
		case "avif", "avis":
			return ".avif"
		case "jxl ":
			return ".jxl"
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
			return ".heic"
		}
		return ""
	case len(data) >= 12 && string(data[0:12]) == "\x00\x00\x00\x0cJXL \r\n\x87\n":
		return ".jxl"
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0x0A:
		return ".jxl"
	case len(data) >= 4 && (string(data[0:4]) == "II*\x00" || string(data[0:4]) == "MM\x00*"):
		return ".tiff"
	case len(data) >= 4 && string(data[0:4]) == "8BPS":
		return ".psd"
	case len(data) >= 4 && string(data[0:4]) == "\x00\x00\x01\x00":
		return ".ico"
	case len(data) >= 8 && string(data[0:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return ".jpg"
	case len(data) >= 6 && (string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a"):
		return ".gif"
	case looksLikeSVG(data):
		return ".svg"
	}
	return ""
}

// looksLikeSVG tells a drawing from the other things that begin with a tag.
func looksLikeSVG(data []byte) bool {
	if len(data) > 512 {
		data = data[:512]
	}
	return bytes.Contains(bytes.ToLower(data), []byte("<svg"))
}

// registerExternalImageDecoder adds the converter to the registry when there
// is one to add, and takes it out again when there is not. Registering it
// unconditionally would make IsImageFile say yes to a webp on a machine that
// cannot open it, and the viewer would open on an error message.
func registerExternalImageDecoder() bool {
	resetInstalledExternalImageTools()
	installed := installedExternalImageToolsLookup()
	seenExt := make(map[string]struct{})
	var formats []string
	for _, tool := range externalImageTools {
		if _, ok := installed[tool.Bin]; !ok {
			continue
		}
		for _, f := range tool.Formats {
			if _, ok := seenExt[f]; ok {
				continue
			}
			seenExt[f] = struct{}{}
			formats = append(formats, f)
		}
	}
	if len(formats) == 0 {
		UnregisterImageDecoder(externalImageDecoder)
		return false
	}
	RegisterImageDecoder(ImageDecoder{
		Name:       externalImageDecoder,
		Priority:   externalImagePriority,
		Extensions: formats,
		DecodeCtx:  decodeImageExternally,
		Label:      externalImageToolLabel,
	})
	return true
}

func init() {
	registerExternalImageDecoder()
}
