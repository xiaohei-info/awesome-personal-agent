package auth

import (
	"reflect"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 钥匙给谁用与逐来源档位（§14.2、§14.14）：内部只给三级 token 且不带档位；档位只取 full/excerpt/meta；对话捕获不能给；
// 缺省是外部、空表（不给）；解析时随凭据带出；轮换复制两列；改档位立即生效，已吊销的不能改。
func TestAudienceAndSourceScopes(t *testing.T) {
	e := setup(t)
	bad := []Spec{
		{Kind: Ext, Scope: "export", Audience: AudienceInternal},
		{Kind: Delegate, Audience: AudienceInternal, SourceScopes: map[string]string{"src_a": ScopeFull}},
		{Kind: Delegate, Audience: "local"},
		{Kind: Observer, SourceScopes: map[string]string{"src_a": "none"}},
		{Kind: Observer, SourceScopes: map[string]string{"capture:prj": ScopeFull}},
		{Kind: Webhook, Scope: "src_a", SourceScopes: map[string]string{"src_a": ScopeMeta}},
	}
	for _, sp := range bad {
		err := e.s.Update(ctx, func(tx *store.Tx) error { _, _, err := e.a.Issue(tx, sp); return err })
		if code(err) != domain.CodeInvalidRequest {
			t.Errorf("%+v must be refused, got %v", sp, err)
		}
	}

	def, defSecret := e.issue(t, Spec{Kind: Observer})
	if def.Audience != AudienceExternal || len(def.SourceScopes) != 0 {
		t.Fatalf("a key is external and gives nothing unless chosen: %+v", def)
	}
	in, inSecret := e.issue(t, Spec{Kind: Delegate, Audience: AudienceInternal})
	ext, extSecret := e.issue(t, Spec{Kind: Ext, Scope: "export", SourceScopes: map[string]string{"src_a": ScopeExcerpt, "src_b": ScopeFull}})
	for secret, want := range map[string]Credential{defSecret: def, inSecret: in, extSecret: ext} {
		got, err := e.resolve(secret)
		if err != nil || got.Audience != want.Audience || !reflect.DeepEqual(got.SourceScopes, want.SourceScopes) {
			t.Fatalf("resolve carries audience and scopes: %+v %v, want %+v", got, err, want)
		}
	}

	var rotated Credential
	if err := e.s.Update(ctx, func(tx *store.Tx) (err error) { rotated, _, err = e.a.Rotate(tx, ext.ID, time.Hour); return }); err != nil {
		t.Fatal(err)
	}
	if rotated.Audience != AudienceExternal || !reflect.DeepEqual(rotated.SourceScopes, ext.SourceScopes) {
		t.Fatalf("rotation keeps the scopes: %+v", rotated)
	}

	if err := e.s.Update(ctx, func(tx *store.Tx) error {
		_, err := e.a.SetScope(tx, def.ID, AudienceExternal, map[string]string{"src_a": ScopeMeta})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.resolve(defSecret); !reflect.DeepEqual(got.SourceScopes, map[string]string{"src_a": ScopeMeta}) {
		t.Fatalf("a scope change takes effect on the next request: %+v", got)
	}
	if err := e.s.Update(ctx, func(tx *store.Tx) error {
		_, err := e.a.SetScope(tx, ext.ID, AudienceInternal, nil)
		return err
	}); code(err) != domain.CodeInvalidRequest {
		t.Fatalf("an ext key cannot become internal: %v", err)
	}
	if err := e.s.Update(ctx, func(tx *store.Tx) error { return e.a.Revoke(tx, def.ID) }); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Update(ctx, func(tx *store.Tx) error {
		_, err := e.a.SetScope(tx, def.ID, AudienceInternal, nil)
		return err
	}); code(err) != domain.CodeConflict {
		t.Fatalf("a revoked key cannot be changed: %v", err)
	}
}

// 升级前的行 source_scopes 为空串：按空表读（最严），由一次性迁移写入；MigrateScopes 只动还没迁移的行。
func TestUnmigratedRowsReadAsGivingNothing(t *testing.T) {
	e := setup(t)
	c, secret := e.issue(t, Spec{Kind: Delegate})
	if err := e.s.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE auth_credential SET source_scopes = '' WHERE id = ?`, c.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.resolve(secret); err != nil || len(got.SourceScopes) != 0 || got.Audience != AudienceExternal {
		t.Fatalf("an unmigrated row gives nothing: %+v %v", got, err)
	}
	var todo []Credential
	if err := e.s.Update(ctx, func(tx *store.Tx) (err error) {
		if todo, err = e.a.Unmigrated(tx); err != nil {
			return err
		}
		if err = e.a.MigrateScopes(tx, c.ID, map[string]string{"src_a": ScopeFull}); err != nil {
			return err
		}
		return e.a.MigrateScopes(tx, c.ID, map[string]string{"src_a": ScopeMeta}) // 已迁移：不再改
	}); err != nil || len(todo) != 1 || todo[0].ID != c.ID {
		t.Fatalf("unmigrated: %+v %v", todo, err)
	}
	if got, _ := e.resolve(secret); got.SourceScopes["src_a"] != ScopeFull {
		t.Fatalf("migration writes once: %+v", got)
	}
}
