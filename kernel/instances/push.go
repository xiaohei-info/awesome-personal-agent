package instances

import (
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// TaskOf 返回事项出自哪个定时任务实例与它的类型（包名）；不是实例建的事项、或实例已不在时 ok 为假。只读。
func TaskOf(tx *store.Tx, it domain.Item) (instance, pack string, ok bool, err error) {
	if it.Origin == nil {
		return "", "", false, nil
	}
	id, found := strings.CutPrefix(it.Origin.Source, domain.OriginInstancePrefix)
	if !found || id == "" {
		return "", "", false, nil
	}
	in, err := Get(tx, id)
	if isCode(err, domain.CodeNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return in.ID, in.Pack, true, nil
}

// PushToMain 报告这件事项（定时任务的产出）写回 APA 之后要不要再注入主会话（§7.4 主动提醒）：按实例 ?? 按类型 ?? 默认。
func PushToMain(tx *store.Tx, pol policy.Policy, it domain.Item) (pack string, on bool, err error) {
	id, pack, ok, err := TaskOf(tx, it)
	if err != nil || !ok {
		return pack, false, err
	}
	return pack, pol.Disturb.ProactiveSettings().PushTask(pack, id), nil
}
