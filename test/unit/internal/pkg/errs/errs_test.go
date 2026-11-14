package errs_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/godw/godw/internal/pkg/errs"
)

// 守住错误码到文案的映射：新增码忘记登记时必须落到「未知错误」，
// 而不是返回空串让前端显示空白。
func TestCodeMessage(t *testing.T) {
	cases := []struct {
		name string
		code errs.Code
		want string
	}{
		{"缺省文案-参数", errs.CodeInvalidArgument, "参数不合法"},
		{"缺省文案-租约", errs.CodeLeaseLost, "执行租约已失效"},
		{"缺省文案-鉴权", errs.CodeForbidden, "无权限"},
		{"未登记的错误码", errs.Code(99999), "未知错误"},
		{"OK 无文案", errs.OK, "未知错误"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.code.Message(); got != tc.want {
				t.Fatalf("Code(%d).Message() = %q，期望 %q", tc.code, got, tc.want)
			}
		})
	}
}

// 守住 HTTP 状态映射：每个分支都要给出明确状态码，
// 遗漏会让客户端拿到 500 而误判为服务端故障。
func TestCodeHTTPStatus(t *testing.T) {
	cases := []struct {
		name string
		code errs.Code
		want int
	}{
		{"成功", errs.OK, http.StatusOK},
		{"参数错误", errs.CodeInvalidArgument, http.StatusBadRequest},
		{"模型不合法", errs.CodeModelInvalid, http.StatusBadRequest},
		{"管道不合法", errs.CodePipelineInvalid, http.StatusBadRequest},
		{"管道有环", errs.CodePipelineCycle, http.StatusBadRequest},
		{"编译失败", errs.CodeCompileFailed, http.StatusBadRequest},
		{"资源不存在", errs.CodeNotFound, http.StatusNotFound},
		{"资源冲突", errs.CodeConflict, http.StatusConflict},
		{"数据源重复", errs.CodeDatasourceDuplicate, http.StatusConflict},
		{"实例冲突", errs.CodeInstanceConflict, http.StatusConflict},
		{"模型不可变", errs.CodeModelImmutable, http.StatusConflict},
		{"未认证", errs.CodeUnauthorized, http.StatusUnauthorized},
		{"无权限", errs.CodeForbidden, http.StatusForbidden},
		{"服务不可用", errs.CodeUnavailable, http.StatusServiceUnavailable},
		{"租约丢失", errs.CodeLeaseLost, http.StatusServiceUnavailable},
		{"内部错误走默认分支", errs.CodeInternal, http.StatusInternalServerError},
		{"未登记的码走默认分支", errs.Code(99999), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.code.HTTPStatus(); got != tc.want {
				t.Fatalf("Code(%d).HTTPStatus() = %d，期望 %d", tc.code, got, tc.want)
			}
		})
	}
}

// 守住 Error() 的三种排版：带 Cause、带 problems 明细、纯文案。
// 若 problems 分支失效，参数校验错误会退化成笼统提示，调用方无法定位字段。
func TestAppErrorFormatting(t *testing.T) {
	cases := []struct {
		name string
		err  *errs.AppError
		want string
	}{
		{
			name: "带底层错误",
			err:  errs.Wrap(errs.CodeExecutionFailed, errors.New("boom"), "执行失败"),
			want: "[40003] 执行失败: boom",
		},
		{
			name: "带 problems 明细",
			err:  errs.New(errs.CodeInvalidArgument, "").WithMeta("problems", []string{"name 必填", "age 非法"}),
			want: "[10001] 参数不合法: name 必填; age 非法",
		},
		{
			name: "problems 为空切片时退回纯文案",
			err:  errs.New(errs.CodeInvalidArgument, "").WithMeta("problems", []string{}),
			want: "[10001] 参数不合法",
		},
		{
			name: "problems 类型不对时不展开",
			err:  errs.New(errs.CodeInvalidArgument, "").WithMeta("problems", "name 必填"),
			want: "[10001] 参数不合法",
		},
		{
			name: "仅文案",
			err:  errs.New(errs.CodeNotFound, ""),
			want: "[10002] 资源不存在",
		},
		{
			name: "自定义文案优先",
			err:  errs.New(errs.CodeNotFound, "模型 42 不存在"),
			want: "[10002] 模型 42 不存在",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 守住 Unwrap：errors.Is 必须能穿透 AppError 找到根因，
// 否则上层按哨兵错误判断（如 sql.ErrNoRows）会全部失效。
func TestAppErrorUnwrap(t *testing.T) {
	cause := errors.New("conn refused")
	e := errs.Wrap(errs.CodeDatasourceUnreachable, cause, "")
	if !errors.Is(e, cause) {
		t.Fatal("AppError 应能通过 errors.Is 命中 Cause")
	}
	if e.Error() != "[20001] 数据源连接失败: conn refused" {
		t.Fatalf("Wrap 空文案应取默认文案，实际 %q", e.Error())
	}
}

// 守住 WithMeta 的惰性初始化与链式返回：nil map 直接赋值会 panic，
// 返回新对象则会让链式调用丢失前面写入的字段。
func TestWithMeta(t *testing.T) {
	e := &errs.AppError{Code: errs.CodeInvalidArgument, Msg: "x"}
	got := e.WithMeta("field", "name").WithMeta("value", 3)
	if got != e {
		t.Fatal("WithMeta 应返回自身以支持链式调用")
	}
	if len(e.Meta) != 2 || e.Meta["field"] != "name" || e.Meta["value"] != 3 {
		t.Fatalf("Meta 内容不符: %#v", e.Meta)
	}
	// 已有 map 时不应被重置
	e.WithMeta("field", "title")
	if e.Meta["field"] != "title" || len(e.Meta) != 2 {
		t.Fatalf("二次写入应覆盖同键且保留其它键: %#v", e.Meta)
	}
}

// 守住 From 的归一化：nil 必须返回 nil（调用方普遍直接 return From(err)），
// 已是 AppError 必须原样返回，避免错误码被降级成 500。
func TestFrom(t *testing.T) {
	if got := errs.From(nil); got != nil {
		t.Fatalf("From(nil) 应为 nil，实际 %v", got)
	}

	original := errs.New(errs.CodeConflict, "已存在")
	if got := errs.From(original); got != original {
		t.Fatal("From 对 AppError 应原样返回同一指针")
	}

	wrapped := fmt.Errorf("外层包装: %w", original)
	if got := errs.From(wrapped); got != original {
		t.Fatal("From 应通过 errors.As 穿透包装返回原 AppError")
	}

	cause := errors.New("disk full")
	plain := errs.From(cause)
	if plain == nil {
		t.Fatal("普通 error 不应返回 nil")
	}
	if plain.Code != errs.CodeInternal {
		t.Fatalf("普通 error 应归一为 CodeInternal，实际 %d", plain.Code)
	}
	if plain.Msg != errs.CodeInternal.Message() {
		t.Fatalf("空文案应取默认文案，实际 %q", plain.Msg)
	}
	if !errors.Is(plain, cause) {
		t.Fatal("归一后的 AppError 应保留 Cause")
	}
}

// 守住 Is 只按业务码判断：非 AppError 必须返回 false，
// 否则上层的「资源不存在则新建」分支会被任意错误误触发。
func TestIs(t *testing.T) {
	e := errs.Wrap(errs.CodeNotFound, errors.New("no rows"), "")
	if !errs.Is(e, errs.CodeNotFound) {
		t.Fatal("应命中包装后的业务码")
	}
	if !errs.Is(fmt.Errorf("wrap: %w", e), errs.CodeNotFound) {
		t.Fatal("应穿透 fmt.Errorf 包装命中业务码")
	}
	if errs.Is(e, errs.CodeConflict) {
		t.Fatal("不同业务码不应命中")
	}
	if errs.Is(errors.New("plain"), errs.CodeNotFound) {
		t.Fatal("非 AppError 不应命中任何业务码")
	}
	if errs.Is(nil, errs.CodeNotFound) {
		t.Fatal("nil 不应命中任何业务码")
	}
}

// 守住 Report 的副作用边界：nil 错误不能上报（否则日志全是噪音），
// 未接线 Hook 时也不能 panic（单元测试与 CLI 场景常见）。
func TestReport(t *testing.T) {
	old := errs.Hook
	t.Cleanup(func() { errs.Hook = old })

	errs.Hook = nil
	errs.Report(errors.New("无人监听"))
	errs.Report(nil) // 兜底：nil 错误不应触发上报

	var got []error
	errs.Hook = func(err error) { got = append(got, err) }

	target := errs.New(errs.CodeNotFound, "")
	errs.Report(target)
	errs.Report(nil)
	if len(got) != 1 || got[0] != target {
		t.Fatalf("Hook 应只收到一次非 nil 错误，实际 %#v", got)
	}
}
