// Package download 实现带断点续传和并发分块的文件下载。
package download

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fansa92/gdl/internal/progress"
)

// errNoRange 表示服务器中途拒绝 Range 请求，需要退回单流模式重来。
var errNoRange = errors.New("服务器不支持 Range 请求")

// Options 是一次下载任务的全部配置。
type Options struct {
	URL          string
	Output       string // 输出文件，或已存在的目录 / 以分隔符结尾的目录
	OutputDir    string // 目录模式（Output 为空时生效）
	Stdout       bool   // 输出到 StdoutWriter（管道模式，不支持续传）
	StdoutWriter io.Writer

	Connections int           // 并发分块数
	Timeout     time.Duration // 单块空闲超时
	Retries     int           // 单块最大重试次数
	Force       bool          // 覆盖已存在的文件
	NoClobber   bool          // 文件已存在时直接跳过
	Quiet       bool          // 不显示进度条
	UserAgent   string
	Header      http.Header
	Sum         string // 下载后校验：sha256 / sha512 / sha1 / md5
	ProgressOut io.Writer

	RateLimit string // "5M", "100k"
	Proxy     string // http/socks5
	Insecure  bool
	CACert    string
	HTTP2     *bool    // nil=auto
	Mirrors   []string // 备选镜像

	// 条件请求与请求侧
	Conditional bool   // -N：按本地文件时间做 If-Modified-Since，未变则跳过
	User        string // Basic 认证用户名
	Password    string
	Bearer      string // Bearer token
	Cookie      string // 直接拼进 Cookie 头
	Jar         *cookiejar.Jar
	CertFile    string
	KeyFile     string

	// 限制
	MaxFilesize int64         // >0 生效，探测后与下载中双重检查
	MinFilesize int64         // >0 生效，下载完成后检查
	SpeedLimit  int64         // bytes/s，>0 生效
	SpeedTime   time.Duration // 低于 SpeedLimit 持续该时长后中断
	MaxRedirs   int           // 0=默认 10，<0 不限
	IPVersion   int           // 0 自动 / 4 / 6
	Interface   string        // 出口网卡
}

// Result 描述一次成功的下载。
type Result struct {
	Path        string
	URL         string
	Size        int64
	Duration    time.Duration
	Sum         string
	Resumed     bool
	NotModified bool
	Skipped     bool
	StatusCode  int
}

// chunkRun 是运行时的分块状态，字段通过原子操作共享。
type chunkRun struct {
	start int64        // 分块起点（不变）
	end   atomic.Int64 // 含端点；-1 表示长度未知
	done  atomic.Int64 // 已下载字节数
}

func (r *chunkRun) complete() bool {
	end := r.end.Load()
	if end < 0 {
		return false
	}
	return r.done.Load() >= end-r.start+1
}

type downloader struct {
	opt    Options
	client *http.Client
	header http.Header

	tmp       string
	statePath string
	file      *os.File
	w         io.Writer // stdout 模式下的写入目标
	stdout    bool
	sumHash   hash.Hash
	bar       *progress.Bar
	single    bool // 只有一个从 0 开始的分块（允许 200 回退）
	mu        sync.Mutex
	dirty     atomic.Bool

	condSince time.Time
	status    atomic.Int64

	stateStop chan struct{}
	stateDone chan struct{}
	stateOnce sync.Once
}

func normalize(opt Options) Options {
	if opt.Connections < 1 {
		opt.Connections = 1
	}
	if opt.Retries < 0 {
		opt.Retries = 0
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "gdl/1.0"
	}
	if opt.ProgressOut == nil {
		opt.ProgressOut = io.Discard
	}
	if opt.SpeedLimit > 0 && opt.SpeedTime <= 0 {
		opt.SpeedTime = 10 * time.Second
	}
	if opt.MaxRedirs == 0 {
		opt.MaxRedirs = 10
	}
	if opt.Stdout {
		// 管道不可回退，只能单流顺序写
		opt.Connections = 1
		if opt.StdoutWriter == nil {
			opt.StdoutWriter = os.Stdout
		}
	}
	return opt
}

func (o Options) validate() error {
	if strings.TrimSpace(o.URL) == "" {
		return Errorf(ExitUsage, "缺少 URL")
	}
	if o.Sum != "" {
		switch strings.ToLower(o.Sum) {
		case "sha256", "sha512", "sha1", "md5":
		default:
			return Errorf(ExitUsage, "不支持的校验算法 %q（可用：sha256 sha512 sha1 md5）", o.Sum)
		}
	}
	if o.MinFilesize > 0 && o.MaxFilesize > 0 && o.MinFilesize > o.MaxFilesize {
		return Errorf(ExitUsage, "--min-filesize 大于 --max-filesize")
	}
	return nil
}

// Download 下载 opt.URL 到 opt.Output。
func Download(ctx context.Context, opt Options) (*Result, error) {
	opt = normalize(opt)
	if err := opt.validate(); err != nil {
		return nil, err
	}

	d := &downloader{opt: opt, stdout: opt.Stdout}
	if opt.Header != nil {
		d.header = opt.Header.Clone()
	} else {
		d.header = http.Header{}
	}
	d.client = d.newClient()
	return d.runWithMirrors(ctx, false)
}

// runWithMirrors 尝试主 URL 和所有镜像，直到成功
func (d *downloader) runWithMirrors(ctx context.Context, noRange bool) (*Result, error) {
	urls := append([]string{d.opt.URL}, d.opt.Mirrors...)
	var lastErr error
	for i, u := range urls {
		d.opt.URL = u
		d.client = d.newClient()
		res, err := d.run(ctx, noRange)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, errNoRange) || fatal(err) {
			return nil, err
		}
		if i < len(urls)-1 {
			select {
			case <-ctx.Done():
				return nil, Wrap(ExitCancelled, ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	if len(urls) == 1 {
		return nil, lastErr
	}
	return nil, Wrap(ExitNetwork, fmt.Errorf("所有镜像均失败，最后错误: %w", lastErr))
}

// run 执行一次完整的下载流程。noRange 为 true 时强制单流模式。
func (d *downloader) run(ctx context.Context, noRange bool) (*Result, error) {
	began := time.Now()

	// -N：用本地已存在文件的时间做条件请求
	if d.opt.Conditional && !d.stdout {
		if p := d.guessPath(); p != "" {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				d.condSince = fi.ModTime()
			}
		}
	}

	inf, err := d.probeWithRetry(ctx, d.opt.URL)
	if err != nil {
		return nil, err
	}
	if noRange {
		inf.ranges = false
	}

	outPath, err := d.resolvePath(inf)
	if err != nil {
		return nil, err
	}
	if !d.stdout {
		if dir := filepath.Dir(outPath); dir != "" && dir != "." {
			if merr := os.MkdirAll(dir, 0o755); merr != nil {
				return nil, Wrap(ExitPermission, fmt.Errorf("创建目录失败: %w", merr))
			}
		}
	}

	res := &Result{Path: outPath, URL: d.opt.URL}
	if inf.notModified {
		res.NotModified = true
		res.StatusCode = http.StatusNotModified
		res.Duration = time.Since(began)
		if inf.size > 0 {
			res.Size = inf.size
		}
		return res, nil
	}

	if d.opt.MaxFilesize > 0 && inf.size > d.opt.MaxFilesize {
		return nil, Errorf(ExitLimit, "文件大小 %s 超过上限 %s",
			progress.FormatBytes(inf.size), progress.FormatBytes(d.opt.MaxFilesize))
	}

	exists := false
	var existSize int64
	if fi, err := os.Stat(outPath); err == nil && !fi.IsDir() {
		exists = true
		existSize = fi.Size()
	}
	if exists && !d.opt.Force && !d.opt.Conditional {
		if d.opt.NoClobber {
			res.Skipped = true
			res.Size = existSize
			res.Duration = time.Since(began)
			return res, nil
		}
		return nil, Errorf(ExitExists, "文件已存在: %s（-f 覆盖 / -nc 跳过）", outPath)
	}

	if d.stdout {
		d.w = d.opt.StdoutWriter
		if d.opt.Sum != "" {
			d.sumHash = newHash(strings.ToLower(d.opt.Sum))
		}
	}

	d.tmp = outPath + ".part"
	d.statePath = outPath + ".part.json"

	// 载入断点状态（管道模式与条件请求模式不做续传）
	var st *state
	if noRange || d.stdout || d.opt.Conditional {
		os.Remove(d.statePath)
		os.Remove(d.tmp)
	} else {
		st = d.loadState(inf)
		if st == nil {
			os.Remove(d.statePath)
			os.Remove(d.tmp)
		}
	}

	// 划分分块
	var planned []chunk
	var startBytes int64
	switch {
	case st != nil:
		planned = st.Chunks
	case inf.size > 0 && inf.ranges && d.opt.Connections > 1:
		planned = splitChunks(inf.size, d.opt.Connections)
	default:
		end := int64(-1)
		if inf.size > 0 {
			end = inf.size - 1
		}
		planned = []chunk{{Start: 0, End: end}}
	}
	for _, c := range planned {
		startBytes += c.Done
	}
	d.single = len(planned) == 1 && planned[0].Start == 0

	// 打开输出（管道模式下不需要临时文件）
	if !d.stdout {
		file, ferr := os.OpenFile(d.tmp, os.O_CREATE|os.O_RDWR, 0o644)
		if ferr != nil {
			return nil, Wrap(ExitPermission, fmt.Errorf("创建临时文件失败: %w", ferr))
		}
		d.file = file
		defer func() {
			if d.file != nil {
				d.file.Close()
			}
		}()

		if len(planned) > 1 {
			// 分块模式预分配完整长度，便于 WriteAt 定位
			if fi, serr := file.Stat(); serr == nil && fi.Size() != inf.size {
				if terr := file.Truncate(inf.size); terr != nil {
					return nil, Wrap(ExitPermission, fmt.Errorf("预分配文件失败: %w", terr))
				}
			}
		} else if st == nil {
			if terr := file.Truncate(0); terr != nil {
				return nil, Wrap(ExitPermission, fmt.Errorf("清空临时文件失败: %w", terr))
			}
		}
	}

	runs := make([]*chunkRun, len(planned))
	allKnown := true
	var total int64
	for i, c := range planned {
		r := &chunkRun{start: c.Start}
		r.end.Store(c.End)
		r.done.Store(c.Done)
		runs[i] = r
		if c.End < 0 {
			allKnown = false
		} else {
			total += c.End - c.Start + 1
		}
	}

	d.bar = progress.New(0, d.opt.ProgressOut, d.opt.Quiet)
	if allKnown {
		d.bar.SetTotal(total)
	}
	d.bar.Add(startBytes)

	if !d.stdout {
		d.saveState(snapshot(runs), inf)
		d.startStateSaver(runs, inf)
		defer d.stopStateSaver()
	}

	resumed := startBytes > 0

	err = d.runChunks(ctx, runs)
	if err != nil {
		d.bar.Stop()
		d.stopStateSaver()
		if !d.stdout {
			d.saveState(snapshot(runs), inf) // 保留断点
		}
		if ctx.Err() != nil {
			if d.stdout {
				return nil, Wrap(ExitCancelled, fmt.Errorf("下载已中断"))
			}
			return nil, Wrap(ExitCancelled, fmt.Errorf("下载已中断，进度已保存，重新执行同一命令即可断点续传"))
		}
		if errors.Is(err, errNoRange) && !noRange {
			// 服务器改口不支持 Range：关掉文件，单流重来一次
			if d.file != nil {
				d.file.Close()
				d.file = nil
			}
			return d.run(ctx, true)
		}
		return nil, err
	}

	// 校验所有分块都已完成
	var expected int64 = -1
	if allKnown {
		expected = 0
	}
	for _, r := range runs {
		end := r.end.Load()
		if end < 0 {
			continue
		}
		want := end - r.start + 1
		if r.done.Load() != want {
			return nil, Errorf(ExitVerify, "分块未完成（%d / %d 字节）", r.done.Load(), want)
		}
		if expected >= 0 {
			expected += want
		}
	}

	d.bar.Stop()
	d.stopStateSaver()
	res.Duration = time.Since(began)
	res.StatusCode = int(d.status.Load())
	if res.StatusCode == 0 {
		res.StatusCode = http.StatusOK
	}

	// 管道模式：无法回读文件，收尾只做大小与流式哈希
	if d.stdout {
		res.Size = d.bar.Done()
		res.Resumed = false
		if d.sumHash != nil {
			res.Sum = strings.ToLower(d.opt.Sum) + ":" + hex.EncodeToString(d.sumHash.Sum(nil))
		}
		if d.opt.MinFilesize > 0 && res.Size < d.opt.MinFilesize {
			return nil, Errorf(ExitLimit, "已写入 %s，小于下限 %s",
				progress.FormatBytes(res.Size), progress.FormatBytes(d.opt.MinFilesize))
		}
		return res, nil
	}

	if err := d.file.Sync(); err != nil {
		return nil, Wrap(ExitPermission, fmt.Errorf("刷新文件失败: %w", err))
	}
	d.file.Close()
	d.file = nil

	fi, err := os.Stat(d.tmp)
	if err != nil {
		return nil, Wrap(ExitPermission, err)
	}
	if expected >= 0 && fi.Size() != expected {
		return nil, Errorf(ExitVerify, "文件大小不一致：期望 %d 字节，实际 %d 字节", expected, fi.Size())
	}
	res.Size = fi.Size()
	res.Resumed = resumed

	if d.opt.MinFilesize > 0 && res.Size < d.opt.MinFilesize {
		os.Remove(d.tmp)
		return nil, Errorf(ExitLimit, "文件大小 %s 小于下限 %s",
			progress.FormatBytes(res.Size), progress.FormatBytes(d.opt.MinFilesize))
	}

	if d.opt.Sum != "" {
		s, serr := fileChecksum(d.tmp, strings.ToLower(d.opt.Sum))
		if serr != nil {
			return nil, Wrap(ExitVerify, fmt.Errorf("计算校验和失败: %w", serr))
		}
		res.Sum = s
	}

	if err := os.Rename(d.tmp, outPath); err != nil {
		// Windows 下目标已存在时会失败，尝试先删除再重命名
		if rerr := os.Remove(outPath); rerr != nil && !os.IsNotExist(rerr) {
			return nil, Wrap(ExitPermission, fmt.Errorf("重命名失败: %w", err))
		}
		if err2 := os.Rename(d.tmp, outPath); err2 != nil {
			return nil, Wrap(ExitPermission, fmt.Errorf("重命名失败: %w", err2))
		}
	}
	os.Remove(d.statePath)
	os.Remove(d.statePath + ".tmp")

	// -N：把服务端修改时间落到本地，下一次条件请求才有意义
	if d.opt.Conditional && inf.modified != "" {
		if t, perr := http.ParseTime(inf.modified); perr == nil {
			_ = os.Chtimes(outPath, t, t)
		}
	}

	return res, nil
}

// guessPath 在探测前尽力猜出输出路径（用于 -N 判断本地文件是否存在）。
func (d *downloader) guessPath() string {
	out := strings.TrimSpace(d.opt.Output)
	if out != "" {
		if fi, err := os.Stat(out); err == nil && fi.IsDir() {
			return ""
		}
		if strings.HasSuffix(out, "/") || strings.HasSuffix(out, `\`) {
			return ""
		}
		return out
	}
	dir := d.opt.OutputDir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, DefaultName(d.opt.URL))
}

// resolvePath 结合探测结果决定最终输出路径（优先使用 Content-Disposition 文件名）。
func (d *downloader) resolvePath(inf *info) (string, error) {
	if d.stdout {
		return "-", nil
	}
	out := strings.TrimSpace(d.opt.Output)
	dir := d.opt.OutputDir
	if out != "" {
		if fi, err := os.Stat(out); err == nil && fi.IsDir() {
			dir, out = out, ""
		} else if strings.HasSuffix(out, "/") || strings.HasSuffix(out, `\`) {
			dir, out = out, ""
		}
	}
	if out != "" {
		return out, nil
	}
	if dir == "" {
		dir = "."
	}
	name := inf.name
	if name == "" {
		name = DefaultName(d.opt.URL)
	}
	if name == "" {
		return "", Errorf(ExitUsage, "无法确定输出文件名：%s", d.opt.URL)
	}
	return filepath.Join(dir, name), nil
}

// DefaultName 从 URL 推断本地文件名。
func DefaultName(rawURL string) string {
	name := ""
	if req, err := http.NewRequest(http.MethodGet, rawURL, nil); err == nil {
		p := req.URL.Path
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			p = p[i+1:]
		}
		name = sanitizeName(p)
	}
	if name == "" {
		name = "download"
	}
	return name
}

// probeWithRetry 带重试的资源探测（临时 5xx / 网络抖动不应直接失败）。
func (d *downloader) probeWithRetry(ctx context.Context, target string) (*info, error) {
	var last error
	for i := 0; i <= d.opt.Retries; i++ {
		if i > 0 {
			if err := waitBackoff(ctx, i); err != nil {
				return nil, Wrap(ExitCancelled, err)
			}
		}
		inf, err := d.probe(ctx, target)
		if err == nil {
			return inf, nil
		}
		if ctx.Err() != nil {
			return nil, Wrap(ExitCancelled, ctx.Err())
		}
		last = err
	}
	return nil, Wrap(ExitNetwork, fmt.Errorf("探测资源失败: %w", last))
}

// runChunks 并发下载所有分块。
func (d *downloader) runChunks(ctx context.Context, runs []*chunkRun) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan *chunkRun)
	errCh := make(chan error, 1)
	var failed atomic.Bool
	var wg sync.WaitGroup

	workers := d.opt.Connections
	if workers > len(runs) {
		workers = len(runs)
	}
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				if failed.Load() {
					continue
				}
				if err := d.downloadChunk(cctx, c); err != nil {
					select {
					case errCh <- err:
					default:
					}
					failed.Store(true)
					cancel()
				}
			}
		}()
	}

	for _, c := range runs {
		select {
		case jobs <- c:
		case <-cctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// downloadChunk 带重试地下载一个分块，断点在 c.done 中累积。
func (d *downloader) downloadChunk(ctx context.Context, c *chunkRun) error {
	if c.complete() {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt <= d.opt.Retries; attempt++ {
		if attempt > 0 {
			if err := waitBackoff(ctx, attempt); err != nil {
				return err
			}
		}
		err := d.attempt(ctx, c)
		if err == nil {
			return nil
		}
		if errors.Is(err, errNoRange) || errors.Is(err, context.Canceled) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 限制类 / 校验类错误重试没有意义
		if fatal(err) {
			return err
		}
		// 管道已经写出去的字节收不回来，不能重试
		if d.stdout && c.done.Load() > 0 {
			return err
		}
		lastErr = err
	}
	return Wrap(ExitNetwork, fmt.Errorf("重试 %d 次后仍失败: %w", d.opt.Retries, lastErr))
}

type speedSample struct {
	t     time.Time
	bytes int64
}

// attempt 发起一次请求并读取当前分块的剩余数据。
func (d *downloader) attempt(ctx context.Context, c *chunkRun) error {
	start := c.start + c.done.Load()
	end := c.end.Load()
	if end >= 0 && start > end {
		if start == end+1 {
			return nil // 已完成
		}
		// 数据异常越界，退回分块起点重来
		if prev := c.done.Swap(0); prev != 0 {
			d.bar.Add(-prev)
		}
		start = c.start
	}

	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 空闲超时看门狗 + 速度下限检查
	var lastAct atomic.Int64
	lastAct.Store(time.Now().UnixNano())

	var reasonMu sync.Mutex
	var reason error
	abortWith := func(err error) {
		reasonMu.Lock()
		if reason == nil {
			reason = err
		}
		reasonMu.Unlock()
		cancel()
	}
	reasonOf := func() error {
		reasonMu.Lock()
		defer reasonMu.Unlock()
		return reason
	}

	watch := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		samples := []speedSample{{time.Now(), d.bar.Done()}}
		for {
			select {
			case <-watch:
				return
			case <-actx.Done():
				return
			case now := <-tk.C:
				if time.Since(time.Unix(0, lastAct.Load())) >= d.opt.Timeout {
					abortWith(Wrap(ExitNetwork, fmt.Errorf("等待数据超时（%s 无响应）", d.opt.Timeout)))
					return
				}
				if d.opt.SpeedLimit <= 0 {
					continue
				}
				cur := d.bar.Done()
				samples = append(samples, speedSample{now, cur})
				window := d.opt.SpeedTime
				if window <= 0 {
					window = 10 * time.Second
				}
				for len(samples) > 2 && now.Sub(samples[1].t) >= window {
					samples = samples[1:]
				}
				span := now.Sub(samples[0].t)
				if span < window {
					continue
				}
				rate := float64(cur-samples[0].bytes) / span.Seconds()
				if rate < float64(d.opt.SpeedLimit) {
					abortWith(Errorf(ExitLimit, "平均速度 %s/s 低于下限 %s/s，持续 %s 后中断",
						progress.FormatBytes(int64(rate)),
						progress.FormatBytes(d.opt.SpeedLimit),
						progress.FormatDuration(span.Seconds())))
					return
				}
			}
		}
	}()
	defer close(watch)

	req, err := d.newRequest(ctx, http.MethodGet, d.opt.URL)
	if err != nil {
		return Wrap(ExitUsage, err)
	}
	req = req.WithContext(actx)
	if start > 0 || c.start > 0 || end >= 0 {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}

	resp, err := d.client.Do(req)
	if err != nil {
		if r := reasonOf(); r != nil {
			return r
		}
		if actx.Err() != nil && ctx.Err() == nil {
			return Wrap(ExitNetwork, fmt.Errorf("等待数据超时（%s 无响应）: %w", d.opt.Timeout, err))
		}
		return Wrap(ExitNetwork, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		d.status.Store(int64(resp.StatusCode))
		// 正常：校验服务器返回的范围与请求一致
		if s, _, _, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && s != start {
			return Errorf(ExitHTTP, "服务器返回的范围不匹配（期望 %d，实际 %d）", start, s)
		}
	case http.StatusOK:
		d.status.Store(int64(resp.StatusCode))
		if !d.single || c.start > 0 {
			return errNoRange
		}
		// 服务器忽略了 Range，改为从头完整下载
		if prev := c.done.Swap(0); prev != 0 {
			d.bar.Add(-prev)
		}
		if resp.ContentLength >= 0 {
			c.end.Store(resp.ContentLength - 1)
			d.bar.SetTotal(resp.ContentLength)
			if d.opt.MaxFilesize > 0 && resp.ContentLength > d.opt.MaxFilesize {
				return Errorf(ExitLimit, "文件大小 %s 超过上限 %s",
					progress.FormatBytes(resp.ContentLength), progress.FormatBytes(d.opt.MaxFilesize))
			}
		} else {
			c.end.Store(-1)
			d.bar.SetTotal(-1)
		}
		if !d.stdout {
			if terr := d.file.Truncate(0); terr != nil {
				return Wrap(ExitPermission, fmt.Errorf("清空临时文件失败: %w", terr))
			}
		}
		start = 0
	case http.StatusRequestedRangeNotSatisfiable:
		return Errorf(ExitHTTP, "服务器返回 416，无法继续（远端文件可能已变化）")
	default:
		return Errorf(ExitHTTP, "HTTP %s", resp.Status)
	}

	// 读取并写入
	buf := make([]byte, 256*1024)
	off := start
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			lastAct.Store(time.Now().UnixNano())
			if d.stdout {
				if _, werr := d.w.Write(buf[:n]); werr != nil {
					return Wrap(ExitPermission, fmt.Errorf("写入输出失败: %w", werr))
				}
				if d.sumHash != nil {
					d.sumHash.Write(buf[:n])
				}
			} else if _, werr := d.file.WriteAt(buf[:n], off); werr != nil {
				return Wrap(ExitPermission, fmt.Errorf("写入文件失败: %w", werr))
			}
			off += int64(n)
			c.done.Add(int64(n))
			d.bar.Add(int64(n))
			d.dirty.Store(true)

			if d.opt.MaxFilesize > 0 && d.bar.Done() > d.opt.MaxFilesize {
				return Errorf(ExitLimit, "已下载 %s，超过上限 %s",
					progress.FormatBytes(d.bar.Done()), progress.FormatBytes(d.opt.MaxFilesize))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if r := reasonOf(); r != nil {
				return r
			}
			if actx.Err() != nil && ctx.Err() == nil {
				return Wrap(ExitNetwork, fmt.Errorf("下载超时（%s 无数据）: %w", d.opt.Timeout, rerr))
			}
			// 连接中断等可重试错误，c.done 已记录进度
			return Wrap(ExitNetwork, fmt.Errorf("连接中断: %w", rerr))
		}
	}

	// 收完之后核对长度
	if e := c.end.Load(); e >= 0 {
		want := e - c.start + 1
		got := c.done.Load()
		if got < want {
			return Errorf(ExitVerify, "连接提前中断（%s / %s）",
				progress.FormatBytes(got), progress.FormatBytes(want))
		}
		if got > want {
			if d.single {
				// 服务器给多了：裁掉多余部分
				if !d.stdout {
					if terr := d.file.Truncate(want); terr != nil {
						return Wrap(ExitPermission, terr)
					}
				}
				d.bar.Add(want - got)
				c.done.Store(want)
				return nil
			}
			return Errorf(ExitVerify, "服务器返回的数据超出预期（远端文件可能已变化）")
		}
	}
	return nil
}

// startStateSaver 启动周期性保存断点的协程。
func (d *downloader) startStateSaver(runs []*chunkRun, inf *info) {
	d.stateStop = make(chan struct{})
	d.stateDone = make(chan struct{})
	d.stateOnce = sync.Once{}
	go func() {
		defer close(d.stateDone)
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				if d.dirty.Swap(false) {
					d.saveState(snapshot(runs), inf)
				}
			case <-d.stateStop:
				if d.dirty.Swap(false) {
					d.saveState(snapshot(runs), inf)
				}
				return
			}
		}
	}()
}

// stopStateSaver 停止状态保存协程，可重复调用。
func (d *downloader) stopStateSaver() {
	if d.stateStop == nil {
		return
	}
	d.stateOnce.Do(func() {
		close(d.stateStop)
		<-d.stateDone
	})
}

func snapshot(runs []*chunkRun) []chunk {
	out := make([]chunk, len(runs))
	for i, r := range runs {
		out[i] = chunk{Start: r.start, End: r.end.Load(), Done: r.done.Load()}
	}
	return out
}

// splitChunks 把 [0, size) 均匀切成 n 段。
func splitChunks(size int64, n int) []chunk {
	if int64(n) > size {
		n = int(size)
	}
	if n < 1 {
		n = 1
	}
	chunks := make([]chunk, 0, n)
	base, rem := size/int64(n), size%int64(n)
	var off int64
	for i := 0; i < n; i++ {
		l := base
		if int64(i) < rem {
			l++
		}
		chunks = append(chunks, chunk{Start: off, End: off + l - 1})
		off += l
	}
	return chunks
}

// waitBackoff 指数退避，可被 ctx 取消。
func waitBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(1<<uint(attempt-1)) * 400 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newHash(algo string) hash.Hash {
	switch algo {
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	case "sha1":
		return sha1.New()
	case "md5":
		return md5.New()
	}
	return nil
}

// fileChecksum 计算文件哈希，返回 "算法:十六进制"。
func fileChecksum(path, algo string) (string, error) {
	h := newHash(algo)
	if h == nil {
		return "", fmt.Errorf("不支持的校验算法: %s", algo)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return algo + ":" + hex.EncodeToString(h.Sum(nil)), nil
}
