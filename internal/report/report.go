// Package report 实现 --write-out 模板渲染。
package report

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Fields 是模板可引用的数据。
type Fields struct {
	URL         string
	Path        string
	Size        int64
	Duration    time.Duration
	Status      int
	Sum         string
	Resumed     bool
	NotModified bool
	Skipped     bool
	ExitCode    int
	Err         error
}

func (f Fields) speed() float64 {
	if f.Duration <= 0 {
		return 0
	}
	return float64(f.Size) / f.Duration.Seconds()
}

func (f Fields) tokens() map[string]string {
	errMsg := ""
	if f.Err != nil {
		errMsg = f.Err.Error()
	}
	return map[string]string{
		"http_code":          strconv.Itoa(f.Status),
		"response_code":      strconv.Itoa(f.Status),
		"size_download":      strconv.FormatInt(f.Size, 10),
		"time_total":         fmt.Sprintf("%.3f", f.Duration.Seconds()),
		"speed_download":     fmt.Sprintf("%.0f", f.speed()),
		"url_effective":      f.URL,
		"filename_effective": f.Path,
		"checksum":           f.Sum,
		"resumed":            boolStr(f.Resumed),
		"not_modified":       boolStr(f.NotModified),
		"skipped":            boolStr(f.Skipped),
		"exitcode":           strconv.Itoa(f.ExitCode),
		"errormsg":           errMsg,
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Format 渲染模板。支持 %{name} 占位符与 \n \t \r \\ 转义；未知占位符原样输出。
func Format(format string, f Fields) string {
	if format == "" {
		return ""
	}
	tokens := f.tokens()
	var b strings.Builder
	for i := 0; i < len(format); {
		c := format[i]
		if c == '\\' && i+1 < len(format) {
			switch format[i+1] {
			case 'n':
				b.WriteByte('\n')
				i += 2
				continue
			case 't':
				b.WriteByte('\t')
				i += 2
				continue
			case 'r':
				b.WriteByte('\r')
				i += 2
				continue
			case '\\':
				b.WriteByte('\\')
				i += 2
				continue
			}
		}
		if c == '%' && i+1 < len(format) {
			if format[i+1] == '%' {
				b.WriteByte('%')
				i += 2
				continue
			}
			if format[i+1] == '{' {
				if end := strings.IndexByte(format[i+2:], '}'); end >= 0 {
					name := format[i+2 : i+2+end]
					if name == "json" {
						b.WriteString(f.json())
					} else if v, ok := tokens[name]; ok {
						b.WriteString(v)
					} else {
						b.WriteString(format[i : i+2+end+1])
					}
					i += 2 + end + 1
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func (f Fields) json() string {
	errMsg := ""
	if f.Err != nil {
		errMsg = f.Err.Error()
	}
	payload := map[string]any{
		"url":                f.URL,
		"filename_effective": f.Path,
		"http_code":          f.Status,
		"size_download":      f.Size,
		"time_total":         f.Duration.Seconds(),
		"speed_download":     f.speed(),
		"checksum":           f.Sum,
		"resumed":            f.Resumed,
		"not_modified":       f.NotModified,
		"skipped":            f.Skipped,
		"exitcode":           f.ExitCode,
		"errormsg":           errMsg,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(data)
}
