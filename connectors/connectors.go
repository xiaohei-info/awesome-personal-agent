// Package connectors 是随主程序编译的连接插件（设计 §1.3、§3）：渠道。每种类型一个子目录
// connectors/<类型>/，带一份接入声明 connector.yaml（嵌入主程序）和实现 kernel/outcall/conn 接口的代码。
// 厂商协议、SDK 与消息渲染只在这里；内核不 import 本目录，由装配方（cli/serve）把插件交给外呼调用器。
//
// 本包只做声明的解析与校验，各类型的子包用它读自己的 connector.yaml。声明不合法是构建缺陷，由各子包的测试守住。
package connectors

import (
	"bytes"
	"fmt"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"gopkg.in/yaml.v3"
)

// Parse 严格解析一份 connector.yaml（未知字段拒绝）并校验。
func Parse(b []byte) (conn.Decl, error) {
	var d conn.Decl
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("connector.yaml: %w", err)
	}
	return d, d.Check()
}

// MustParse 是 Parse，失败即 panic：嵌入的声明不合法只可能是构建缺陷。
func MustParse(b []byte) conn.Decl {
	d, err := Parse(b)
	if err != nil {
		panic(err)
	}
	return d
}
