package doctor

import (
	"os"
	"regexp"
	"testing"
)

// requireCodes 断言报告里每项检查和每个对象都带 codes.go 里声明的机器码，problems 里的也是（Web 按它显示中文，§15.5）。
// codesFile 是 codes.go 的路径（各包测试从自己的目录给出）。
func requireCodes(t *testing.T, codesFile string, checks ...Check) {
	t.Helper()
	src, err := os.ReadFile(codesFile)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*Code\w+\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		known[m[1]] = true
	}
	ok := func(where, code string) {
		if !known[code] {
			t.Errorf("%s: code %q is not declared in codes.go", where, code)
		}
	}
	for _, c := range checks {
		ok(c.Name, c.Code)
		for _, d := range c.Details {
			ok(c.Name+"/"+d.Subject, d.Code)
			ps, _ := d.Data["problems"].([]string)
			for _, p := range ps {
				ok(c.Name+"/"+d.Subject+" problems", p)
			}
			if d.Status != OK && d.Code == CodeOK {
				t.Errorf("%s/%s: status %s with code ok", c.Name, d.Subject, d.Status)
			}
		}
	}
}
