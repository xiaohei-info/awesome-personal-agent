package doctor

import (
	"context"
	"fmt"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/connections"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// KeyFact 是主密钥文件的事实：不读内容之外的任何东西，不输出密钥。
type KeyFact struct {
	Present bool
	Mode    os.FileMode
}

// KeyFactOf 看主密钥文件在不在、权限是多少。
func KeyFactOf(path string) KeyFact {
	fi, err := os.Stat(path)
	if err != nil {
		return KeyFact{}
	}
	return KeyFact{Present: true, Mode: fi.Mode().Perm()}
}

// ReadConnections 在一个只读事务里读连接与密文概况，填进 f（§5.6）。cs 的保管库是当前主密钥；主密钥缺失时调用方给一个
// 临时密钥，这时全部密文都算解不开、必填凭据的连接显示 credentials_missing，与 apa serve 启动后看到的一致。
func (f *Facts) ReadConnections(ctx context.Context, st *store.Store, cs *connections.Service, key KeyFact) error {
	err := st.View(ctx, func(tx *store.Tx) (err error) {
		if f.Connections, err = cs.List(tx, ""); err != nil {
			return err
		}
		f.Secrets, err = cs.Vault().Inspect(tx)
		return err
	})
	f.Key, f.ConnectionsRead = key, err == nil
	return err
}

// secretsCheck 只报数量与结论，不输出任何凭据内容。
func secretsCheck(f Facts) Check {
	s := f.Secrets
	c := Check{Name: Secrets, Status: OK, Data: map[string]any{"rows": s.Rows, "unreadable": s.Unreadable, "other_key": s.OtherKey, "key_file": f.Key.Present}}
	switch {
	case !f.Key.Present && s.Rows == 0:
		c.Code, c.Why, c.Next = CodeSecretsNone, "no connection credential is stored; apa serve generates the master key file on its next start", "nothing to do"
	case !f.Key.Present:
		c.Status, c.Code = Warning, CodeSecretsKeyMissing
		c.Why = "the master key file is missing; apa serve generates a new one, and credentials saved under the old key must be filled in again"
		c.Next = "restore master.key from a backup if you have one, otherwise re-enter the credentials on the connection cards"
	case f.Key.Mode&0o077 != 0:
		c.Status, c.Code = Error, CodeSecretsKeyMode
		c.Data["mode"] = fmt.Sprintf("%o", f.Key.Mode)
		c.Why = "the master key file is readable by other accounts"
		c.Next = "chmod 600 <APA_HOME>/master.key"
	case s.Unreadable > 0:
		c.Status, c.Code = Warning, CodeSecretsUnreadable
		c.Why = fmt.Sprintf("%d of %d stored credentials cannot be decrypted with the current master key (%d were saved under another key)", s.Unreadable, s.Rows, s.OtherKey)
		c.Next = "re-enter the credentials on the connection cards that ask for it, or restore the master key that saved them"
	case s.Rows == 0:
		c.Code, c.Why, c.Next = CodeSecretsNone, "no connection credential is stored (optional)", "nothing to do"
	default:
		c.Code, c.Why, c.Next = CodeSecretsOK, fmt.Sprintf("%d stored credentials decrypt with the current master key", s.Rows), "nothing to do"
	}
	return c
}

// connectionsCheck 逐个连接报现状、问题代码与最近成功时间（§1.6）。停用的连接只报状态，不算问题。
func connectionsCheck(f Facts) Check {
	c := Check{Name: Connections, Status: OK, Data: map[string]any{"count": len(f.Connections)}}
	for _, x := range f.Connections {
		d := Detail{Subject: x.ID, Status: OK, Code: CodeConnectionOK, Data: map[string]any{"direction": x.Direction, "type": x.Type, "name": x.Name, "enabled": x.Enabled}}
		if x.LastOKAt != nil {
			d.Data["last_ok_at"] = ts(*x.LastOKAt)
		}
		var missing []string
		for k, ok := range x.Secrets {
			if !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			d.Data["missing_credentials"] = missing
		}
		switch x.State {
		case connections.StateDisabled:
			d.Code, d.Why = CodeConnectionDisabled, "disabled; deliveries routed to it go to Web unread"
		case connections.StateProblem:
			d.Status, d.Code = Warning, CodeConnectionProblem
			d.Data["problem"] = x.Problem
			d.Why = "the last attempt reported " + x.Problem + "; deliveries keep retrying and then fall back to Web unread"
			d.Next = "open the connection card in Web settings and follow its prompt"
		default:
			d.Why = "the last attempt succeeded"
		}
		c.Add(d)
	}
	switch {
	case len(f.Connections) == 0:
		c.Code, c.Why, c.Next = CodeConnectionsNone, "no channel or output is connected (optional)", "add one in Web settings when you want reminders outside the Web"
	case c.Status == OK:
		c.Code, c.Why, c.Next = CodeConnectionsOK, "every enabled connection worked on its last attempt", "nothing to do"
	default:
		c.Code, c.Why, c.Next = CodeConnectionsProblem, "some connections reported a problem; see details", "fix them on the connection cards in Web settings"
	}
	return c
}
