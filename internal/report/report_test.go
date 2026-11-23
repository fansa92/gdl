package report

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func sample() Fields {
	return Fields{
		URL:      "https://example.com/f.zip",
		Path:     "f.zip",
		Size:     2048,
		Duration: 2 * time.Second,
		Status:   200,
		Sum:      "sha256:ab",
	}
}

func TestFormatTokens(t *testing.T) {
	got := Format(`%{http_code} %{size_download} %{speed_download} %{filename_effective}`, sample())
	want := "200 2048 1024 f.zip"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
}

func TestFormatEscapes(t *testing.T) {
	got := Format(`a\tb\nc\\d %%{http_code}`, sample())
	want := "a\tb\nc\\d %{http_code}"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
	if strings.Contains(Format(`\n`, sample()), `\`) {
		t.Fatal("换行转义失败")
	}
}

func TestFormatUnknownToken(t *testing.T) {
	if got := Format(`%{nope}`, sample()); got != "%{nope}" {
		t.Fatalf("未知占位符应原样输出，实际 %q", got)
	}
}

func TestFormatJSONAndError(t *testing.T) {
	f := sample()
	f.Err = errors.New("boom")
	f.ExitCode = 5
	out := Format("%{json}", f)
	if !strings.Contains(out, `"http_code":200`) || !strings.Contains(out, `"errormsg":"boom"`) {
		t.Fatalf("json 输出不符: %s", out)
	}
}
