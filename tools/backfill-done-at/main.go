// backfill-done-at 给工作台迁移来、没记完成日期（ext.workbench.completed 为空）的已完成事项补日期。一次性、幂等，只走 HTTP API。
//
// 决定文件是 JSON：事项 id → {"date":"YYYY-MM-DD","progress":"<进展 id>"} 或 {"unknown":true}。
// 写日期要有出处：所引进展的 on 必须等于该日期（迁移原文里带日期的完成表述）；查不到出处的写 unknown，
// 不写任何东西，内核把它们列为"完成日期不详"（附录 B.2 done_unknown），不猜日期。
// 默认只演练，不写；加 -apply 才写。已是该日期的跳过；已有别的日期的拒绝覆盖。
//
//	APA_TOKEN=... backfill-done-at -url http://127.0.0.1:7777 -decisions d.json [-apply]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
)

type decision struct {
	Date     string `json:"date"`
	Progress string `json:"progress"`
	Unknown  bool   `json:"unknown"`
}

type detail struct {
	Item struct {
		Version int64                      `json:"version"`
		Phase   string                     `json:"phase"`
		Ext     map[string]json.RawMessage `json:"ext"`
	} `json:"item"`
	Progress []struct{ ID, On string } `json:"progress"`
}

var day = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func call(c *http.Client, method, url, token, ifMatch string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d %s", method, url, resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

func run(c *http.Client, base, token string, dec map[string]decision, apply bool, w io.Writer) error {
	var list struct {
		DoneUnknown []string `json:"done_unknown"`
	}
	if err := call(c, "GET", base+"/v1/items?phase=done", token, "", nil, &list); err != nil {
		return err
	}
	unknown := map[string]bool{}
	for _, id := range list.DoneUnknown {
		unknown[id] = true
	}
	ids := make([]string, 0, len(dec))
	for id := range dec {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := dec[id]
		var it detail
		if err := call(c, "GET", base+"/v1/items/"+id, token, "", nil, &it); err != nil {
			return err
		}
		var wb map[string]any
		if raw, ok := it.Item.Ext["workbench"]; !ok || json.Unmarshal(raw, &wb) != nil || it.Item.Phase != "done" {
			return fmt.Errorf("%s: 不是工作台迁移来的已完成事项", id)
		}
		cur, _ := wb["completed"].(string)
		switch {
		case d.Unknown:
			if cur != "" {
				return fmt.Errorf("%s: 决定写不详，但事项已有完成日期 %s", id, cur)
			}
			fmt.Fprintf(w, "%s\t不详（不写，列入 done_unknown=%v）\n", id, unknown[id])
		case !day.MatchString(d.Date):
			return fmt.Errorf("%s: date 须为 YYYY-MM-DD", id)
		case cur == d.Date:
			fmt.Fprintf(w, "%s\t已是 %s，跳过\n", id, d.Date)
		case cur != "":
			return fmt.Errorf("%s: 已有完成日期 %s，不覆盖", id, cur)
		default:
			ok := false
			for _, p := range it.Progress {
				ok = ok || p.ID == d.Progress && p.On == d.Date
			}
			if !ok {
				return fmt.Errorf("%s: 进展 %q 不存在或所指日期不是 %s，没有出处不写", id, d.Progress, d.Date)
			}
			wb["completed"] = d.Date
			it.Item.Ext["workbench"], _ = json.Marshal(wb)
			if !apply {
				fmt.Fprintf(w, "%s\t将写 %s（演练）\n", id, d.Date)
				continue
			}
			body := map[string]any{"ext": it.Item.Ext, "reason": "补工作台迁移事项的完成日期，出处：进展 " + d.Progress}
			var res map[string]any
			if err := call(c, "PATCH", base+"/v1/items/"+id, token, strconv.Quote(strconv.FormatInt(it.Item.Version, 10)), body, &res); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\t已写 %s\n", id, d.Date)
		}
	}
	return nil
}

func main() {
	url := flag.String("url", os.Getenv("APA_URL"), "内核地址")
	file := flag.String("decisions", "", "决定文件（JSON）")
	apply := flag.Bool("apply", false, "真写；默认只演练")
	flag.Parse()
	var dec map[string]decision
	b, err := os.ReadFile(*file)
	if err == nil {
		err = json.Unmarshal(b, &dec)
	}
	if err == nil && (*url == "" || os.Getenv("APA_TOKEN") == "") {
		err = fmt.Errorf("需要 -url（或 APA_URL）与环境变量 APA_TOKEN")
	}
	if err == nil {
		err = run(http.DefaultClient, *url, os.Getenv("APA_TOKEN"), dec, *apply, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
