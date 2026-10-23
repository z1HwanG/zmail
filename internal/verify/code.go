// Package verify 从邮件主题与正文中提取验证码。
package verify

import (
	"regexp"
	"strings"
)

var codePatterns = []*regexp.Regexp{
	// 关键词在前：中间不允许出现数字，数字后必须是边界，避免把长数字前缀当成验证码。
	regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|安全码|确认码|verification code|verify code|verification|otp|passcode|code)[^\d]{0,20}(\d{4,8})(?:[^\d]|$)`),
	// 数字在前：数字前必须是边界，后面紧跟关键词。
	regexp.MustCompile(`(?i)(?:^|[^\d])(\d{4,8})[^\d]{0,10}(?:验证码|校验码|verification code|verify code|otp|passcode)`),
}

var negativePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(订单号|订单编号|order\s*(?:no|number|id)|invoice|金额|price|total|\$|￥|¥)`),
}

var subjectKeywords = []string{
	"验证码", "校验码", "动态码", "安全码", "确认码",
	"verification code", "verify code", "verification", "otp", "passcode", "sign-in code", "login code",
}

var (
	reSep  = regexp.MustCompile(`[\s\-–—_]+`)
	reYear = regexp.MustCompile(`^(19|20)\d{2}$`)
)

// Extract 尝试从主题与正文中提取验证码。未找到返回 ""。
func Extract(subject, body string) string {
	s := strings.TrimSpace(subject)
	b := normalize(body)
	combined := s + "\n" + b

	if looksNegative(s) || (looksNegative(firstLines(b, 3)) && !containsSubjectKeyword(s)) {
		// 订单/金额类邮件不提取，除非主题明确是验证码邮件。
		if !containsSubjectKeyword(s) {
			return ""
		}
	}

	for _, re := range codePatterns {
		for _, m := range re.FindAllStringSubmatch(combined, 5) {
			if len(m) < 2 {
				continue
			}
			if cand := sanitizeCode(m[1], b); cand != "" {
				return cand
			}
		}
	}
	if containsSubjectKeyword(s) {
		for _, cand := range allDigitRuns(b, 8) {
			if c := sanitizeCode(cand, b); c != "" {
				return c
			}
		}
	}
	return ""
}

// IsVerificationMail 判断邮件是否像验证码邮件。
func IsVerificationMail(subject, body string) bool {
	if containsSubjectKeyword(subject) {
		return true
	}
	return Extract(subject, body) != ""
}

func sanitizeCode(cand, body string) string {
	cand = strings.TrimSpace(cand)
	if len(cand) < 4 || len(cand) > 8 {
		return ""
	}
	if reYear.MatchString(cand) {
		return ""
	}
	if strings.HasPrefix(cand, "0000") && len(cand) > 4 {
		return ""
	}
	// 排除金额：数字紧邻货币符号。
	idx := strings.Index(body, cand)
	if idx > 0 {
		prefix := body[:idx]
		if strings.HasSuffix(prefix, "$") || strings.HasSuffix(prefix, "¥") || strings.HasSuffix(prefix, "￥") {
			return ""
		}
	}
	return cand
}

func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return s
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func looksNegative(s string) bool {
	for _, re := range negativePatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func containsSubjectKeyword(s string) bool {
	low := strings.ToLower(s)
	for _, k := range subjectKeywords {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

func allDigitRuns(s string, maxLen int) []string {
	var out []string
	run := strings.Builder{}
	flush := func() {
		if run.Len() >= 4 && run.Len() <= maxLen {
			out = append(out, run.String())
		}
		run.Reset()
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			run.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}
