// Package packs 随发行嵌入内置能力包（附录 C.1）。每个子目录是一个包：pack.yaml、prompts/、views/、templates/。
package packs

import "embed"

// FS 是只读的内置包目录；包目录之外的文件（本文件）由加载器忽略。
//
//go:embed all:*
var FS embed.FS
