package http

import (
	"net/http"
	"strings"

	"github.com/godw/godw/internal/pkg/errs"
)

// 数据文档站接口。只读，不落库：所有内容都由字典、质量、血缘、运行记录实时拼出来。
// 这样「文档」不会和事实脱钩——没有第二份需要同步的数据。

func (h *Handler) catalogDocs(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	page, err := h.Svc.CatalogDocs(r.Context(), workspaceOf(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, r, page)
}

// catalogExport 导出单文件文档站。
// 默认 inline（可直接在新标签页打开），download=1 时作为附件下载。
func (h *Handler) catalogExport(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "html"
	}
	inline := r.URL.Query().Get("download") != "1"
	disposition := "inline"
	if !inline {
		disposition = "attachment"
	}

	switch format {
	case "md", "markdown":
		md, err := h.Svc.DictionaryMarkdown(r.Context(), workspaceOf(r.Context()))
		if err != nil {
			fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", disposition+`; filename="dictionary.md"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(md))
	case "html":
		page, err := h.Svc.CatalogHTML(r.Context(), workspaceOf(r.Context()))
		if err != nil {
			fail(w, r, err)
			return
		}
		h.audit(r, "catalog.export", "html", map[string]any{"bytes": len(page)})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", disposition+`; filename="catalog.html"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(page))
	default:
		fail(w, r, errs.New(errs.CodeInvalidArgument, "不支持的导出格式: "+format+"（html / md）"))
	}
}
