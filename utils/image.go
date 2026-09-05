package utils

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

// imageTokenPrefix marks the placeholder injected in place of markdown
// images before glamour rendering. Tokens are alphanumeric only so glamour
// cannot autolink, emphasize, or line-wrap them.
const imageTokenPrefix = "GLOWIMGTOKEN"

// imagePattern matches inline-form markdown images: ![alt](src "title").
var imagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// fencePattern matches an opening or closing code fence.
var fencePattern = regexp.MustCompile("^\\s*(`{3,}|~{3,})")

// ImageOptions controls how image placeholders are turned into terminal art.
type ImageOptions struct {
	// BaseDir resolves relative image paths; empty means the working
	// directory, or the document URL directory for remote documents.
	BaseDir string
	// Width is the maximum art width in terminal columns.
	Width int
	// MaxRows is the maximum art height in terminal rows; zero means the
	// image is only constrained by Width.
	MaxRows int
	// ColorMode is the chafa -c value: none, 16, 256, or full.
	ColorMode string
}

// InjectImageTokens replaces inline-form markdown images with unique
// placeholder tokens on their own paragraph lines and returns the rewritten
// document together with the image sources and alt texts in token order.
// Images inside fenced code blocks or inline code spans are left untouched.
func InjectImageTokens(md string) (string, []string, []string) {
	lines := strings.Split(md, "\n")
	inCode := false
	fenceChar := byte(0)
	fenceLen := 0
	var srcs, alts []string

	for i, line := range lines {
		if f := fencePattern.FindStringSubmatch(line); f != nil {
			if !inCode {
				inCode = true
				fenceChar = f[1][0]
				fenceLen = len(f[1])
			} else if f[1][0] == fenceChar && len(f[1]) >= fenceLen &&
				strings.TrimSpace(line[strings.LastIndex(line, f[1])+len(f[1]):]) == "" {
				inCode = false
			}
			continue
		}
		if inCode {
			continue
		}

		pos := 0
		for {
			m := imagePattern.FindStringSubmatchIndex(line[pos:])
			if m == nil {
				break
			}
			start, end := pos+m[0], pos+m[1]
			src := line[pos+m[4] : pos+m[5]]
			alt := line[pos+m[2] : pos+m[3]]

			// Skip matches inside inline code spans and unsupported forms.
			if strings.Count(line[:start], "`")%2 == 1 || src == "" ||
				strings.HasPrefix(src, "data:") {
				pos = end
				continue
			}

			token := imageTokenPrefix + strconv.Itoa(len(srcs))
			srcs = append(srcs, src)
			alts = append(alts, alt)
			line = line[:start] + "\n\n" + token + "\n\n" + line[end:]
			pos = start + len(token) + 4
		}
		lines[i] = line
	}

	return strings.Join(lines, "\n"), srcs, alts
}

// ReplaceImageTokens post-processes glamour output: every line containing an
// image token is replaced by the chafa character art for that image,
// prefixed with the line's leading indentation. When chafa is unavailable
// the output is returned unchanged; when a single image fails, its line
// falls back to the alt text.
func ReplaceImageTokens(rendered string, srcs, alts []string, opts ImageOptions) string {
	if len(srcs) == 0 || !chafaAvailable() {
		return rendered
	}
	tokenRe := regexp.MustCompile(imageTokenPrefix + `(\d+)`)

	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		m := tokenRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx < 0 || idx >= len(srcs) {
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		art, err := renderChafa(srcs[idx], opts)
		if err != nil {
			fallback := srcs[idx]
			if idx < len(alts) && alts[idx] != "" {
				fallback = alts[idx]
			}
			lines[i] = indent + fallback
			continue
		}

		artLines := strings.Split(art, "\n")
		for j := range artLines {
			artLines[j] = indent + artLines[j]
		}
		lines[i] = strings.Join(artLines, "\n")
	}
	return strings.Join(lines, "\n")
}

// ChafaColorMode returns the chafa -c value for the current terminal. A
// truecolor terminal gets 24-bit art; anything piped (or without color
// support) stays at 256 colors or fewer. The detected profile describes
// the terminal glow was started from, not whatever reads the far end of
// a pipe, so truecolor is used only when that terminal is the stdout
// glow writes to.
var ChafaColorMode = sync.OnceValue(func() string {
	switch colorprofile.Detect(os.Stdout, os.Environ()) {
	case colorprofile.NoTTY, colorprofile.Ascii:
		return "none"
	case colorprofile.TrueColor:
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return "full"
		}
		return "256"
	default:
		return "256"
	}
})

var (
	chafaBin  string
	chafaOnce sync.Once
	lookChafa = exec.LookPath
)

func chafaAvailable() bool {
	chafaOnce.Do(func() { chafaBin, _ = lookChafa("chafa") })
	return chafaBin != ""
}

type artKey struct {
	path      string
	width     int
	maxRows   int
	colorMode string
}

var artCache sync.Map

var httpClient = &http.Client{Timeout: 30 * time.Second}

// expandSGRState rewrites differential SGR updates in chafa art into
// fully specified color escapes. Chafa relies on terminal state
// persisting between escapes: a cell that changes only its foreground
// sends "\e[38;5;Nm" and keeps the previous cell's background.
// Stateless consumers (pagers, editors such as vim's AnsiEsc) render
// each escape independently and lose the carried attribute, so every
// color escape that omits an attribute gets the carried value
// materialized. Resets (0, and the implied-parameter "\e[m") and
// default restores (39, 49) clear the carried state and pass through
// unchanged.
//
// Precondition: the input holds indexed color only. The caller gates on
// ColorMode != "full" because truecolor triples are not parsed here,
// and bare 16-color codes (chafa -c 16) are neither parsed nor
// carried.
func expandSGRState(s string) string {
	fg, bg := "", ""
	var b strings.Builder
	i := 0
	for i < len(s) {
		loc := sgrRe.FindStringSubmatchIndex(s[i:])
		if loc == nil {
			b.WriteString(s[i:])
			break
		}
		start, end := i+loc[0], i+loc[1]
		b.WriteString(s[i:start])
		body := s[i+loc[2] : i+loc[3]]
		codes := strings.Split(body, ";")
		fgSeen, bgSeen := false, false
		skip := 0
		for _, c := range codes {
			switch {
			case skip == 38 && c == "5":
				skip = 385
			case skip == 385:
				fg, fgSeen = c, true
				skip = 0
			case skip == 48 && c == "5":
				skip = 485
			case skip == 485:
				bg, bgSeen = c, true
				skip = 0
			case skip != 0:
				// truecolor triples are excluded by the caller's
				// 256-color gate; anything else here is malformed
				skip = 0
			case c == "38":
				skip = 38
			case c == "48":
				skip = 48
			case c == "0" || c == "":
				fg, bg = "", ""
			case c == "39":
				fg = ""
			case c == "49":
				bg = ""
			}
		}
		rewrite := ""
		if !fgSeen && fg != "" {
			rewrite += ";38;5;" + fg
		}
		if !bgSeen && bg != "" {
			rewrite += ";48;5;" + bg
		}
		if rewrite != "" {
			b.WriteString("\x1b[" + body + rewrite + "m")
		} else {
			b.WriteString(s[start:end])
		}
		i = end
	}
	return b.String()
}

var sgrRe = regexp.MustCompile("\x1b\\[([0-9;]*)m")

func renderChafa(src string, opts ImageOptions) (string, error) {
	path, err := resolveImageSource(src, opts.BaseDir)
	if err != nil {
		return "", err
	}

	width := opts.Width
	if width <= 0 {
		width = 80
	}
	// chafa fits the image inside the given box, preserving aspect ratio.
	size := strconv.Itoa(width) + "x"
	if opts.MaxRows > 0 {
		size += strconv.Itoa(opts.MaxRows)
	}
	key := artKey{path, width, opts.MaxRows, opts.ColorMode}
	if v, ok := artCache.Load(key); ok {
		return v.(string), nil
	}

	cmd := exec.Command(chafaBin,
		"-f", "symbols",
		"--polite", "on",
		"--animate", "off",
		// Fill symbols add intra-cell gradient detail; measured ~2%
		// closer to the source with both 256 and truecolor output.
		"--fill", "block,half,quad,stipple",
		"-c", opts.ColorMode,
		"-s", size,
		path,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("chafa failed for %s: %w: %s", src, err, strings.TrimSpace(stderr.String()))
	}

	art := strings.TrimSuffix(string(out), "\n")
	if art == "" {
		return "", fmt.Errorf("chafa produced no output for %s", src)
	}
	// Expansion is only needed for stateless consumers, which see the
	// piped art capped at 256 colors by ChafaColorMode. A truecolor TTY
	// renders the differential escapes itself, and expandSGRState does
	// not parse truecolor triples.
	if opts.ColorMode != "full" {
		art = expandSGRState(art)
	}
	artCache.Store(key, art)
	return art, nil
}

func resolveImageSource(src, baseDir string) (string, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return fetchToCache(src)
	}
	if strings.HasPrefix(baseDir, "http://") || strings.HasPrefix(baseDir, "https://") {
		if u, err := url.Parse(baseDir); err == nil {
			if r, err := u.Parse(src); err == nil {
				return fetchToCache(r.String())
			}
		}
		return "", fmt.Errorf("cannot resolve image %q against %q", src, baseDir)
	}
	if filepath.IsAbs(src) {
		return statImage(src)
	}
	dir := baseDir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	return statImage(filepath.Join(dir, src))
}

func statImage(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	return path, nil
}

func imageCacheDir() string {
	if d := os.Getenv("GLOW_IMAGE_CACHE_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "glow-image-cache")
}

// fetchToCache downloads a remote image into the cache directory and returns
// the local path. Cached files are keyed by the URL hash and reused.
func fetchToCache(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(rawURL))
	name := hex.EncodeToString(sum[:])
	// The extension is cosmetic: chafa sniffs the content, so an unknown
	// type simply keeps the cache name extensionless.
	ext := filepath.Ext(u.Path)
	if len(ext) > 6 || strings.ContainsAny(ext, "?#") {
		ext = ""
	}
	dst := filepath.Join(imageCacheDir(), name+ext)

	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst, nil
	}
	if err := os.MkdirAll(imageCacheDir(), 0o755); err != nil {
		return "", err
	}

	resp, err := httpClient.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}

	f, err := os.CreateTemp(imageCacheDir(), ".dl-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	return dst, nil
}
