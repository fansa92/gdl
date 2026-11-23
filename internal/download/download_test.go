package download

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var modTime = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// newServer 起一个支持 Range / HEAD / If-Modified-Since 的测试服务器。
func newServer(t *testing.T, data []byte, disposition string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v1"`)
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			if ts, err := http.ParseTime(ims); err == nil && !modTime.After(ts) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		http.ServeContent(w, r, "file.bin", modTime, bytes.NewReader(data))
	}))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 13)
	}
	return b
}

func quietOpts(url, dir string) Options {
	return Options{
		URL:         url,
		OutputDir:   dir,
		Connections: 4,
		Quiet:       true,
		Retries:     1,
		Timeout:     5 * time.Second,
	}
}

func TestChunkedDownload(t *testing.T) {
	data := randBytes(300 * 1024)
	srv := newServer(t, data, "")
	defer srv.Close()

	dir := t.TempDir()
	res, err := Download(context.Background(), quietOpts(srv.URL+"/file.bin", dir))
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("大小不符: %d != %d", res.Size, len(data))
	}
	got, err := os.ReadFile(filepath.Join(dir, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("内容不一致")
	}
	if _, err := os.Stat(filepath.Join(dir, "file.bin.part.json")); !os.IsNotExist(err) {
		t.Fatal("断点状态文件未清理")
	}
}

func TestContentDispositionName(t *testing.T) {
	data := randBytes(1024)
	srv := newServer(t, data, `attachment; filename="server-name.bin"`)
	defer srv.Close()

	dir := t.TempDir()
	res, err := Download(context.Background(), quietOpts(srv.URL+"/url-name.bin", dir))
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	want := filepath.Join(dir, "server-name.bin")
	if res.Path != want {
		t.Fatalf("路径未采用 Content-Disposition: %q != %q", res.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("文件未按服务端文件名落盘: %v", err)
	}
}

func TestConditionalNotModified(t *testing.T) {
	data := randBytes(2048)
	srv := newServer(t, data, "")
	defer srv.Close()

	dir := t.TempDir()
	opt := quietOpts(srv.URL+"/file.bin", dir)
	opt.Conditional = true
	if _, err := Download(context.Background(), opt); err != nil {
		t.Fatalf("首次下载失败: %v", err)
	}

	// 第二次：本地 mtime 已被回写成服务端时间，应命中 304
	res, err := Download(context.Background(), opt)
	if err != nil {
		t.Fatalf("条件请求失败: %v", err)
	}
	if !res.NotModified {
		t.Fatalf("期望 304 未修改，实际 status=%d", res.StatusCode)
	}
}

func TestNoClobberAndExists(t *testing.T) {
	data := randBytes(64)
	srv := newServer(t, data, "")
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	opt := quietOpts(srv.URL+"/file.bin", dir)
	if _, err := Download(context.Background(), opt); ExitCode(err) != ExitExists {
		t.Fatalf("期望 ExitExists，实际 %v (code=%d)", err, ExitCode(err))
	}

	opt.NoClobber = true
	res, err := Download(context.Background(), opt)
	if err != nil {
		t.Fatalf("跳过模式不应报错: %v", err)
	}
	if !res.Skipped {
		t.Fatal("期望 skipped")
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatal("跳过模式下不应改动文件")
	}
}

func TestStdout(t *testing.T) {
	data := randBytes(5000)
	srv := newServer(t, data, "")
	defer srv.Close()

	var buf bytes.Buffer
	opt := Options{
		URL:          srv.URL + "/file.bin",
		Stdout:       true,
		StdoutWriter: &buf,
		Quiet:        true,
		Timeout:      5 * time.Second,
		Sum:          "sha256",
	}
	res, err := Download(context.Background(), opt)
	if err != nil {
		t.Fatalf("管道模式失败: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("stdout 内容不一致: %d vs %d", buf.Len(), len(data))
	}
	if res.Sum == "" {
		t.Fatal("管道模式应输出哈希")
	}
}

func TestMaxFilesize(t *testing.T) {
	data := randBytes(4096)
	srv := newServer(t, data, "")
	defer srv.Close()

	opt := quietOpts(srv.URL+"/file.bin", t.TempDir())
	opt.MaxFilesize = 1000
	if _, err := Download(context.Background(), opt); ExitCode(err) != ExitLimit {
		t.Fatalf("期望 ExitLimit，实际 %v (code=%d)", err, ExitCode(err))
	}
}

func TestResumeFromState(t *testing.T) {
	data := randBytes(2000)
	srv := newServer(t, data, "")
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "file.bin")
	part := target + ".part"
	if err := os.WriteFile(part, data[:800], 0o644); err != nil {
		t.Fatal(err)
	}
	st := state{
		URL:       srv.URL + "/file.bin",
		Size:      int64(len(data)),
		ETag:      `"v1"`,
		Modified:  modTime.Format(http.TimeFormat),
		Chunks:    []chunk{{Start: 0, End: int64(len(data)) - 1, Done: 800}},
		UpdatedAt: time.Now(),
	}
	raw, _ := json.Marshal(&st)
	if err := os.WriteFile(part+".json", raw, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Download(context.Background(), quietOpts(srv.URL+"/file.bin", dir))
	if err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if !res.Resumed {
		t.Fatal("期望识别为断点续传")
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Fatalf("续传后内容不一致（%d 字节）", len(got))
	}
}

func TestAuthAndCookieHeaders(t *testing.T) {
	var gotAuth, gotCookie, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Length", "2")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	opt := quietOpts(srv.URL+"/f.txt", t.TempDir())
	opt.User = "alice"
	opt.Password = "s3cret"
	opt.Cookie = "a=b"
	opt.UserAgent = "gdl-test"
	if _, err := Download(context.Background(), opt); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if want := "Basic YWxpY2U6czNjcmV0"; gotAuth != want {
		t.Fatalf("Basic 认证头不符: %q != %q", gotAuth, want)
	}
	if gotCookie != "a=b" {
		t.Fatalf("Cookie 头不符: %q", gotCookie)
	}
	if gotUA != "gdl-test" {
		t.Fatalf("UA 不符: %q", gotUA)
	}

	opt.User, opt.Password, opt.Bearer = "", "", "tok123"
	opt.OutputDir = t.TempDir()
	if _, err := Download(context.Background(), opt); err != nil {
		t.Fatalf("第二次下载失败: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Bearer 头不符: %q", gotAuth)
	}
}

func TestMaxRedirs(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	opt := quietOpts(srv.URL+"/loop", t.TempDir())
	opt.MaxRedirs = 3
	if _, err := Download(context.Background(), opt); ExitCode(err) != ExitNetwork {
		t.Fatalf("期望 ExitNetwork，实际 %v (code=%d)", err, ExitCode(err))
	}
}

func TestRangeUnsupported(t *testing.T) {
	data := randBytes(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	opt := quietOpts(srv.URL+"/file.bin", dir)
	if _, err := Download(context.Background(), opt); err != nil {
		t.Fatalf("不支持 Range 的服务器应回退单流: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "file.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("回退单流后内容不一致")
	}
}
