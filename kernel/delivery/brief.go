package delivery

import (
	"encoding/json"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// BriefID 是简报事项的投递 id：一份简报只入队一次（设计 §3.6）。
func BriefID(item string) string { return "brief:" + item }

// BriefContent 是简报投递的内容：标题取事项标题，摘要取事项一句话摘要，没有则列出分节的项目名。
// 分节正文不进投递（只在 Web 简报页读），spec 不合预期时摘要留空，不挡投递。
func BriefContent(it domain.Item, webURL string) domain.DeliveryContent {
	c := domain.DeliveryContent{Title: it.Title, Summary: it.Summary, WebURL: webURL}
	if c.Summary != "" {
		return c
	}
	var spec struct {
		Sections []struct {
			Project string `json:"project"`
		} `json:"sections"`
	}
	if json.Unmarshal(it.Spec, &spec) != nil {
		return c
	}
	var names []string
	for _, s := range spec.Sections {
		if s.Project != "" {
			names = append(names, s.Project)
		}
	}
	if len(names) > 0 {
		c.Summary = "按项目分节：" + strings.Join(names, "、")
	}
	return c
}
