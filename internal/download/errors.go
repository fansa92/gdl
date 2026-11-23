package download

import (
	"context"
	"errors"
	"fmt"
)

// 进程退出码。取值参考 curl / wget 的常见约定，便于脚本判断失败原因。
const (
	ExitOK         = 0   // 成功（含 304 未修改、-nc 跳过）
	ExitGeneric    = 1   // 未分类错误
	ExitUsage      = 2   // 用法 / 参数错误
	ExitExists     = 3   // 目标文件已存在且未加 -f
	ExitNetwork    = 4   // 网络、超时、TLS 等传输层错误
	ExitHTTP       = 5   // 服务器返回 4xx/5xx
	ExitVerify     = 6   // 校验失败（哈希、长度）
	ExitLimit      = 7   // 触碰限制（大小上限、速度下限）
	ExitPermission = 8   // 本地文件/目录不可写
	ExitCancelled  = 130 // 用户中断（Ctrl+C）
)

// Error 携带退出码的错误。
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Wrap 给错误附加退出码；已有明确退出码的错误不会被降级覆盖。
func Wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return err
	}
	return &Error{Code: code, Err: err}
}

// Errorf 构造一个带退出码的错误。
func Errorf(code int, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// ExitCode 从错误里取出退出码。
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ExitCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return ExitNetwork
	}
	return ExitGeneric
}

// fatal 判断该错误是否不值得重试（限制类、校验类、用法类错误）。
func fatal(err error) bool {
	var de *Error
	if !errors.As(err, &de) {
		return false
	}
	switch de.Code {
	case ExitUsage, ExitExists, ExitVerify, ExitLimit:
		return true
	}
	return false
}
