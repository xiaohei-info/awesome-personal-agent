package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
)

// 主机本地入口（§14.12）：apa serve 在数据目录的 host/（0700）下开一个 Unix 套接字，主机终端上的 `apa merge … --apply`
// 经它在服务进程里执行，与维护循环共用同一个内核（同一把归类锁、同一条命令管线与提交后订阅），不用停服务。
// 能连上它的就是能写数据目录里主机配置文件的人，按主机上的本人（hostCLI）执行，同 §14.12；它不在网络监听上。

func hostSocket(l config.Layout) string { return filepath.Join(l.Home, "host", "serve.sock") }

// hostReq 是一次经主机本地入口的 apa merge --apply。
type hostReq struct {
	Sub    string               `json:"sub"` // run 或 mark-pending
	By     string               `json:"by,omitempty"`
	Filter inbound.ManualFilter `json:"filter"`
}

type hostResp struct {
	Report any           `json:"report,omitempty"`
	Error  *domain.Error `json:"error,omitempty"`
}

// listenHost 开主机本地入口。调用前 serve 已占住监听端口，不会有第二个 serve，上次没正常退出留下的套接字文件可以删。
func listenHost(l config.Layout) (net.Listener, error) {
	p := hostSocket(l)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", p)
}

// serveHost 在 ln 上应答 POST /merge，直到返回的 stop 被调用。
func serveHost(ln net.Listener, in *inbound.Service) (stop func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /merge", func(w http.ResponseWriter, r *http.Request) {
		var q hostReq
		var res hostResp
		var err error
		if json.NewDecoder(r.Body).Decode(&q) != nil {
			err = usage("bad host request", mergeUsage)
		} else {
			switch q.Sub {
			case "run":
				res.Report, err = in.RunManual(r.Context(), hostCLI, q.Filter)
			case "mark-pending":
				res.Report, err = in.MarkPending(r.Context(), hostCLI, q.By, q.Filter, true)
			default:
				err = usage("unknown host request "+q.Sub, mergeUsage)
			}
		}
		if err != nil {
			if !errors.As(err, &res.Error) {
				res.Error = domain.NewError(domain.CodeInternal, err.Error(), "see the apa serve log, then run again; settled proposals are not touched twice")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	return func() { srv.Close() }
}

// viaServe 把一次 --apply 交给正在跑的 apa serve。served 为假表示没有服务在应答（套接字不在或连不上），调用方离线跑。
func viaServe(ctx context.Context, l config.Layout, q hostReq) (report json.RawMessage, served bool, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", hostSocket(l))
	if err != nil {
		return nil, false, nil
	}
	defer conn.Close()
	body, _ := json.Marshal(q)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://apa-host/merge", bytes.NewReader(body))
	if err := req.Write(conn); err != nil {
		return nil, true, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, true, domain.NewError(domain.CodeInternal, "apa serve closed the host entry before answering: "+err.Error(), "check the apa serve log; run `apa merge run --dry-run` to see what is still pending")
	}
	defer resp.Body.Close()
	var out struct {
		Report json.RawMessage `json:"report"`
		Error  *domain.Error   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, true, err
	}
	if out.Error != nil {
		return out.Report, true, out.Error
	}
	return out.Report, true, nil
}
