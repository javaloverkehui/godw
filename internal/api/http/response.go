package http

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/jsonx"
)

// Response 是所有接口的统一响应体。前端只需判断 code==0。
type Response struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Page 是分页结果的标准形状。
type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, r *http.Request, data any) {
	writeJSON(w, http.StatusOK, Response{
		Code: 0, Message: "ok", Data: data, RequestID: requestID(r.Context()),
	})
}

func created(w http.ResponseWriter, r *http.Request, data any) {
	writeJSON(w, http.StatusCreated, Response{
		Code: 0, Message: "created", Data: data, RequestID: requestID(r.Context()),
	})
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	ae := errs.From(err)
	// 5xx 一律留痕：对外仍是笼统文案，服务端日志里保留根因。
	if ae.Code.HTTPStatus() >= 500 {
		errs.Report(err)
	}
	writeJSON(w, ae.Code.HTTPStatus(), Response{
		Code: int(ae.Code), Message: ae.Msg, Data: ae.Meta, RequestID: requestID(r.Context()),
	})
}

func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "请求体解析失败")
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// paginate 对内存切片做分页，规模小的列表统一走它。
func paginate[T any](items []T, page, size int) Page[T] {
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	total := len(items)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return Page[T]{Items: items[start:end], Total: total, Page: page, PageSize: size}
}

var _ = jsonx.Marshal
var _ = strings.TrimSpace
