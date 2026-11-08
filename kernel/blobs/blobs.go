// Package blobs 是内容寻址附件（设计 §6.6、§16.2）：blob.put / blob.get 按内容 sha256 存取，
// 相同内容只存一份，材料正文用 apa://blob/<hash> 引用。blob 不可变，换附件即新哈希（新修订由材料模块写）。
// 物理删除不在本包（purge 负责）；本包不写账本表，只经命令管线登记两条命令。
package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// RefPrefix 是正文里引用 blob 的 URI 前缀。
const RefPrefix = "apa://blob/"

// PutResult 是 blob.put 的结果；Created 为假表示相同内容已存在，未重复写入。
type PutResult struct {
	Hash    string `json:"hash"`
	Ref     string `json:"ref"`
	Size    int64  `json:"size"`
	Created bool   `json:"created"`
}

// GetResult 是 blob.get 的结果。
type GetResult struct {
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Content []byte `json:"content"`
}

// Ref 返回 hash 对应的引用。
func Ref(hash string) string { return RefPrefix + hash }

// ParseRef 从 apa://blob/<hash> 取出 hash；格式不合法返回错误。
func ParseRef(s string) (string, error) {
	h, ok := strings.CutPrefix(s, RefPrefix)
	if !ok || !validHash(h) {
		return "", fmt.Errorf("blobs: %q is not an %s<sha256-hex> reference", s, RefPrefix)
	}
	return h, nil
}

// validHash 只接受 64 位小写十六进制，同时挡住路径穿越。
func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

// internal 把 I/O 与完整性错误归为 internal（退出码 1），响应里不带路径；domain 错误原样通过。
func internal[T any](v T, err error) (any, error) {
	var de *domain.Error
	switch {
	case err == nil:
		return v, nil
	case errors.As(err, &de):
		return nil, err
	}
	return nil, domain.NewError(domain.CodeInternal, "blob storage failed", "check the data directory and retry")
}

type hashIn struct {
	Hash string `json:"hash"`
}

type putIn struct {
	Content []byte `json:"content"`
}

// Register 在 x 上登记 blob.put、blob.get；dir 是 <APA_HOME>/blobs，须已存在。
// shortcut: 不限单个 blob 大小（整块读入内存），上传入口（Web/API）接入时在传输层加上限。
func Register(x *command.Executor, dir string) error {
	noObject := func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil }
	if err := x.Register(command.Spec{
		Name: "blob.put", Action: authz.ActionReviseMaterial, Tier: authz.TierWork, Object: noObject,
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			var p putIn
			if err := json.Unmarshal(in, &p); err != nil {
				return invalid("input must be {\"content\": <base64>}")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var p putIn
			if err := json.Unmarshal(c.Input, &p); err != nil {
				return nil, invalid("input must be {\"content\": <base64>}")
			}
			return internal(put(dir, p.Content))
		},
	}); err != nil {
		return err
	}
	return x.Register(command.Spec{
		Name: "blob.get", Action: authz.ActionRead, Object: noObject,
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			var h hashIn
			if err := json.Unmarshal(in, &h); err != nil || !validHash(h.Hash) {
				return invalid("hash must be 64 lowercase hex characters")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var h hashIn
			if err := json.Unmarshal(c.Input, &h); err != nil || !validHash(h.Hash) {
				return nil, invalid("hash must be 64 lowercase hex characters")
			}
			return internal(get(dir, h.Hash))
		},
	})
}

func put(dir string, content []byte) (PutResult, error) {
	sum := sha256.Sum256(content)
	h := hex.EncodeToString(sum[:])
	res := PutResult{Hash: h, Ref: Ref(h), Size: int64(len(content))}
	final := filepath.Join(dir, h)
	if _, err := os.Stat(final); err == nil {
		return res, nil // 已存在：内容寻址，同哈希即同内容
	} else if !errors.Is(err, os.ErrNotExist) {
		return PutResult{}, err
	}
	// 先写临时文件并落盘，再原子改名：崩溃只会留下 .tmp，不会留下半截 blob。
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return PutResult{}, err
	}
	defer os.Remove(tmp.Name()) // 改名成功后为空操作
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return PutResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return PutResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return PutResult{}, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return PutResult{}, err
	}
	if d, err := os.Open(dir); err == nil { // 目录项落盘
		err = d.Sync()
		d.Close()
		if err != nil {
			return PutResult{}, err
		}
	}
	res.Created = true
	return res, nil
}

func get(dir, h string) (GetResult, error) {
	data, err := os.ReadFile(filepath.Join(dir, h))
	if errors.Is(err, os.ErrNotExist) {
		return GetResult{}, domain.NewError(domain.CodeNotFound, "blob does not exist", "check the reference")
	}
	if err != nil {
		return GetResult{}, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != h {
		return GetResult{}, fmt.Errorf("blobs: stored content of %s does not match its hash", h)
	}
	return GetResult{Hash: h, Size: int64(len(data)), Content: data}, nil
}

// Remove 删除 dir 下哈希为 h 的 blob 文件（物理删除用）；不存在视为已删除。
// 调用方负责先确认没有其他引用。
func Remove(dir, h string) error {
	if !validHash(h) {
		return invalid("hash must be 64 lowercase hex characters")
	}
	if err := os.Remove(filepath.Join(dir, h)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
