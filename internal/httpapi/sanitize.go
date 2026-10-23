package httpapi

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
)

// 正文大小上限：超过 1MB 的 HTML 只保留前 200KB（按字节截断并补齐合法 UTF-8）。
const (
	maxHTMLBody   = 1 << 20
	truncatedHTML = 200 << 10
)

// htmlPolicy 是正文消毒白名单：允许常见排版、链接、图片、表格，移除脚本/表单/事件属性。
var htmlPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(true)
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

var (
	imgTagRe  = regexp.MustCompile("(?is)<img\\b[^>]*>")
	srcAttrRe = regexp.MustCompile("(?is)\\bsrc\\s*=\\s*\"([^\"]*)\"")
	scriptRe  = regexp.MustCompile("(?is)<(script|style)\\b[^>]*>.*?</\\s*(script|style)\\s*>")
	anyTagRe  = regexp.MustCompile("(?s)<[^>]*>")
)

// sanitizeHTML 对邮件正文做白名单消毒，并把远程图片改写成同源代理地址。
func sanitizeHTML(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > maxHTMLBody {
		raw = truncateUTF8(raw, truncatedHTML)
		raw += "<p>[正文过长，已截断]</p>"
	}
	out := htmlPolicy.Sanitize(raw)
	// bluemonday 会为外链添加 rel="nofollow"；再补上 noopener noreferrer。
	out = strings.ReplaceAll(out, "rel=\"nofollow\"", "rel=\"nofollow noopener noreferrer\"")
	return rewriteImageSources(out)
}

// rewriteImageSources 把远程图片改写成 /api/proxy/image?u=...，避免浏览器直连第三方
// 追踪服务器；同时加 loading="lazy" 与 referrerpolicy="no-referrer"。
// 非 http(s) 的 cid:/空地址替换为占位文本，内联 raster data URI 保留。
func rewriteImageSources(in string) string {
	return imgTagRe.ReplaceAllStringFunc(in, func(tag string) string {
		m := srcAttrRe.FindStringSubmatch(tag)
		if len(m) < 2 {
			return tag
		}
		rawSrc := html.UnescapeString(m[1])
		low := strings.ToLower(strings.TrimSpace(rawSrc))
		switch {
		case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
			proxied := "/api/proxy/image?u=" + url.QueryEscape(rawSrc)
			tag = srcAttrRe.ReplaceAllString(tag, "src=\""+proxied+"\"")
			tag = strings.TrimSuffix(tag, ">")
			tag = strings.TrimSuffix(tag, "/")
			tag = strings.TrimRight(tag, " ")
			return tag + " loading=\"lazy\" referrerpolicy=\"no-referrer\">"
		case strings.HasPrefix(low, "data:image/png"), strings.HasPrefix(low, "data:image/jpeg"),
			strings.HasPrefix(low, "data:image/jpg"), strings.HasPrefix(low, "data:image/gif"),
			strings.HasPrefix(low, "data:image/webp"), strings.HasPrefix(low, "data:image/bmp"):
			return tag
		default:
			return "<span class=\"blocked-image\">[图片已屏蔽]</span>"
		}
	})
}

// htmlToText 粗略地把 HTML 转成纯文本，仅用于验证码提取等辅助逻辑。
func htmlToText(raw string) string {
	if raw == "" {
		return ""
	}
	s := scriptRe.ReplaceAllString(raw, " ")
	s = anyTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := []byte(s[:n])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// handleProxyImage 把远程图片代理委托给 internal/imgproxy（SSRF 安全、5MB 上限）。
func (s *Server) handleProxyImage(w http.ResponseWriter, r *http.Request) {
	if s.img == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "图片代理未就绪")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "缺少图片地址参数 u")
		return
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "图片地址必须是 http(s) URL")
		return
	}
	s.img.ServeImage(w, r, raw)
}
