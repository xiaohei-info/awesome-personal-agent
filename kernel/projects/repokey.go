package projects

import (
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// repoID 是解析后的仓库身份。host 为空是 owner/repo 简写：写法里没有托管站点，内核也不认定默认站点。
type repoID struct{ host, path string }

// parseRepo 解析 URL（https、ssh 等）、scp 写法（git@host:o/r）、host/o/r 与 o/r。
// ok 为假的是本地路径、file:// 这类没有站点的地址：路径区分大小写，不能按仓库身份规范化。
func parseRepo(remote string) (id repoID, ok bool) {
	s := strings.TrimSpace(remote)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return id, false
		}
		id = repoID{u.Hostname(), u.Path}
	} else if host, rest, scp := strings.Cut(s, ":"); scp && !strings.Contains(host, "/") {
		id = repoID{host[strings.LastIndex(host, "@")+1:], rest}
	} else {
		first, tail, _ := strings.Cut(strings.TrimRight(s, "/"), "/")
		switch {
		case first == "" || first[0] == '.' || first[0] == '~': // 绝对或相对的本地路径
			return id, false
		case strings.Contains(tail, "/") && strings.Contains(first, "."):
			id = repoID{first, tail}
		case tail != "" && !strings.Contains(tail, "/"):
			id = repoID{"", first + "/" + tail}
		default:
			return id, false
		}
	}
	id.path = strings.TrimSuffix(strings.Trim(id.path, "/"), ".git")
	for _, seg := range strings.Split(id.path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return id, false
		}
	}
	return repoID{strings.ToLower(id.host), strings.ToLower(id.path)}, true
}

// RepoKey 是仓库身份的规范写法：host/owner/repo，小写、去 .git，ssh 与 https 同形；
// owner/repo 简写没有 host；本地路径原样保留。
func RepoKey(remote string) string {
	id, ok := parseRepo(remote)
	switch {
	case !ok:
		return strings.TrimSpace(remote)
	case id.host == "":
		return id.path
	}
	return id.host + "/" + id.path
}

// SameRepo 判断两种写法是否指向同一仓库。一边是 owner/repo 简写时只比路径，因为简写不带站点；
// 两边都带站点时站点也要相同。
func SameRepo(a, b string) bool {
	x, xok := parseRepo(a)
	y, yok := parseRepo(b)
	if !xok || !yok {
		return !xok && !yok && a != "" && strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return x.path == y.path && (x.host == y.host || x.host == "" || y.host == "")
}

// RepoHasLink 判断链接是否落在仓库之下（按路径段，org/repo 不匹配 org/repo2）。
func RepoHasLink(remote, link string) bool {
	r, rok := parseRepo(remote)
	l, lok := parseRepo(link)
	if !rok || !lok || l.host == "" || r.host != "" && r.host != l.host {
		return false
	}
	return l.path == r.path || strings.HasPrefix(l.path, r.path+"/")
}

// Find 在项目列表里找仓库的登记项：规范写法相同的优先；否则只有简写对上唯一一个登记项才算命中，
// 同名仓库在多个站点登记时不猜。
func Find(ps []domain.Project, remote string) (domain.Project, domain.Repo, bool) {
	key := RepoKey(remote)
	var hitP domain.Project
	var hitR domain.Repo
	n := 0
	for _, p := range ps {
		for _, r := range p.Repos {
			if RepoKey(r.Remote) == key {
				return p, r, true
			}
			if SameRepo(r.Remote, remote) {
				hitP, hitR = p, r
				n++
			}
		}
	}
	if n != 1 {
		return domain.Project{}, domain.Repo{}, false
	}
	return hitP, hitR, true
}
