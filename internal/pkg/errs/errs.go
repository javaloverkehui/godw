package errs

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Code 是稳定的业务错误码。分段规则与 API 文档一致：
//
//	10xxx 通用/参数    20xxx 数据源与连接
//	30xxx 模型与管道   40xxx 调度与执行
//	50xxx 权限与系统
type Code int

const (
	OK Code = 0

	CodeInvalidArgument Code = 10001
	CodeNotFound        Code = 10002
	CodeConflict        Code = 10003
	CodeInternal        Code = 10004
	CodeUnavailable     Code = 10005

	CodeDatasourceUnreachable Code = 20001
	CodeDatasourceAuth        Code = 20002
	CodeDatasourceDuplicate   Code = 20003
	CodeMetadataCollect       Code = 20004

	CodeModelInvalid    Code = 30001
	CodeModelImmutable  Code = 30002
	CodePipelineCycle   Code = 30003
	CodePipelineInvalid Code = 30004
	CodeCompileFailed   Code = 30005

	CodeInstanceConflict Code = 40001
	CodeLeaseLost        Code = 40002
	CodeExecutionFailed  Code = 40003
	CodeQualityGate      Code = 40004

	CodeUnauthorized Code = 50001
	CodeForbidden    Code = 50002
)

var messages = map[Code]string{
	CodeInvalidArgument:       "参数不合法",
	CodeNotFound:              "资源不存在",
	CodeConflict:              "资源冲突",
	CodeInternal:              "服务内部错误",
	CodeUnavailable:           "服务暂不可用",
	CodeDatasourceUnreachable: "数据源连接失败",
	CodeDatasourceAuth:        "数据源认证失败",
	CodeDatasourceDuplicate:   "数据源已存在",
	CodeMetadataCollect:       "元数据采集失败",
	CodeModelInvalid:          "模型定义不合法",
	CodeModelImmutable:        "模型已发布，需新建版本",
	CodePipelineCycle:         "管道存在环",
	CodePipelineInvalid:       "管道定义不合法",
	CodeCompileFailed:         "编译失败",
	CodeInstanceConflict:      "实例状态冲突",
	CodeLeaseLost:             "执行租约已失效",
	CodeExecutionFailed:       "任务执行失败",
	CodeQualityGate:           "质量门禁未通过",
	CodeUnauthorized:          "未认证",
	CodeForbidden:             "无权限",
}

// Message 返回错误码的默认可读文案。
func (c Code) Message() string {
	if m, ok := messages[c]; ok {
		return m
	}
	return "未知错误"
}

// HTTPStatus 把业务码映射为 HTTP 状态码，保证前端只需看 code 字段。
func (c Code) HTTPStatus() int {
	switch c {
	case OK:
		return http.StatusOK
	case CodeInvalidArgument, CodeModelInvalid, CodePipelineInvalid, CodePipelineCycle, CodeCompileFailed:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeDatasourceDuplicate, CodeInstanceConflict, CodeModelImmutable:
		return http.StatusConflict
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeUnavailable, CodeLeaseLost:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// AppError 是贯穿全链路的错误类型。Cause 保留底层错误供 errors.Is/As 使用，
// Meta 携带定位信息（字段名、数据源 ID、SQL 片段等），不对外暴露敏感内容。
type AppError struct {
	Code  Code           `json:"code"`
	Msg   string         `json:"message"`
	Cause error          `json:"-"`
	Meta  map[string]any `json:"meta,omitempty"`
}

func (e *AppError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Msg, e.Cause)
	}
	// 校验类错误把逐条问题摊平，避免调用方只能看到笼统的「不合法」。
	if p, ok := e.Meta["problems"].([]string); ok && len(p) > 0 {
		return fmt.Sprintf("[%d] %s: %s", e.Code, e.Msg, strings.Join(p, "; "))
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Msg)
}

func (e *AppError) Unwrap() error { return e.Cause }

// WithMeta 追加定位信息，返回自身便于链式调用。
func (e *AppError) WithMeta(k string, v any) *AppError {
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[k] = v
	return e
}

// New 构造一个应用错误，Msg 为空时取错误码默认文案。
func New(code Code, msg string) *AppError {
	if msg == "" {
		msg = code.Message()
	}
	return &AppError{Code: code, Msg: msg}
}

// Wrap 包装底层错误，保留 Cause 链。
func Wrap(code Code, cause error, msg string) *AppError {
	e := New(code, msg)
	e.Cause = cause
	return e
}

// From 把任意错误归一化为 AppError；已是 AppError 则原样返回。
func From(err error) *AppError {
	if err == nil {
		return nil
	}
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	return Wrap(CodeInternal, err, "")
}

// Is 判断错误链中是否包含指定业务码。
func Is(err error, code Code) bool {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae.Code == code
	}
	return false
}

// Hook 是 5xx 错误的旁路记录点。
//
// 对外只回「服务内部错误」是对的——不该把 SQL 细节暴露给调用方；
// 但服务端必须留下完整原因，否则线上只能看到 500 而无从下手。
// HTTP 层在 fail() 里触发它，main 负责接到 logger 上。
var Hook func(error)

// Report 在有 Hook 时上报错误，供 HTTP 层调用。
func Report(err error) {
	if err != nil && Hook != nil {
		Hook(err)
	}
}
