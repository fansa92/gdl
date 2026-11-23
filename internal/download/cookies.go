package download

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// NewJar 创建一个遵循公共后缀列表的 cookie 罐。
func NewJar() *cookiejar.Jar {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil
	}
	return jar
}

// LoadCookiesFile 读取 Netscape 格式的 cookie 文件（curl -b / wget --load-cookies 的格式）。
func LoadCookiesFile(path string, jar *cookiejar.Jar) error {
	if jar == nil {
		return fmt.Errorf("cookie 罐未初始化")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var (
		sc      = bufio.NewScanner(f)
		byHost  = map[string][]*http.Cookie{}
		schemes = map[string]string{}
		line    int
	)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "Set-Cookie:") {
			// 宽松兼容：直接喂给 http.ReadSetCookies 不方便，跳过
			continue
		}
		parts := strings.Split(s, "\t")
		if len(parts) < 7 {
			continue
		}
		domain := strings.TrimSpace(parts[0])
		includeSub := strings.EqualFold(strings.TrimSpace(parts[1]), "TRUE")
		cpath := strings.TrimSpace(parts[2])
		secure := strings.EqualFold(strings.TrimSpace(parts[3]), "TRUE")
		exp, _ := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		name := parts[5]
		value := strings.Join(parts[6:], "\t")
		if domain == "" || name == "" {
			continue
		}
		if cpath == "" {
			cpath = "/"
		}
		host := strings.TrimPrefix(domain, ".")
		c := &http.Cookie{
			Name:   name,
			Value:  value,
			Path:   cpath,
			Secure: secure,
		}
		if includeSub {
			c.Domain = host
		}
		if exp > 0 {
			c.Expires = time.Unix(exp, 0)
		}
		byHost[host] = append(byHost[host], c)
		scheme := "http"
		if secure {
			scheme = "https"
		}
		schemes[host] = scheme
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for host, cs := range byHost {
		u := &url.URL{Scheme: schemes[host], Host: host, Path: "/"}
		jar.SetCookies(u, cs)
	}
	return nil
}

// SaveCookiesFile 把各 URL 域下的 cookie 写成 Netscape 格式，供下次 --load-cookies 复用。
func SaveCookiesFile(path string, jar *cookiejar.Jar, urls []string) error {
	if jar == nil {
		return fmt.Errorf("cookie 罐未初始化")
	}
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	b.WriteString("# 由 gdl 生成\n\n")
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		host := u.Hostname()
		if seen[host] {
			continue
		}
		seen[host] = true
		for _, c := range jar.Cookies(u) {
			exp := int64(0)
			if !c.Expires.IsZero() {
				exp = c.Expires.Unix()
			}
			secure := "FALSE"
			if c.Secure {
				secure = "TRUE"
			}
			domain := host
			if c.Domain != "" {
				domain = "." + strings.TrimPrefix(c.Domain, ".")
			}
			cpath := c.Path
			if cpath == "" {
				cpath = "/"
			}
			fmt.Fprintf(&b, "%s\tTRUE\t%s\t%s\t%d\t%s\t%s\n", domain, cpath, secure, exp, c.Name, c.Value)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
