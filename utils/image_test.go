package utils

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestInjectImageTokens(t *testing.T) {
	tt := []struct {
		name      string
		input     string
		wantSrcs  []string
		wantNoTok bool
	}{
		{
			name:     "single image",
			input:    "hello\n\n![alt text](img.png)\n\nbye",
			wantSrcs: []string{"img.png"},
		},
		{
			name:     "multiple images",
			input:    "![a](1.png) text ![b](2.jpg)",
			wantSrcs: []string{"1.png", "2.jpg"},
		},
		{
			name:      "fenced code block",
			input:     "```go\n![x](not-an-image.png)\n```",
			wantNoTok: true,
		},
		{
			name:      "tilde fenced block",
			input:     "~~~\n![x](not-an-image.png)\n~~~",
			wantNoTok: true,
		},
		{
			name:      "inline code span",
			input:     "see `![x](no.png)` here",
			wantNoTok: true,
		},
		{
			name:      "reference-style untouched",
			input:     "![alt][ref]\n\n[ref]: img.png",
			wantNoTok: true,
		},
		{
			name:      "data URI skipped",
			input:     "![x](data:image/png;base64,AAAA)",
			wantNoTok: true,
		},
		{
			name:     "image with title",
			input:    `![a](img.png "the title")`,
			wantSrcs: []string{"img.png"},
		},
		{
			name:     "image inside link",
			input:    "[![a](thumb.png)](https://example.com)",
			wantSrcs: []string{"thumb.png"},
		},
		{
			name:     "code block before image",
			input:    "```\nfence\n```\n\n![a](real.png)",
			wantSrcs: []string{"real.png"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			out, srcs, alts := InjectImageTokens(tc.input)
			if len(srcs) != len(tc.wantSrcs) {
				t.Fatalf("got srcs %v, want %v", srcs, tc.wantSrcs)
			}
			for i := range srcs {
				if srcs[i] != tc.wantSrcs[i] {
					t.Errorf("srcs[%d] = %q, want %q", i, srcs[i], tc.wantSrcs[i])
				}
				if alts[i] == "" {
					t.Errorf("alts[%d] is empty", i)
				}
			}
			if tc.wantNoTok && strings.Contains(out, imageTokenPrefix) {
				t.Errorf("unexpected token in output:\n%s", out)
			}
			if !tc.wantNoTok {
				for i := range srcs {
					want := imageTokenPrefix + string(rune('0'+i))
					if !strings.Contains(out, want) {
						t.Errorf("missing token %s in output:\n%s", want, out)
					}
				}
			}
		})
	}
}

func TestReplaceImageTokensPassthrough(t *testing.T) {
	lookChafa = func(string) (string, error) { return "", exec.ErrNotFound }
	chafaOnce = sync.Once{}

	injected, srcs, alts := InjectImageTokens("![a](img.png)")
	rendered := "some\n" + imageTokenPrefix + "0\n" + "output"
	out := ReplaceImageTokens(rendered, srcs, alts, ImageOptions{ColorMode: "256"})
	if out != rendered {
		t.Errorf("expected passthrough without chafa, got:\n%s", out)
	}
	_ = injected
}

func TestReplaceImageTokensFallbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "chafa")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lookChafa = func(string) (string, error) { return fakeBin, nil }
	chafaOnce = sync.Once{}

	injected, srcs, alts := InjectImageTokens("![pretty picture](missing.png)")
	out := ReplaceImageTokens("  "+injected, srcs, alts, ImageOptions{ColorMode: "none"})
	if !strings.Contains(out, "pretty picture") {
		t.Errorf("expected alt text fallback, got:\n%s", out)
	}
	if strings.Contains(out, imageTokenPrefix) {
		t.Errorf("token not replaced:\n%s", out)
	}
}

func TestReplaceImageTokensIndentAndArt(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "one-pixel.png")
	// 1x1 transparent PNG
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(img, png, 0o644); err != nil {
		t.Fatal(err)
	}
	lookChafa = func(string) (string, error) { return "chafa", nil }
	chafaOnce = sync.Once{}

	_, srcs, alts := InjectImageTokens("![p](" + img + ")")
	out := ReplaceImageTokens("    "+imageTokenPrefix+"0", srcs, alts,
		ImageOptions{BaseDir: dir, Width: 20, ColorMode: "none"})
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Errorf("art line lost indentation: %q", line)
		}
	}
	if strings.Contains(out, imageTokenPrefix) {
		t.Errorf("token not replaced:\n%s", out)
	}
}

func TestReplaceImageTokensMaxRows(t *testing.T) {
	img := filepath.Join("..", "example.png") // 1984x2548, tall
	lookChafa = func(string) (string, error) { return "chafa", nil }
	chafaOnce = sync.Once{}

	_, srcs, alts := InjectImageTokens("![p](" + img + ")")

	capped := strings.Split(ReplaceImageTokens(imageTokenPrefix+"0", srcs, alts,
		ImageOptions{Width: 60, MaxRows: 20, ColorMode: "none"}), "\n")
	if len(capped) > 20 {
		t.Errorf("art height %d exceeds MaxRows 20", len(capped))
	}
	if len(capped) < 2 {
		t.Errorf("expected art, got %q", capped)
	}

	uncapped := strings.Split(ReplaceImageTokens(imageTokenPrefix+"0", srcs, alts,
		ImageOptions{Width: 60, MaxRows: 0, ColorMode: "none"}), "\n")
	if len(uncapped) <= len(capped) {
		t.Errorf("MaxRows 0 should not cap height: %d vs %d", len(uncapped), len(capped))
	}
}

func TestFetchToCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLOW_IMAGE_CACHE_DIR", dir)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/missing.png" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("image-bytes"))
	}))
	defer srv.Close()

	got, err := fetchToCache(srv.URL + "/img/pic.png")
	if err != nil {
		t.Fatalf("fetchToCache: %v", err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("cached file unreadable: %v", err)
	}
	if string(data) != "image-bytes" {
		t.Errorf("cached content = %q, want %q", data, "image-bytes")
	}

	// A second fetch must be served from the cache, not the network.
	again, err := fetchToCache(srv.URL + "/img/pic.png")
	if err != nil {
		t.Fatalf("second fetchToCache: %v", err)
	}
	if again != got || hits != 1 {
		t.Errorf("expected cache reuse: path %q vs %q, hits %d", again, got, hits)
	}

	if _, err := fetchToCache(srv.URL + "/missing.png"); err == nil {
		t.Error("expected an error for a non-200 response")
	}
}

func TestExpandSGRState(t *testing.T) {
	tt := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fully specified cells pass through",
			in:   "\x1b[38;5;237;48;5;100mA\x1b[38;5;8;48;5;100mB\x1b[0m",
			want: "\x1b[38;5;237;48;5;100mA\x1b[38;5;8;48;5;100mB\x1b[0m",
		},
		{
			name: "fg-only update inherits carried background",
			in:   "\x1b[38;5;237;48;5;100mA\x1b[38;5;8mB\x1b[0m",
			want: "\x1b[38;5;237;48;5;100mA\x1b[38;5;8;48;5;100mB\x1b[0m",
		},
		{
			name: "bg-only update inherits carried foreground",
			in:   "\x1b[38;5;237;48;5;100mA\x1b[48;5;200mB\x1b[0m",
			want: "\x1b[38;5;237;48;5;100mA\x1b[48;5;200;38;5;237mB\x1b[0m",
		},
		{
			name: "empty parameter is an implied reset",
			in:   "\x1b[38;5;1;48;5;2mA\x1b[m\x1b[38;5;9mB\x1b[0m",
			want: "\x1b[38;5;1;48;5;2mA\x1b[m\x1b[38;5;9mB\x1b[0m",
		},
		{
			name: "reset clears the carried state",
			in:   "\x1b[38;5;237;48;5;100mA\x1b[0m\x1b[38;5;8mB\x1b[0m",
			want: "\x1b[38;5;237;48;5;100mA\x1b[0m\x1b[38;5;8mB\x1b[0m",
		},
		{
			name: "49 restores the default background, fg carries",
			in:   "\x1b[38;5;237;48;5;100mA\x1b[49mB\x1b[0m",
			want: "\x1b[38;5;237;48;5;100mA\x1b[49;38;5;237mB\x1b[0m",
		},
		{
			name: "text without color escapes passes through",
			in:   "plain text\x1b[1mbold\x1b[0m",
			want: "plain text\x1b[1mbold\x1b[0m",
		},
	}
	for _, tc := range tt {
		if got := expandSGRState(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
