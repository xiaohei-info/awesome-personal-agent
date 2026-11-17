//go:build apa_testclock

package serve

import srcdecl "github.com/xiaohei-info/awesome-personal-agent/sources"

// 只编进 apa_testclock 测试构建：给桩来源脚本 SRC_STUB（tests/doubles/srcstub）补一份写了
// content.attachments: fetch 的接入声明，e2e 才能走通"来源程序声明支持取回"的路径；发布构建不含它。
// 对应 tests/doubles/srcstub/source.yaml；"不支持取回"的对照来源用另一个程序名（src-stub-nofetch），没有声明。
func init() {
	sourceDecls = append(sourceDecls, srcdecl.Declaration{Program: "src-stub", Default: srcdecl.Text{Zh: "桩来源", En: "Stub source"},
		Sender: srcdecl.Sender{Trust: srcdecl.TrustUnverified}, Content: srcdecl.Content{Attachments: srcdecl.AttachmentsFetch}})
}
