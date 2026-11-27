package domain

import (
	"regexp"
	"strings"
)

// 投递正文里的链接用 Markdown 写法（事项 id 藏在链接里，§7.4）；只能看文字的出口用 StripLinks 去掉链接只留文字。

var mdLink = regexp.MustCompile(`\[([^\[\]]*)\]\((?:https?://|/)[^()\s]*\)`)

// MDLink 是一个 Markdown 链接；文字里的方括号与换行换掉，免得截断链接。
func MDLink(text, url string) string {
	text = strings.NewReplacer("[", "【", "]", "】", "\n", " ", "\r", " ").Replace(text)
	return "[" + text + "](" + url + ")"
}

// StripLinks 把 Markdown 链接换成它的文字。
func StripLinks(s string) string { return mdLink.ReplaceAllString(s, "$1") }
