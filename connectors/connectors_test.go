package connectors

import (
	"strings"
	"testing"
)

const sample = `type: sample
direction: channel
title: {zh: 示例, en: Sample}
tagline: {zh: 只能看, en: View only}
default: {zh: 示例, en: Sample}
via: {zh: 示例服务器, en: the sample server}
capabilities: {buttons: false, reply: false, identify: false}
fields:
  - {key: server, kind: url, label: {zh: 服务器地址, en: Server}, default: "https://example.invalid", required: true}
  - {key: key, kind: secret, label: {zh: 密钥, en: Key}, required: true}
`

func TestParseAcceptsValidAndRejectsIncomplete(t *testing.T) {
	d, err := Parse([]byte(sample))
	if err != nil || d.Type != "sample" || len(d.SecretKeys()) != 1 || !d.Capabilities.ViewOnly() {
		t.Fatalf("parse: %+v %v", d, err)
	}
	for _, bad := range []string{
		strings.Replace(sample, ", en: Sample}\ntagline", "}\ntagline", 1), // 缺英文
		sample + "extra: 1\n", // 未知字段
		strings.Replace(sample, "kind: secret", "kind: password", 1), // 未知字段种类
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
