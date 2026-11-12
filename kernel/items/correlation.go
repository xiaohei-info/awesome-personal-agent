package items

import (
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 事项登记的关联键（§9.4）：本人或主会话为事项登记"去重域加键"，之后该域里带这个键的来源事件归属到这件事项。
// Domain 为空表示不限去重域。登记与移除经 item.relate（带 key），记 item.updated，payload 的 Key 供折叠重建。

// Correlation 是事项登记的一个关联键。
type Correlation struct {
	Domain string `json:"domain,omitempty"`
	Key    string `json:"key"`
}

func setCorrelation(tx *store.Tx, item string, x Correlation, remove bool) error {
	var err error
	if remove {
		_, err = tx.Exec(`DELETE FROM item_correlation WHERE item = ? AND domain = ? AND key = ?`, item, x.Domain, x.Key)
	} else {
		_, err = tx.Exec(`INSERT OR IGNORE INTO item_correlation (item, domain, key) VALUES (?, ?, ?)`, item, x.Domain, x.Key)
	}
	return err
}

func hasCorrelation(tx *store.Tx, item string, x Correlation) (bool, error) {
	r, err := tx.Query(`SELECT 1 FROM item_correlation WHERE item = ? AND domain = ? AND key = ?`, item, x.Domain, x.Key)
	if err != nil {
		return false, err
	}
	defer r.Close()
	return r.Next(), r.Err()
}

// Correlations 返回事项登记的关联键，按域、键排序；没有时返回 nil。
func Correlations(tx *store.Tx, item string) ([]Correlation, error) {
	r, err := tx.Query(`SELECT domain, key FROM item_correlation WHERE item = ? ORDER BY domain, key`, item)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Correlation
	for r.Next() {
		var c Correlation
		if err := r.Scan(&c.Domain, &c.Key); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, r.Err()
}

// Correlated 返回在去重域 dedupeDomain 里登记了 key 的事项（登记时没限域的也算），按 id 排序、不重复。
// 不同去重域登记的同名键不互相命中。
func Correlated(tx *store.Tx, dedupeDomain, key string) ([]string, error) {
	r, err := tx.Query(`SELECT DISTINCT item FROM item_correlation WHERE key = ? AND (domain = ? OR domain = '') ORDER BY item`, key, dedupeDomain)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var id string
		if err := r.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, r.Err()
}

// checkCorrelation 校验要登记的键：非空，不含空白与控制字符（键要能在命令行里原样输入，也进事件日志）。
func checkCorrelation(x Correlation) error {
	bad := func(s string) bool {
		return strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0
	}
	switch {
	case x.Key == "" || bad(x.Key):
		return invalid("correlation key is required and must not contain whitespace or control characters")
	case bad(x.Domain):
		return invalid("correlation domain must not contain whitespace or control characters")
	}
	return nil
}
