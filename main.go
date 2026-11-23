// gdl 是一个支持断点续传与并发分块的命令行下载工具。
//
//	gdl -c 8 -o file.zip https://example.com/file.zip
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fansa92/gdl/internal/config"
	"github.com/fansa92/gdl/internal/download"
	"github.com/fansa92/gdl/internal/progress"
	"github.com/fansa92/gdl/internal/report"
)

// version 可在构建时通过 -ldflags "-X main.version=x.y.z" 注入（release 流程用 tag 覆盖）。
var version = "1.1.0"

// stringList 支持重复出现的字符串选项（如 -H, -M）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	cp := setupConsoleUTF8()
	code := run(os.Args[1:])
	cp.restore()
	os.Exit(code)
}

func run(args []string) int {
	// 先加载配置文件 + 环境变量
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		return download.ExitUsage
	}

	fs := flag.NewFlagSet("gdl", flag.ExitOnError)
	fs.Usage = func() { usage(os.Stderr) }

	var (
		output        string
		connections   int
		retries       int
		jobs          int
		maxRedirs     int
		timeout       time.Duration
		speedTime     time.Duration
		force         bool
		quiet         bool
		stdoutMode    bool
		conditional   bool
		noClobber     bool
		insecure      bool
		ipv4          bool
		ipv6          bool
		showVer       bool
		showHelp      bool
		sum           string
		agent         string
		rateLimit     string
		proxy         string
		caCert        string
		http2         string
		progressStyle string
		color         string
		completion    string
		inputFile     string
		writeOut      string
		user          string
		password      string
		bearer        string
		cookie        string
		cookieJar     string
		loadCookies   string
		certFile      string
		keyFile       string
		iface         string
		maxSize       string
		minSize       string
		speedLimit    string
		headers       stringList
		mirrors       stringList
	)

	// 输出
	fs.StringVar(&output, "o", "", "输出文件或目录（- 表示写到 stdout）")
	fs.StringVar(&output, "output", "", "输出文件或目录")
	fs.StringVar(&output, "O", "", "同 -o（wget 风格）")

	// 传输
	fs.IntVar(&connections, "c", 0, "并发分块数（0=自动）")
	fs.IntVar(&connections, "connections", 0, "并发分块数")
	fs.IntVar(&retries, "r", -1, "每个分块的最大重试次数（-1=配置文件默认）")
	fs.IntVar(&retries, "retries", -1, "每个分块的最大重试次数")
	fs.DurationVar(&timeout, "t", 0, "空闲超时（0=配置文件默认）")
	fs.DurationVar(&timeout, "timeout", 0, "空闲超时")
	fs.IntVar(&maxRedirs, "max-redirs", 0, "最大重定向次数（0=默认10，-1=不限）")

	// 文件
	fs.BoolVar(&force, "f", false, "覆盖已存在的文件并重新下载")
	fs.BoolVar(&force, "force", false, "覆盖已存在的文件")
	fs.BoolVar(&noClobber, "nc", false, "文件已存在时跳过而不是报错")
	fs.BoolVar(&noClobber, "no-clobber", false, "文件已存在时跳过")
	fs.BoolVar(&conditional, "N", false, "按本地文件时间做条件请求，未修改则跳过")
	fs.BoolVar(&conditional, "timestamping", false, "条件请求（同 -N）")
	fs.BoolVar(&stdoutMode, "stdout", false, "写到 stdout（管道模式）")
	fs.StringVar(&sum, "s", "", "下载后校验哈希：sha256|sha512|sha1|md5")
	fs.StringVar(&sum, "sum", "", "下载后校验哈希")

	// 请求
	fs.StringVar(&agent, "U", "", "自定义 User-Agent")
	fs.StringVar(&agent, "agent", "", "自定义 User-Agent")
	fs.Var(&headers, "H", "自定义请求头 \"Name: value\"，可重复")
	fs.Var(&headers, "header", "自定义请求头，可重复")
	fs.StringVar(&user, "u", "", "Basic 认证 user[:password]")
	fs.StringVar(&user, "user", "", "Basic 认证 user[:password]")
	fs.StringVar(&password, "password", "", "Basic 认证密码")
	fs.StringVar(&bearer, "bearer", "", "Bearer token")
	fs.StringVar(&cookie, "cookie", "", "直接发送 Cookie 头，如 \"a=b; c=d\"")
	fs.StringVar(&loadCookies, "load-cookies", "", "读取 Netscape 格式 cookie 文件")
	fs.StringVar(&cookieJar, "cookie-jar", "", "下载结束后保存 cookie 到文件")

	// 网络
	fs.Var(&mirrors, "M", "镜像 URL，可重复，按顺序尝试")
	fs.Var(&mirrors, "mirror", "镜像 URL，可重复")
	fs.StringVar(&rateLimit, "L", "", "限速，如 5M/100k/1G")
	fs.StringVar(&rateLimit, "rate-limit", "", "限速")
	fs.StringVar(&proxy, "P", "", "代理 http:// / https:// / socks5://")
	fs.StringVar(&proxy, "proxy", "", "代理")
	fs.BoolVar(&insecure, "k", false, "跳过 TLS 证书验证")
	fs.BoolVar(&insecure, "insecure", false, "跳过 TLS 证书验证")
	fs.StringVar(&caCert, "ca-cert", "", "自定义 CA 证书文件")
	fs.StringVar(&certFile, "cert", "", "客户端证书（PEM）")
	fs.StringVar(&keyFile, "key", "", "客户端私钥（PEM）")
	fs.StringVar(&http2, "http2", "", "HTTP/2: auto|on|off")
	fs.BoolVar(&ipv4, "4", false, "仅使用 IPv4")
	fs.BoolVar(&ipv6, "6", false, "仅使用 IPv6")
	fs.StringVar(&iface, "interface", "", "绑定出口网卡")

	// 限制
	fs.StringVar(&maxSize, "max-filesize", "", "文件大小上限，如 100M")
	fs.StringVar(&minSize, "min-filesize", "", "文件大小下限，如 1k")
	fs.StringVar(&speedLimit, "speed-limit", "", "速度下限，低于该值持续 --speed-time 后中断")
	fs.DurationVar(&speedTime, "speed-time", 0, "速度下限的观察窗口（默认 10s）")

	// 批量与显示
	fs.StringVar(&inputFile, "i", "", "从文件读取 URL 清单（- 表示 stdin）")
	fs.StringVar(&inputFile, "input-file", "", "从文件读取 URL 清单")
	fs.IntVar(&jobs, "j", 0, "同时下载的任务数（默认 1）")
	fs.IntVar(&jobs, "jobs", 0, "同时下载的任务数")
	fs.StringVar(&writeOut, "w", "", "下载结束后输出模板，支持 %{http_code} 等占位符")
	fs.StringVar(&writeOut, "write-out", "", "下载结束后输出模板")
	fs.BoolVar(&quiet, "q", false, "不显示进度条与冗余输出")
	fs.BoolVar(&quiet, "quiet", false, "安静模式")
	fs.StringVar(&progressStyle, "progress", "", "进度条风格: auto|ascii|unicode|none")
	fs.StringVar(&color, "color", "", "颜色: auto|always|never")
	fs.StringVar(&completion, "completion", "", "生成补全脚本: bash|zsh|fish|powershell")
	fs.BoolVar(&showVer, "version", false, "显示版本号")
	fs.BoolVar(&showHelp, "help", false, "显示帮助")
	fs.BoolVar(&showHelp, "h", false, "显示帮助（短）")

	// 处理 --help/-h/--version 提前退出（在原始参数上检查，避免 permute 干扰）
	for _, a := range args {
		if a == "--help" || a == "-h" {
			usage(os.Stdout)
			return download.ExitOK
		}
		if a == "--version" {
			fmt.Printf("gdl %s\n", version)
			return download.ExitOK
		}
	}

	// 选项前置（支持 `gdl url -o out`）
	args = permute(fs, args)
	if err := fs.Parse(args); err != nil {
		return download.ExitUsage
	}

	// 处理补全生成
	if completion != "" {
		genCompletion(os.Stdout, completion)
		return download.ExitOK
	}
	if showVer {
		fmt.Printf("gdl %s\n", version)
		return download.ExitOK
	}
	if showHelp {
		usage(os.Stdout)
		return download.ExitOK
	}
	if ipv4 && ipv6 {
		fmt.Fprintln(os.Stderr, "错误: -4 与 -6 不能同时使用")
		return download.ExitUsage
	}

	// 输出到 stdout 的两种写法
	if output == "-" {
		stdoutMode = true
		output = ""
	}
	if stdoutMode && output != "" {
		fmt.Fprintln(os.Stderr, "错误: --stdout 与 -o 文件名不能同时使用")
		return download.ExitUsage
	}

	ipVersion := 0
	if ipv4 {
		ipVersion = 4
	} else if ipv6 {
		ipVersion = 6
	}

	// 带单位的大小参数
	parseSize := func(name, v string) (config.Size, bool) {
		if strings.TrimSpace(v) == "" {
			return 0, true
		}
		n, perr := download.ParseSize(v)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "错误: --%s %v\n", name, perr)
			return 0, false
		}
		return config.Size(n), true
	}
	maxFS, ok1 := parseSize("max-filesize", maxSize)
	minFS, ok2 := parseSize("min-filesize", minSize)
	spdLimit, ok3 := parseSize("speed-limit", speedLimit)
	if !ok1 || !ok2 || !ok3 {
		return download.ExitUsage
	}

	// Basic 认证：-u user:pass 与 --password 两种写法都支持
	authUser, authPass := user, password
	if authUser != "" && authPass == "" {
		if u, p, found := strings.Cut(authUser, ":"); found {
			authUser, authPass = u, p
		}
	}

	// CLI 覆盖配置
	cliCfg := &config.Config{
		Output:        output,
		Connections:   connections,
		Retries:       retries,
		Timeout:       timeout,
		Force:         force,
		NoClobber:     noClobber,
		Conditional:   conditional,
		Quiet:         quiet,
		Stdout:        stdoutMode,
		Jobs:          jobs,
		Sum:           sum,
		UserAgent:     agent,
		Headers:       headers,
		Mirrors:       mirrors,
		RateLimit:     rateLimit,
		Proxy:         proxy,
		Insecure:      insecure,
		CACert:        caCert,
		CertFile:      certFile,
		KeyFile:       keyFile,
		ProgressStyle: progressStyle,
		Color:         color,
		User:          authUser,
		Password:      authPass,
		Bearer:        bearer,
		Cookie:        cookie,
		CookieJar:     cookieJar,
		LoadCookies:   loadCookies,
		IPVersion:     ipVersion,
		Interface:     iface,
		MaxRedirs:     maxRedirs,
		MaxFilesize:   maxFS,
		MinFilesize:   minFS,
		SpeedLimit:    spdLimit,
		SpeedTime:     speedTime,
		WriteOut:      writeOut,
	}
	if http2 != "" {
		switch strings.ToLower(http2) {
		case "on", "true", "1", "yes":
			b := true
			cliCfg.HTTP2 = &b
		case "off", "false", "0", "no":
			b := false
			cliCfg.HTTP2 = &b
		}
	}
	cfg.MergeCLI(cliCfg)

	// 收集 URL：命令行 + 批量清单
	urls := append([]string(nil), fs.Args()...)
	if inputFile != "" {
		lines, lerr := readURLList(inputFile)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "错误: 读取 URL 清单失败: %v\n", lerr)
			return download.ExitUsage
		}
		urls = append(urls, lines...)
	}
	if len(urls) == 0 {
		usage(os.Stderr)
		return download.ExitUsage
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			fmt.Fprintf(os.Stderr, "错误: 仅支持 http/https，收到 %q\n", u)
			return download.ExitUsage
		}
	}
	for _, m := range cfg.Mirrors {
		if !strings.HasPrefix(m, "http://") && !strings.HasPrefix(m, "https://") {
			fmt.Fprintf(os.Stderr, "错误: 镜像仅支持 http/https，收到 %q\n", m)
			return download.ExitUsage
		}
	}

	// 输出路径形态
	out := strings.TrimSpace(cfg.Output)
	outIsDir := false
	if out != "" {
		if fi, serr := os.Stat(out); serr == nil && fi.IsDir() {
			outIsDir = true
		}
		if strings.HasSuffix(out, "/") || strings.HasSuffix(out, `\`) {
			outIsDir = true
		}
	}
	if len(urls) > 1 && out != "" && !outIsDir {
		fmt.Fprintln(os.Stderr, "错误: 下载多个 URL 时不能用 -o 指定文件名（可指定目录）")
		return download.ExitUsage
	}
	if cfg.Stdout {
		if len(urls) > 1 {
			fmt.Fprintln(os.Stderr, "错误: --stdout 只能用于单个 URL")
			return download.ExitUsage
		}
		if cfg.Jobs > 1 {
			fmt.Fprintln(os.Stderr, "错误: --stdout 不支持 -j 并发")
			return download.ExitUsage
		}
	}
	if cfg.Jobs < 1 {
		cfg.Jobs = 1
	}

	// cookie 罐
	var jar *cookiejar.Jar
	if cfg.LoadCookies != "" || cfg.CookieJar != "" {
		jar = download.NewJar()
		if cfg.LoadCookies != "" {
			if lerr := download.LoadCookiesFile(cfg.LoadCookies, jar); lerr != nil {
				fmt.Fprintf(os.Stderr, "错误: 读取 cookie 文件失败: %v\n", lerr)
				return download.ExitUsage
			}
		}
	}

	progress.SetGlobalStyle(cfg.ProgressStyle)
	progress.SetGlobalColor(cfg.Color)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := download.ExitOK
	if cfg.Jobs <= 1 {
		for _, u := range urls {
			if !cfg.Quiet && !cfg.Stdout {
				fmt.Printf("下载: %s\n", u)
			}
			began := time.Now()
			res, derr := runOne(ctx, cfg, jar, u, out, outIsDir, false)
			code = mergeCode(code, reportOne(cfg, u, began, res, derr))
			if ctx.Err() != nil {
				break
			}
		}
	} else {
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			sem = make(chan struct{}, cfg.Jobs)
		)
		for _, u := range urls {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				began := time.Now()
				res, derr := runOne(ctx, cfg, jar, u, out, outIsDir, true)
				mu.Lock()
				code = mergeCode(code, reportOne(cfg, u, began, res, derr))
				mu.Unlock()
			}(u)
		}
		wg.Wait()
	}

	if cfg.CookieJar != "" && jar != nil {
		if serr := download.SaveCookiesFile(cfg.CookieJar, jar, urls); serr != nil {
			fmt.Fprintf(os.Stderr, "警告: 保存 cookie 失败: %v\n", serr)
		}
	}
	return code
}

// runOne 执行单个 URL 的下载。
func runOne(ctx context.Context, cfg *config.Config, jar *cookiejar.Jar, u, out string, outIsDir, quietBar bool) (*download.Result, error) {
	opts := cfg.ToDownloadOptions()
	opts.URL = u
	opts.Jar = jar
	opts.ProgressOut = os.Stderr
	if out != "" {
		if outIsDir {
			opts.OutputDir = out
		} else {
			opts.Output = out
		}
	}
	if cfg.Stdout {
		opts.Stdout = true
		opts.StdoutWriter = os.Stdout
		opts.Quiet = true
	}
	if quietBar {
		opts.Quiet = true
	}
	return download.Download(ctx, opts)
}

// reportOne 输出结果，返回该任务的退出码。
func reportOne(cfg *config.Config, u string, began time.Time, res *download.Result, err error) int {
	code := download.ExitCode(err)
	fields := report.Fields{
		URL:      u,
		Duration: time.Since(began),
		ExitCode: code,
		Err:      err,
	}
	if res != nil {
		fields.Path = res.Path
		fields.Size = res.Size
		fields.Status = res.StatusCode
		fields.Sum = res.Sum
		fields.Resumed = res.Resumed
		fields.NotModified = res.NotModified
		fields.Skipped = res.Skipped
		if res.Duration > 0 {
			fields.Duration = res.Duration
		}
	}

	msgOut := io.Writer(os.Stdout)
	if cfg.Stdout {
		msgOut = os.Stderr
	}

	switch {
	case err != nil:
		if code == download.ExitCancelled {
			fmt.Fprintf(os.Stderr, "\n已取消: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "\n失败: %v\n", err)
		}
	case res != nil && !cfg.Quiet:
		switch {
		case res.NotModified:
			fmt.Fprintf(msgOut, "未修改: %s\n", res.Path)
		case res.Skipped:
			fmt.Fprintf(msgOut, "跳过（已存在）: %s\n", res.Path)
		default:
			extra := ""
			if res.Resumed {
				extra = "  (断点续传)"
			}
			speed := float64(0)
			if d := res.Duration.Seconds(); d > 0 {
				speed = float64(res.Size) / d
			}
			fmt.Fprintf(msgOut, "完成: %s%s 用时 %s (%s/s) -> %s\n",
				progress.FormatBytes(res.Size), extra,
				progress.FormatDuration(res.Duration.Seconds()),
				progress.FormatBytes(int64(speed)), res.Path)
			if res.Sum != "" {
				fmt.Fprintf(msgOut, "校验: %s\n", res.Sum)
			}
		}
	}

	if cfg.WriteOut != "" {
		fmt.Fprint(os.Stdout, report.Format(cfg.WriteOut, fields))
	}
	return code
}

func mergeCode(cur, next int) int {
	if cur != download.ExitOK {
		return cur
	}
	return next
}

// readURLList 读取批量 URL 清单：跳过空行与 # 注释。
func readURLList(path string) ([]string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// permute 把选项参数移到位置参数前面，方便 `gdl url -o out` 这种写法。
// 同时正确处理 --long=value、--long value、-x value 三种形式。
func permute(fs *flag.FlagSet, args []string) []string {
	isBool := func(name string) bool {
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			return bf.IsBoolFlag()
		}
		return false
	}

	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if len(a) > 1 && (a[0] == '-' || strings.HasPrefix(a, "--")) {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			hasValue := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
				hasValue = true
			}
			// 单独的 "-" 表示 stdin/stdout，应作为取值而不是选项
			nextIsValue := i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || args[i+1] == "-")
			if !hasValue && !isBool(name) && nextIsValue {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `gdl %s - 命令行下载器（断点续传 / 并发分块 / 条件请求 / 认证 / 限速 / 镜像）

用法:
  gdl [选项] <URL> [URL...]

基础选项:
  -o, -O, -output    输出文件或目录（- 表示写到 stdout）
  -c,  -connections  并发分块数（0=自动，默认 %d）
  -r,  -retries      每个分块的最大重试次数（默认 5）
  -t,  -timeout      空闲超时（默认 30s）
  -f,  -force        覆盖已存在的文件并重新下载
  -nc, -no-clobber   文件已存在时跳过（退出码 0）
  -N,  -timestamping 条件请求：本地已是最新则跳过（304）
  -s,  -sum          下载后校验哈希：sha256|sha512|sha1|md5
  -i,  -input-file   从文件读取 URL 清单（- 读 stdin）
  -j,  -jobs         同时下载的任务数（默认 1，并发时自动关进度条）

请求选项:
  -U,  -agent        自定义 User-Agent
  -H,  -header       自定义请求头，可重复，如 -H "Referer: https://..."
  -u,  -user         Basic 认证 user[:password]
       -bearer       Bearer token
       -cookie       直接发送 Cookie 头
       -load-cookies 读取 Netscape 格式 cookie 文件
       -cookie-jar   下载结束后保存 cookie
  -M,  -mirror       镜像 URL，可重复，按顺序尝试

网络选项:
  -L,  -rate-limit   限速，如 5M/100k/1G
  -P,  -proxy        代理 http:// / https:// / socks5://
  -k,  -insecure     跳过 TLS 证书验证
       -ca-cert      自定义 CA 证书文件
       -cert / -key  客户端证书与私钥
  -4 / -6            仅用 IPv4 / IPv6
       -interface    绑定出口网卡
       -http2        HTTP/2 模式: auto|on|off
       -max-redirs   最大重定向次数（0=默认10，-1=不限）

限制选项:
       -max-filesize 大小上限，如 100M（探测后与下载中双重检查）
       -min-filesize 大小下限，如 1k
       -speed-limit  速度下限，低于该值持续 -speed-time 后中断
       -speed-time   速度下限观察窗口（默认 10s）

显示与工具:
  -q,  -quiet        安静模式
       -progress     进度条风格: auto|ascii|unicode|none
       -color        颜色模式: auto|always|never
  -w,  -write-out    下载结束后输出模板，如 "%%{http_code} %%{size_download}\n"
       -completion   生成补全脚本: bash|zsh|fish|powershell
       -version      显示版本号
  -h,  -help         显示帮助

示例:
  gdl https://example.com/file.zip
  gdl -c 16 -o ./file.zip https://example.com/file.zip
  gdl -O - https://example.com/file.zip | tar xz
  gdl -N -o index.html https://example.com/index.html
  gdl -u alice:secret -o priv.zip https://example.com/priv.zip
  gdl -i urls.txt -j 4 -o ./downloads
  gdl -s sha256 -f -L 2M -w "%%{http_code} %%{speed_download}\n" https://a.com/f.zip
  gdl -H "Referer: https://example.com/" -M https://m1/f.zip -M https://m2/f.zip https://a.com/f.zip

退出码:
  0 成功（含 304 未修改、-nc 跳过）   4 网络/超时/TLS 错误
  1 未分类错误                        5 服务器返回 4xx/5xx
  2 用法错误                          6 校验失败（哈希/长度）
  3 目标文件已存在                    7 触碰限制（大小上限/速度下限）
  8 本地文件不可写                  130 用户中断

配置文件 (~/.config/gdl/config.json):
  所有选项均可写入配置文件，环境变量 GDL_* 优先级更高，命令行参数最高。
  例: {"connections":16,"rate_limit":"10M","proxy":"socks5://127.0.0.1:1080"}

说明:
  下载中断（Ctrl+C 或网络故障）时会保留 <文件>.part 与 <文件>.part.json，
  重新执行同一命令即可断点续传；下载成功后自动清理这两个文件。
`, version, defaultConnections())
}

// genCompletion 生成 Shell 补全脚本
func genCompletion(w io.Writer, shell string) {
	var script string
	switch shell {
	case "bash":
		script = bashCompletion
	case "zsh":
		script = zshCompletion
	case "fish":
		script = fishCompletion
	case "powershell":
		script = powershellCompletion
	default:
		fmt.Fprintf(os.Stderr, "不支持的 shell: %s（可用: bash/zsh/fish/powershell）\n", shell)
		os.Exit(download.ExitUsage)
	}
	fmt.Fprint(w, script)
}

const allFlags = "-o -output -O -c -connections -r -retries -t -timeout -f -force -nc -no-clobber -N -timestamping -s -sum -i -input-file -j -jobs -U -agent -H -header -u -user -bearer -cookie -load-cookies -cookie-jar -M -mirror -L -rate-limit -P -proxy -k -insecure -ca-cert -cert -key -http2 -4 -6 -interface -max-redirs -max-filesize -min-filesize -speed-limit -speed-time -q -quiet -progress -color -w -write-out -completion -version -h -help"

const bashCompletion = `# gdl bash completion
_gdl() {
    local cur
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    if [[ ${cur} == -* ]]; then
        COMPREPLY=( $(compgen -W "` + allFlags + `" -- ${cur}) )
    fi
}
complete -F _gdl gdl
`

const zshCompletion = `#compdef gdl
_gdl() {
    _arguments -C -s \
        '-o[输出文件]:file:_files' '-output[输出文件]:file:_files' '-O[输出文件]:file:_files' \
        '-c[并发分块数]:n:' '-connections[并发分块数]:n:' \
        '-r[重试次数]:n:' '-retries[重试次数]:n:' \
        '-t[空闲超时]:duration:' '-timeout[空闲超时]:duration:' \
        '-f[覆盖]' '-force[覆盖]' '-nc[已存在则跳过]' '-no-clobber[已存在则跳过]' \
        '-N[条件请求]' '-timestamping[条件请求]' \
        '-s[哈希算法]:algo:(sha256 sha512 sha1 md5)' '-sum[哈希算法]:algo:(sha256 sha512 sha1 md5)' \
        '-i[URL 清单]:file:_files' '-input-file[URL 清单]:file:_files' \
        '-j[并发任务数]:n:' '-jobs[并发任务数]:n:' \
        '-U[User-Agent]:ua:' '-agent[User-Agent]:ua:' \
        '-H[请求头]:header:' '-header[请求头]:header:' \
        '-u[用户名]:user:' '-user[用户名]:user:' '-bearer[token]:token:' \
        '-cookie[Cookie]:cookie:' '-load-cookies[cookie 文件]:file:_files' '-cookie-jar[cookie 保存]:file:_files' \
        '-M[镜像]:url:' '-mirror[镜像]:url:' \
        '-L[限速]:rate:' '-rate-limit[限速]:rate:' \
        '-P[代理]:proxy:' '-proxy[代理]:proxy:' \
        '-k[跳过证书验证]' '-insecure[跳过证书验证]' \
        '-ca-cert[CA 证书]:file:_files' '-cert[客户端证书]:file:_files' '-key[客户端私钥]:file:_files' \
        '-http2[HTTP/2 模式]:mode:(auto on off)' '-4[仅 IPv4]' '-6[仅 IPv6]' \
        '-interface[出口网卡]:iface:' '-max-redirs[最大重定向]:n:' \
        '-max-filesize[大小上限]:size:' '-min-filesize[大小下限]:size:' \
        '-speed-limit[速度下限]:rate:' '-speed-time[观察窗口]:duration:' \
        '-q[安静]' '-quiet[安静]' '-progress[进度条]:style:(auto ascii unicode none)' \
        '-color[颜色]:mode:(auto always never)' '-w[输出模板]:fmt:' '-write-out[输出模板]:fmt:' \
        '-completion[生成补全]:shell:(bash zsh fish powershell)' '-version[版本]' \
        '-h[帮助]' '--help[帮助]' ':url:_urls'
}
`

const fishCompletion = `# gdl fish completion
complete -c gdl -f \
  -s o -l output -d "输出文件或目录" -r \
  -s c -l connections -d "并发分块数" -r \
  -s r -l retries -d "重试次数" -r \
  -s t -l timeout -d "空闲超时" -r \
  -s f -l force -d "覆盖已存在文件" \
  -l nc -l no-clobber -d "已存在则跳过" \
  -s N -l timestamping -d "条件请求" \
  -s s -l sum -d "哈希算法" -r \
  -s i -l input-file -d "URL 清单" -r \
  -s j -l jobs -d "并发任务数" -r \
  -s U -l agent -d "User-Agent" -r \
  -s H -l header -d "请求头" -r \
  -s u -l user -d "Basic 认证" -r \
  -l bearer -d "Bearer token" -r \
  -l cookie -d "Cookie" -r \
  -l load-cookies -d "读取 cookie 文件" -r \
  -l cookie-jar -d "保存 cookie" -r \
  -s M -l mirror -d "镜像 URL" -r \
  -s L -l rate-limit -d "限速" -r \
  -s P -l proxy -d "代理" -r \
  -s k -l insecure -d "跳过证书验证" \
  -l ca-cert -d "CA 证书" -r \
  -l cert -d "客户端证书" -r \
  -l key -d "客户端私钥" -r \
  -l http2 -d "HTTP/2 模式" -r \
  -s 4 -d "仅 IPv4" \
  -s 6 -d "仅 IPv6" \
  -l interface -d "出口网卡" -r \
  -l max-redirs -d "最大重定向次数" -r \
  -l max-filesize -d "大小上限" -r \
  -l min-filesize -d "大小下限" -r \
  -l speed-limit -d "速度下限" -r \
  -l speed-time -d "观察窗口" -r \
  -s q -l quiet -d "安静模式" \
  -l progress -d "进度条风格" -r \
  -l color -d "颜色模式" -r \
  -s w -l write-out -d "输出模板" -r \
  -l completion -d "生成补全脚本" -r \
  -l version -d "版本" \
  -s h -l help -d "帮助"
`

const powershellCompletion = `# gdl PowerShell completion (Register-ArgumentCompleter)
Register-ArgumentCompleter -CommandName gdl -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $opts = @(
        '-o', '-output', '-O', '-c', '-connections', '-r', '-retries', '-t', '-timeout',
        '-f', '-force', '-nc', '-no-clobber', '-N', '-timestamping', '-s', '-sum',
        '-i', '-input-file', '-j', '-jobs', '-U', '-agent', '-H', '-header',
        '-u', '-user', '-bearer', '-cookie', '-load-cookies', '-cookie-jar',
        '-M', '-mirror', '-L', '-rate-limit', '-P', '-proxy', '-k', '-insecure',
        '-ca-cert', '-cert', '-key', '-http2', '-4', '-6', '-interface', '-max-redirs',
        '-max-filesize', '-min-filesize', '-speed-limit', '-speed-time',
        '-q', '-quiet', '-progress', '-color', '-w', '-write-out',
        '-completion', '-version', '-h', '-help'
    )
    $opts | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`

// defaultConnections 根据 CPU 数给出一个合理的默认并发值。
func defaultConnections() int {
	n := os.Getenv("GDL_CONNECTIONS")
	if n != "" {
		var v int
		if _, err := fmt.Sscan(n, &v); err == nil && v > 0 {
			return v
		}
	}
	return 8
}
