package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// info 是探测（HEAD / Range 探测）得到的远端资源信息。
type info struct {
	size        int64 // -1 表示未知
	ranges      bool
	etag        string
	modified    string
	name        string
	notModified bool // 条件请求命中 304
}

// newClient 构造下载用的 HTTP 客户端，支持代理、TLS、HTTP/2、限速。
func (d *downloader) newClient() *http.Client {
	to := d.opt.Timeout
	n := d.opt.Connections * 2
	if n < 8 {
		n = 8
	}

	// 基础 Transport
	baseTr := &http.Transport{
		DialContext:           (&dialer{timeout: to, ipv: d.opt.IPVersion, iface: d.opt.Interface}).dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   to,
		ResponseHeaderTimeout: to,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          n,
		MaxIdleConnsPerHost:   n,
		IdleConnTimeout:       90 * time.Second,
	}

	// 代理
	if d.opt.Proxy != "" {
		baseTr.Proxy = d.proxyFunc()
		d.configureSocks5Dial(baseTr)
	}

	// TLS 配置
	if d.opt.Insecure || d.opt.CACert != "" || d.opt.CertFile != "" {
		tlsConfig := &tls.Config{InsecureSkipVerify: d.opt.Insecure}
		if d.opt.CACert != "" {
			if cert, err := os.ReadFile(d.opt.CACert); err == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(cert)
				tlsConfig.RootCAs = pool
			}
		}
		// 客户端证书（双向 TLS）
		if d.opt.CertFile != "" {
			keyFile := d.opt.KeyFile
			if keyFile == "" {
				keyFile = d.opt.CertFile
			}
			if pair, err := tls.LoadX509KeyPair(d.opt.CertFile, keyFile); err == nil {
				tlsConfig.Certificates = []tls.Certificate{pair}
			}
		}
		baseTr.TLSClientConfig = tlsConfig
	}

	// HTTP/2 显式控制
	if d.opt.HTTP2 != nil {
		if *d.opt.HTTP2 {
			baseTr.ForceAttemptHTTP2 = true
		} else {
			baseTr.ForceAttemptHTTP2 = false
			// 彻底禁用 h2
			baseTr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		}
	}

	var rt http.RoundTripper = baseTr

	// 限速包装器
	if d.opt.RateLimit != "" {
		if rl, err := parseRateLimit(d.opt.RateLimit); err == nil && rl > 0 {
			rt = &rateLimitedTransport{Base: baseTr, limiter: newTokenBucket(rl)}
		}
	}

	maxRedirs := d.opt.MaxRedirs
	if maxRedirs == 0 {
		maxRedirs = 10
	}

	client := &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if maxRedirs > 0 && len(via) >= maxRedirs {
				return Errorf(ExitNetwork, "重定向次数超过 %d 次", maxRedirs)
			}
			// 跨域重定向时不把认证信息带过去
			if len(via) > 0 && via[0].URL.Host != req.URL.Host {
				req.Header.Del("Authorization")
				return nil
			}
			for k, vs := range d.header {
				if _, ok := req.Header[k]; !ok {
					req.Header[k] = vs
				}
			}
			return nil
		},
	}
	if d.opt.Jar != nil {
		client.Jar = d.opt.Jar
	}
	return client
}

// proxyFunc 解析代理 URL 返回 ProxyFunc
func (d *downloader) proxyFunc() func(*http.Request) (*url.URL, error) {
	proxyURL, err := url.Parse(d.opt.Proxy)
	if err != nil {
		return http.ProxyFromEnvironment
	}
	switch proxyURL.Scheme {
	case "http", "https":
		return http.ProxyURL(proxyURL)
	case "socks5", "socks5h":
		// socks5 使用自定义 DialContext，这里返回 nil 让 Transport 使用 DialContext
		return func(*http.Request) (*url.URL, error) {
			return nil, nil
		}
	}
	return http.ProxyFromEnvironment
}

// 为 socks5 替换 DialContext
func (d *downloader) configureSocks5Dial(tr *http.Transport) {
	if d.opt.Proxy == "" {
		return
	}
	proxyURL, err := url.Parse(d.opt.Proxy)
	if err != nil || !strings.HasPrefix(proxyURL.Scheme, "socks5") {
		return
	}
	// 创建一个实现 proxy.Dialer 接口的适配器
	baseDialer := &net.Dialer{Timeout: d.opt.Timeout, KeepAlive: 30 * time.Second}
	dialer, err := proxy.FromURL(proxyURL, baseDialer)
	if err == nil {
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			tr.DialContext = ctxDialer.DialContext
		} else {
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	}
}

// newRequest 构造一个带自定义请求头、认证与 cookie 的请求。
func (d *downloader) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range d.header {
		req.Header[k] = append([]string(nil), vs...)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", d.opt.UserAgent)
	}
	if req.Header.Get("Authorization") == "" {
		switch {
		case d.opt.Bearer != "":
			req.Header.Set("Authorization", "Bearer "+d.opt.Bearer)
		case d.opt.User != "":
			req.SetBasicAuth(d.opt.User, d.opt.Password)
		}
	}
	if d.opt.Cookie != "" && req.Header.Get("Cookie") == "" {
		req.Header.Set("Cookie", d.opt.Cookie)
	}
	return req, nil
}

// applyConditional 给请求加上 If-Modified-Since（-N）。
func (d *downloader) applyConditional(req *http.Request) {
	if d.condSince.IsZero() {
		return
	}
	req.Header.Set("If-Modified-Since", d.condSince.UTC().Format(http.TimeFormat))
}

// probe 探测远端资源的大小、是否支持 Range、校验标记和文件名。
func (d *downloader) probe(ctx context.Context, target string) (*info, error) {
	inf := &info{size: -1}

	// 先尝试 HEAD，多数服务器会返回完整元信息
	if req, err := d.newRequest(ctx, http.MethodHead, target); err == nil {
		d.applyConditional(req)
		if resp, err := d.client.Do(req); err == nil {
			io.CopyN(io.Discard, resp.Body, 1)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				fillInfo(inf, resp)
			case http.StatusNotModified:
				inf.notModified = true
				d.status.Store(http.StatusNotModified)
				fillInfo(inf, resp)
				return inf, nil
			}
		}
	}
	if inf.size >= 0 && inf.ranges {
		return inf, nil
	}

	// 用一个 1 字节的 Range 请求确认是否支持断点，并补齐大小
	req, err := d.newRequest(ctx, http.MethodGet, target)
	if err != nil {
		return nil, err
	}
	d.applyConditional(req)
	if inf.size >= 0 {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		if inf.size >= 0 {
			return inf, nil // HEAD 已经拿到大小，探测失败不致命
		}
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		inf.notModified = true
		d.status.Store(http.StatusNotModified)
		fillInfo(inf, resp)
		return inf, nil
	case http.StatusPartialContent:
		inf.ranges = true
		if _, _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && total >= 0 {
			inf.size = total
		}
		fillInfo(inf, resp)
	case http.StatusOK:
		inf.ranges = false
		if cl := resp.ContentLength; cl >= 0 {
			inf.size = cl
		}
		fillInfo(inf, resp)
	default:
		if inf.size >= 0 {
			return inf, nil
		}
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	io.CopyN(io.Discard, resp.Body, 1)
	return inf, nil
}

// fillInfo 从响应头里提取元信息（只在对应字段为空时写入）。
func fillInfo(inf *info, resp *http.Response) {
	h := resp.Header
	if inf.size < 0 {
		if cl := h.Get("Content-Length"); cl != "" {
			var n int64
			if _, err := fmt.Sscan(cl, &n); err == nil && n >= 0 {
				inf.size = n
			}
		}
	}
	if strings.Contains(strings.ToLower(h.Get("Accept-Ranges")), "bytes") {
		inf.ranges = true
	}
	if inf.etag == "" {
		inf.etag = h.Get("Etag")
	}
	if inf.modified == "" {
		inf.modified = h.Get("Last-Modified")
	}
	if inf.name == "" {
		inf.name = filenameFromDisposition(h.Get("Content-Disposition"))
	}
	if inf.name == "" && resp.Request != nil && resp.Request.URL != nil {
		inf.name = filenameFromURL(resp.Request.URL)
	}
}

// parseContentRange 解析 "bytes 0-0/12345"，返回起止和总长度（未知时为 -1）。
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(strings.ToLower(v), "bytes") {
		return 0, 0, 0, false
	}
	v = strings.TrimSpace(v[len("bytes"):])
	slash := strings.IndexByte(v, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}
	rng := strings.TrimSpace(v[:slash])
	totalStr := strings.TrimSpace(v[slash+1:])
	dash := strings.IndexByte(rng, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	if _, err := fmt.Sscan(rng[:dash], &start); err != nil {
		return 0, 0, 0, false
	}
	if _, err := fmt.Sscan(rng[dash+1:], &end); err != nil {
		return 0, 0, 0, false
	}
	if totalStr == "*" {
		return start, end, -1, true
	}
	if _, err := fmt.Sscan(totalStr, &total); err != nil || total < 0 {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

// filenameFromDisposition 从 Content-Disposition 解析文件名。
func filenameFromDisposition(v string) string {
	if v == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(v); err == nil {
		if n := params["filename"]; n != "" {
			return sanitizeName(n)
		}
		if n := params["filename*"]; n != "" {
			if dec := decodeRFC5987(n); dec != "" {
				return sanitizeName(dec)
			}
		}
	}
	return ""
}

// decodeRFC5987 处理形如 UTF-8”%E4%B8%AD%E6%96%87.zip 的值。
func decodeRFC5987(v string) string {
	parts := strings.SplitN(v, "''", 2)
	if len(parts) != 2 {
		return v
	}
	if dec, err := url.PathUnescape(parts[1]); err == nil {
		return dec
	}
	return parts[1]
}

// filenameFromURL 从 URL 路径推断文件名。
func filenameFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	p := u.Path
	if p == "" || p == "/" {
		return ""
	}
	return sanitizeName(path.Base(p))
}

// sanitizeName 去掉路径成分和 Windows 上非法的文件名字符。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" {
		return ""
	}
	return out
}

// ParseSize 解析 "5M" / "100k" / "1G" / 纯数字为字节数（大小与限速共用）。
func ParseSize(s string) (int64, error) {
	n, err := parseRateLimit(s)
	if err != nil {
		return 0, Errorf(ExitUsage, "无法解析大小 %q（示例：100k / 5M / 1G）", s)
	}
	return n, nil
}

// parseRateLimit 解析 "5M", "100k", "1G" 等为字节/秒
func parseRateLimit(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult = 1024
		s = strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "G")
	}
	var n float64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}

// tokenBucket 令牌桶限速
type tokenBucket struct {
	rate   int64 // bytes/sec
	tokens float64
	last   time.Time
	mu     sync.Mutex
}

func newTokenBucket(rate int64) *tokenBucket {
	return &tokenBucket{rate: rate, tokens: float64(rate), last: time.Now()}
}

func (tb *tokenBucket) Take(n int64) time.Duration {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.last).Seconds()
	tb.last = now
	tb.tokens += elapsed * float64(tb.rate)
	if tb.tokens > float64(tb.rate) {
		tb.tokens = float64(tb.rate)
	}
	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		return 0
	}
	need := float64(n) - tb.tokens
	tb.tokens = 0
	return time.Duration(need / float64(tb.rate) * float64(time.Second))
}

// rateLimitedTransport 包装 RoundTripper，在读取响应体时限速
type rateLimitedTransport struct {
	Base    http.RoundTripper
	limiter *tokenBucket
}

func (rlt *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rlt.Base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &rateLimitedReadCloser{rc: resp.Body, limiter: rlt.limiter}
	return resp, nil
}

type rateLimitedReadCloser struct {
	rc      io.ReadCloser
	limiter *tokenBucket
}

func (rlr *rateLimitedReadCloser) Read(p []byte) (int, error) {
	n, err := rlr.rc.Read(p)
	if n > 0 {
		if d := rlr.limiter.Take(int64(n)); d > 0 {
			time.Sleep(d)
		}
	}
	return n, err
}

func (rlr *rateLimitedReadCloser) Close() error {
	return rlr.rc.Close()
}
