package httpapi

import (
	"strings"
	"testing"
)

func TestSanitizeHTMLRemovesActiveContentAndProxiesImages(t *testing.T) {
	got := sanitizeHTML("<div onclick=\"alert(1)\"><script>alert(1)</script><a href=\"javascript:alert(1)\">x</a><img src=\"https://tracker.example/a.png?x=1&amp;y=2\"></div>")
	for _, bad := range []string{"<script", "onclick=", "javascript:", "https://tracker.example"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("unsafe content %q in %s", bad, got)
		}
	}
	for _, want := range []string{"/api/proxy/image?u=", "loading=\"lazy\"", "referrerpolicy=\"no-referrer\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

func TestRewriteImageSourcesBlocksCID(t *testing.T) {
	got := rewriteImageSources(`<img src="cid:photo"><img src="data:image/png;base64,AAAA">`)
	if !strings.Contains(got, "[图片已屏蔽]") || !strings.Contains(got, "data:image/png") {
		t.Fatalf("unexpected rewrite: %s", got)
	}
}
