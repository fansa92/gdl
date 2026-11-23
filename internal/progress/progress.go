// Package progress 提供一个简单的终端进度条。
package progress

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tickEvery = 120 * time.Millisecond
	spinners  = `-\|/`
)

// Style 进度条风格
type Style int

const (
	StyleAuto Style = iota
	StyleASCII
	StyleUnicode
	StyleNone
)

// ColorMode 颜色模式
type ColorMode int

const (
	ColorAuto ColorMode = iota
	ColorAlways
	ColorNever
)

// 全局配置（由 main.go 初始化）
var (
	globalStyle Style     = StyleAuto
	globalColor ColorMode = ColorAuto
	termWidth   int       = detectTermWidth()
)

func detectTermWidth() int {
	// 简易检测：Windows 用 80，Unix 尝试从 stty 获取
	if runtime.GOOS == "windows" {
		return 80
	}
	// 可以用 `stty size` 或 terminfo，这里简化
	return 80
}

// SetGlobalStyle 设置全局进度条风格
func SetGlobalStyle(s string) {
	switch strings.ToLower(s) {
	case "ascii":
		globalStyle = StyleASCII
	case "unicode":
		globalStyle = StyleUnicode
	case "none":
		globalStyle = StyleNone
	default:
		globalStyle = StyleAuto
	}
}

// SetGlobalColor 设置全局颜色模式
func SetGlobalColor(s string) {
	switch strings.ToLower(s) {
	case "always":
		globalColor = ColorAlways
	case "never":
		globalColor = ColorNever
	default:
		globalColor = ColorAuto
	}
}

// shouldColor 判断是否启用颜色
func shouldColor() bool {
	switch globalColor {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default:
		// auto: 检测是否为 TTY
		return isTerminal(os.Stderr)
	}
}

func isTerminal(f *os.File) bool {
	// 简易检测：Go 1.22+ 有 IsTerminal，这里用简单启发式
	// 实际可用 golang.org/x/term
	return false // 暂时关闭颜色，避免乱码
}

// Bar 是一个渲染到终端的进度条。所有方法都支持并发调用。
type Bar struct {
	total atomic.Int64
	done  atomic.Int64
	out   io.Writer
	quiet bool

	start time.Time
	speed float64
	lastT time.Time
	lastD int64

	mu      sync.Mutex
	lastLen int
	stopped bool

	stopOnce sync.Once
	stopCh   chan struct{}

	style Style
	color ColorMode
}

// New 创建一个进度条。total <= 0 表示总大小未知。
func New(total int64, out io.Writer, quiet bool) *Bar {
	b := &Bar{
		out:    out,
		quiet:  quiet,
		start:  time.Now(),
		stopCh: make(chan struct{}),
		style:  effectiveStyle(),
		color:  effectiveColor(),
	}
	if total > 0 {
		b.total.Store(total)
	}
	if !quiet && out != nil {
		go b.loop()
	}
	return b
}

func effectiveStyle() Style {
	if globalStyle != StyleAuto {
		return globalStyle
	}
	// auto: 根据终端能力选择
	if runtime.GOOS == "windows" {
		return StyleASCII // Windows 终端兼容性更好用 ASCII
	}
	return StyleUnicode
}

func effectiveColor() ColorMode {
	if globalColor != ColorAuto {
		return globalColor
	}
	return ColorNever // 暂时默认关闭
}

// SetTotal 设置（或修正）总字节数，n <= 0 表示总大小未知。
func (b *Bar) SetTotal(n int64) {
	if n < 0 {
		n = 0
	}
	b.total.Store(n)
}

// Add 增加已完成字节数，允许为负数（用于回退）。
func (b *Bar) Add(n int64) {
	b.done.Add(n)
}

// Done 返回已完成字节数。
func (b *Bar) Done() int64 {
	return b.done.Load()
}

func (b *Bar) loop() {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.mu.Lock()
			b.draw(false)
			b.mu.Unlock()
		case <-b.stopCh:
			return
		}
	}
}

// Stop 结束进度条并换行。可以多次调用。
func (b *Bar) Stop() {
	b.stopOnce.Do(func() { close(b.stopCh) })
	if b.quiet || b.out == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.draw(true)
	fmt.Fprintln(b.out)
	b.stopped = true
}

// draw 调用方必须持有 b.mu。
func (b *Bar) draw(final bool) {
	if b.quiet || b.out == nil {
		return
	}
	if b.stopped && !final {
		return
	}
	done, total := b.done.Load(), b.total.Load()
	now := time.Now()
	if b.lastT.IsZero() {
		// 首帧只记录基准，避免把续传的已有进度算进瞬时速度
		b.lastT, b.lastD = now, done
	} else if dt := now.Sub(b.lastT).Seconds(); dt > 0 {
		inst := float64(done-b.lastD) / dt
		if b.speed == 0 {
			b.speed = inst
		} else {
			b.speed = b.speed*0.6 + inst*0.4
		}
		b.lastT, b.lastD = now, done
	}
	if b.speed < 0 {
		b.speed = 0
	}
	elapsed := now.Sub(b.start).Seconds()
	if elapsed <= 0 {
		elapsed = 1e-3
	}
	if final && done > 0 {
		// 收尾帧用平均速度，避免最后一次采样间隔太短显示成 0 B/s
		b.speed = float64(done) / elapsed
	}

	line := b.formatLine(done, total, b.speed, elapsed)
	pad := b.lastLen - len(line)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(b.out, "\r%s%s", line, strings.Repeat(" ", pad))
	b.lastLen = len(line)
}

func (b *Bar) formatLine(done, total int64, speed, elapsed float64) string {
	sp := FormatBytes(int64(speed)) + "/s"
	if total > 0 {
		if done > total {
			done = total
		}
		pct := float64(done) / float64(total) * 100

		var bar string
		switch b.style {
		case StyleUnicode:
			bar = unicodeBar(done, total)
		case StyleNone:
			bar = ""
		default: // ASCII
			filled := int(float64(done) / float64(total) * 24)
			if filled > 24 {
				filled = 24
			}
			if filled < 0 {
				filled = 0
			}
			bar = strings.Repeat("█", filled) + strings.Repeat("░", 24-filled)
		}

		eta := ""
		if speed > 0 && done < total {
			eta = "  eta " + FormatDuration(float64(total-done)/speed)
		}

		if b.style == StyleNone {
			return fmt.Sprintf("%5.1f%%  %s / %s  %s%s", pct, FormatBytes(done), FormatBytes(total), sp, eta)
		}
		return fmt.Sprintf("[%s] %5.1f%%  %s / %s  %s%s", bar, pct, FormatBytes(done), FormatBytes(total), sp, eta)
	}

	spin := spinners[int(elapsed*8)%len(spinners)]
	return fmt.Sprintf("%c  %s  %s", spin, FormatBytes(done), sp)
}

func unicodeBar(done, total int64) string {
	const width = 24
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := int(float64(done) / float64(total) * width)
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	// 使用分数块字符实现更平滑的进度条
	full := strings.Repeat("█", filled)
	empty := strings.Repeat("░", width-filled)
	return full + empty
}

// FormatBytes 把字节数格式化为人类可读形式，例如 "12.3 MB"。
func FormatBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		n = 0
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatDuration 把秒数格式化为 "45s" / "3m07s" / "1h02m03s"。
func FormatDuration(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	s := int(sec + 0.5)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm%02ds", s/3600, s/60%60, s%60)
	}
}
