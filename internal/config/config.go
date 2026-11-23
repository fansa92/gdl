package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fansa92/gdl/internal/download"
)

// Size 允许配置文件里写数字（字节）或 "100M" / "5k" 这种带单位的字符串。
type Size int64

func (s *Size) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if str == "" {
			*s = 0
			return nil
		}
		n, err := download.ParseSize(str)
		if err != nil {
			return err
		}
		*s = Size(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = Size(n)
	return nil
}

func (s Size) Int64() int64 { return int64(s) }

// Config 映射命令行/配置文件/环境变量三层优先级：CLI > 文件 > 环境变量 > 默认值
type Config struct {
	Connections   int           `json:"connections"   yaml:"connections"`
	Retries       int           `json:"retries"       yaml:"retries"`
	Timeout       time.Duration `json:"timeout"       yaml:"timeout"`
	RateLimit     string        `json:"rate_limit"    yaml:"rate_limit"` // "5M", "100k" 等
	Proxy         string        `json:"proxy"         yaml:"proxy"`      // "http://..." / "socks5://..."
	Force         bool          `json:"force"         yaml:"force"`
	Quiet         bool          `json:"quiet"         yaml:"quiet"`
	Sum           string        `json:"sum"           yaml:"sum"`
	UserAgent     string        `json:"user_agent"    yaml:"user_agent"`
	Headers       []string      `json:"headers"       yaml:"headers"`
	Insecure      bool          `json:"insecure"      yaml:"insecure"`
	CACert        string        `json:"ca_cert"       yaml:"ca_cert"`
	HTTP2         *bool         `json:"http2"         yaml:"http2"` // nil=auto, true=force, false=disable
	Mirrors       []string      `json:"mirrors"       yaml:"mirrors"`
	ProgressStyle string        `json:"progress_style" yaml:"progress_style"` // "auto"|"ascii"|"unicode"|"none"
	Color         string        `json:"color"         yaml:"color"`           // "auto"|"always"|"never"

	// 输出与调度
	Stdout    bool `json:"stdout"     yaml:"stdout"`
	NoClobber bool `json:"no_clobber" yaml:"no_clobber"`
	Jobs      int  `json:"jobs"       yaml:"jobs"`

	// 条件请求
	Conditional bool `json:"timestamping" yaml:"timestamping"`

	// 认证与 cookie
	User        string `json:"user"         yaml:"user"`
	Password    string `json:"password"     yaml:"password"`
	Bearer      string `json:"bearer"       yaml:"bearer"`
	Cookie      string `json:"cookie"       yaml:"cookie"`
	CookieJar   string `json:"cookie_jar"   yaml:"cookie_jar"`
	LoadCookies string `json:"load_cookies" yaml:"load_cookies"`

	// TLS / 网络
	CertFile  string `json:"cert"      yaml:"cert"`
	KeyFile   string `json:"key"       yaml:"key"`
	IPVersion int    `json:"ip_version" yaml:"ip_version"`
	Interface string `json:"interface"  yaml:"interface"`
	MaxRedirs int    `json:"max_redirs" yaml:"max_redirs"`

	// 限制
	MaxFilesize Size          `json:"max_filesize" yaml:"max_filesize"`
	MinFilesize Size          `json:"min_filesize" yaml:"min_filesize"`
	SpeedLimit  Size          `json:"speed_limit"  yaml:"speed_limit"`
	SpeedTime   time.Duration `json:"speed_time"   yaml:"speed_time"`

	// 运行时填充（不写入文件）
	URLs     []string `json:"-" yaml:"-"`
	Output   string   `json:"-" yaml:"-"`
	WriteOut string   `json:"-" yaml:"-"`
}

func Default() *Config {
	return &Config{
		Connections:   defaultConnections(),
		Retries:       5,
		Timeout:       30 * time.Second,
		UserAgent:     "gdl/1.0.0",
		ProgressStyle: "auto",
		Color:         "auto",
		Jobs:          1,
		MaxRedirs:     10,
		SpeedTime:     10 * time.Second,
	}
}

func defaultConnections() int {
	// 同 main.go 逻辑
	n := 8
	return n
}

// Load 读取配置文件（~/.config/gdl/config.json），再合并环境变量
func Load() (*Config, error) {
	cfg := Default()

	// 1) 配置文件
	path := configPath()
	if data, err := os.ReadFile(path); err == nil {
		if err := parseConfig(data, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	// 2) 环境变量覆盖
	applyEnv(cfg)

	return cfg, nil
}

func configPath() string {
	// 支持 XDG_CONFIG_HOME
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gdl", "config.json")
	}
	home, _ := os.UserConfigDir()
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "gdl", "config.json")
}

func parseConfig(data []byte, cfg *Config) error {
	// 先试 JSON，再试 YAML（简单实现：去掉注释的 JSON 兼容大多数 YAML 子集）
	return json.Unmarshal(data, cfg)
}

func applyEnv(cfg *Config) {
	// GDL_CONNECTIONS, GDL_RETRIES, GDL_TIMEOUT, GDL_RATE_LIMIT, GDL_PROXY,
	// GDL_FORCE, GDL_QUIET, GDL_SUM, GDL_USER_AGENT, GDL_INSECURE, GDL_CA_CERT,
	// GDL_HTTP2, GDL_MIRRORS, GDL_PROGRESS_STYLE, GDL_COLOR, ...
	type envMap struct {
		key string
		set func(string)
	}
	var envs []envMap
	add := func(key string, set func(string)) {
		envs = append(envs, envMap{key: key, set: set})
	}
	addInt := func(key string, target *int) {
		add(key, func(v string) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*target = n
			}
		})
	}
	addDur := func(key string, target *time.Duration) {
		add(key, func(v string) {
			if d, err := time.ParseDuration(v); err == nil {
				*target = d
			}
		})
	}
	addBool := func(key string, target *bool) {
		add(key, func(v string) { *target = v == "1" || v == "true" || v == "yes" })
	}
	addString := func(key string, target *string) {
		add(key, func(v string) { *target = v })
	}
	addSize := func(key string, target *Size) {
		add(key, func(v string) {
			if n, err := download.ParseSize(v); err == nil {
				*target = Size(n)
			}
		})
	}
	addHeaders := func(key string, target *[]string) {
		add(key, func(v string) { *target = append(*target, v) })
	}
	addHTTP2 := func(key string) {
		add(key, func(v string) {
			switch strings.ToLower(v) {
			case "1", "true", "yes", "on":
				b := true
				cfg.HTTP2 = &b
			case "0", "false", "no", "off":
				b := false
				cfg.HTTP2 = &b
			}
		})
	}
	addMirrors := func(key string, target *[]string) {
		add(key, func(v string) { *target = strings.Split(v, ",") })
	}

	addInt("GDL_CONNECTIONS", &cfg.Connections)
	addInt("GDL_RETRIES", &cfg.Retries)
	addDur("GDL_TIMEOUT", &cfg.Timeout)
	addString("GDL_RATE_LIMIT", &cfg.RateLimit)
	addString("GDL_PROXY", &cfg.Proxy)
	addBool("GDL_FORCE", &cfg.Force)
	addBool("GDL_QUIET", &cfg.Quiet)
	addBool("GDL_NO_CLOBBER", &cfg.NoClobber)
	addBool("GDL_TIMESTAMPING", &cfg.Conditional)
	addString("GDL_SUM", &cfg.Sum)
	addString("GDL_USER_AGENT", &cfg.UserAgent)
	addHeaders("GDL_HEADER", &cfg.Headers)
	addBool("GDL_INSECURE", &cfg.Insecure)
	addString("GDL_CA_CERT", &cfg.CACert)
	addHTTP2("GDL_HTTP2")
	addMirrors("GDL_MIRRORS", &cfg.Mirrors)
	addString("GDL_PROGRESS_STYLE", &cfg.ProgressStyle)
	addString("GDL_COLOR", &cfg.Color)
	addInt("GDL_JOBS", &cfg.Jobs)
	addString("GDL_USER", &cfg.User)
	addString("GDL_PASSWORD", &cfg.Password)
	addString("GDL_BEARER", &cfg.Bearer)
	addString("GDL_COOKIE", &cfg.Cookie)
	addString("GDL_COOKIE_JAR", &cfg.CookieJar)
	addString("GDL_LOAD_COOKIES", &cfg.LoadCookies)
	addString("GDL_CERT", &cfg.CertFile)
	addString("GDL_KEY", &cfg.KeyFile)
	addString("GDL_INTERFACE", &cfg.Interface)
	addInt("GDL_MAX_REDIRS", &cfg.MaxRedirs)
	addSize("GDL_MAX_FILESIZE", &cfg.MaxFilesize)
	addSize("GDL_MIN_FILESIZE", &cfg.MinFilesize)
	addSize("GDL_SPEED_LIMIT", &cfg.SpeedLimit)
	addDur("GDL_SPEED_TIME", &cfg.SpeedTime)

	for _, e := range envs {
		if v := os.Getenv(e.key); v != "" {
			e.set(v)
		}
	}
}

// MergeCLI 把命令行解析后的非零值覆盖到 cfg
func (c *Config) MergeCLI(cli *Config) {
	if cli.Connections > 0 {
		c.Connections = cli.Connections
	}
	if cli.Retries >= 0 {
		c.Retries = cli.Retries
	}
	if cli.Timeout > 0 {
		c.Timeout = cli.Timeout
	}
	if cli.RateLimit != "" {
		c.RateLimit = cli.RateLimit
	}
	if cli.Proxy != "" {
		c.Proxy = cli.Proxy
	}
	if cli.Force {
		c.Force = true
	}
	if cli.Quiet {
		c.Quiet = true
	}
	if cli.Stdout {
		c.Stdout = true
	}
	if cli.NoClobber {
		c.NoClobber = true
	}
	if cli.Conditional {
		c.Conditional = true
	}
	if cli.Jobs > 0 {
		c.Jobs = cli.Jobs
	}
	if cli.Sum != "" {
		c.Sum = cli.Sum
	}
	if cli.UserAgent != "" {
		c.UserAgent = cli.UserAgent
	}
	if len(cli.Headers) > 0 {
		c.Headers = cli.Headers
	}
	if cli.Insecure {
		c.Insecure = true
	}
	if cli.CACert != "" {
		c.CACert = cli.CACert
	}
	if cli.HTTP2 != nil {
		c.HTTP2 = cli.HTTP2
	}
	if len(cli.Mirrors) > 0 {
		c.Mirrors = cli.Mirrors
	}
	if cli.ProgressStyle != "" {
		c.ProgressStyle = cli.ProgressStyle
	}
	if cli.Color != "" {
		c.Color = cli.Color
	}
	if cli.User != "" {
		c.User = cli.User
	}
	if cli.Password != "" {
		c.Password = cli.Password
	}
	if cli.Bearer != "" {
		c.Bearer = cli.Bearer
	}
	if cli.Cookie != "" {
		c.Cookie = cli.Cookie
	}
	if cli.CookieJar != "" {
		c.CookieJar = cli.CookieJar
	}
	if cli.LoadCookies != "" {
		c.LoadCookies = cli.LoadCookies
	}
	if cli.CertFile != "" {
		c.CertFile = cli.CertFile
	}
	if cli.KeyFile != "" {
		c.KeyFile = cli.KeyFile
	}
	if cli.IPVersion != 0 {
		c.IPVersion = cli.IPVersion
	}
	if cli.Interface != "" {
		c.Interface = cli.Interface
	}
	if cli.MaxRedirs != 0 {
		c.MaxRedirs = cli.MaxRedirs
	}
	if cli.MaxFilesize != 0 {
		c.MaxFilesize = cli.MaxFilesize
	}
	if cli.MinFilesize != 0 {
		c.MinFilesize = cli.MinFilesize
	}
	if cli.SpeedLimit != 0 {
		c.SpeedLimit = cli.SpeedLimit
	}
	if cli.SpeedTime != 0 {
		c.SpeedTime = cli.SpeedTime
	}
	if len(cli.URLs) > 0 {
		c.URLs = cli.URLs
	}
	if cli.Output != "" {
		c.Output = cli.Output
	}
	if cli.WriteOut != "" {
		c.WriteOut = cli.WriteOut
	}
}

// ToDownloadOptions 转换为 downloader 用的 Options
func (c *Config) ToDownloadOptions() download.Options {
	hdr := make(http.Header)
	for _, h := range c.Headers {
		if k, v, ok := strings.Cut(h, ":"); ok {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return download.Options{
		Connections: c.Connections,
		Timeout:     c.Timeout,
		Retries:     c.Retries,
		Force:       c.Force,
		NoClobber:   c.NoClobber,
		Quiet:       c.Quiet,
		UserAgent:   c.UserAgent,
		Header:      hdr,
		Sum:         c.Sum,
		RateLimit:   c.RateLimit,
		Proxy:       c.Proxy,
		Insecure:    c.Insecure,
		CACert:      c.CACert,
		HTTP2:       c.HTTP2,
		Mirrors:     c.Mirrors,

		Conditional: c.Conditional,
		User:        c.User,
		Password:    c.Password,
		Bearer:      c.Bearer,
		Cookie:      c.Cookie,
		CertFile:    c.CertFile,
		KeyFile:     c.KeyFile,

		MaxFilesize: c.MaxFilesize.Int64(),
		MinFilesize: c.MinFilesize.Int64(),
		SpeedLimit:  c.SpeedLimit.Int64(),
		SpeedTime:   c.SpeedTime,
		MaxRedirs:   c.MaxRedirs,
		IPVersion:   c.IPVersion,
		Interface:   c.Interface,
	}
}
