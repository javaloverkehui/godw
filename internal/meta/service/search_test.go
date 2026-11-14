package service

import (
	"strings"
	"testing"
)

// highlight 是搜索卡片最关键的一段：既要命中中文子串，又不能把用户输入
// 原样拼进 HTML。这里把两个约束都钉住。
func TestHighlight(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		keyword string
		want    string
		wantEm  bool
	}{
		{
			name: "中文子串命中整词内部", text: "订单明细表", keyword: "订单",
			wantEm: true,
		},
		{
			name: "英文大小写不敏感", text: "dwd_order_detail", keyword: "ORDER",
			wantEm: true,
		},
		{
			name: "未命中返回空", text: "订单明细表", keyword: "库存",
			want: "",
		},
		{
			name: "空关键词返回空", text: "订单明细表", keyword: "  ",
			want: "",
		},
		{
			name: "长文本截断并加省略号", text: strings.Repeat("前缀", 40) + "订单" + strings.Repeat("后缀", 40),
			keyword: "订单", wantEm: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := highlight(c.text, c.keyword)
			if c.want != "" || !c.wantEm {
				if got != c.want {
					t.Fatalf("highlight(%q,%q)=%q want %q", c.text, c.keyword, got, c.want)
				}
				return
			}
			if !strings.Contains(got, "<em>") || !strings.Contains(got, "</em>") {
				t.Fatalf("应包含高亮标记: %q", got)
			}
			// 高亮标签内必须是原文关键词（大小写按原文）。
			lower := strings.ToLower(got)
			kw := strings.ToLower(c.keyword)
			if !strings.Contains(lower, "<em>"+kw+"</em>") {
				t.Fatalf("高亮内容不对: %q", got)
			}
		})
	}
}

func TestHighlightEscapesHTML(t *testing.T) {
	got := highlight(`<script>alert(1)</script> 用户表`, "用户")
	if strings.Contains(got, "<script") {
		t.Fatalf("未转义脚本标签: %q", got)
	}
	// 片段窗口按命中位置居中，前面的 script 起始标签可能被截掉，
	// 但被保留下来的部分必须是转义后的文本，不能变成真实标签。
	if strings.Contains(got, "</script>") {
		t.Fatalf("未转义闭合标签: %q", got)
	}
	if !strings.Contains(got, "&lt;/script&gt;") {
		t.Fatalf("应保留转义后的可见文本: %q", got)
	}
	if !strings.Contains(got, "<em>用户</em>") {
		t.Fatalf("转义后仍应高亮: %q", got)
	}
}

// 高亮片段不能从 UTF-8 字符中间切开，否则前端显示乱码。
func TestHighlightUTF8Boundary(t *testing.T) {
	text := strings.Repeat("数据仓库", 30) + "订单" + strings.Repeat("明细", 30)
	got := highlight(text, "订单")
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("出现替换字符，说明切坏了 UTF-8: %q", got)
		}
	}
}
