//go:build e2e

package workbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// ego-browser 是项目规定的 Web 步骤驱动（不引入 Playwright）。本机没有它时，需要真实浏览器的子测试跳过并说明原因，
// 其余 HTTP 层断言照常运行；有它时页面渲染与 360 像素宽度走查必须通过。
func needBrowser(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ego-browser"); err != nil {
		t.Skip("本机没有 ego-browser：页面渲染与手机宽度走查改由装有 ego-browser 的本机验证")
	}
}

// browse 在 ego-browser 的同名任务空间里运行脚本：脚本里有 task、page(p1)、base（内核地址）、out（返回给测试的对象）和 login()。
// 每次调用是独立的 Node 进程，任务空间与页面（含登录态）在调用之间保留。
func (k *kit) browse(space, js string) map[string]any {
	k.t.Helper()
	prelude := fmt.Sprintf(`const task = await taskSpace(%q);
const page = task.page("p1");
const base = %q;
const out = {};
async function login() {
  await page.goto(base + "/");
  const needs = await page.evaluate(() => !!document.querySelector("main.login"));
  if (needs) {
    await page.fill("input", %q);
    await page.click("button[type=submit]");
    await page.waitForSelector("nav", { state: "visible" });
  }
}
`, space, k.URL, "e2e-baseline-password")
	epilogue := "\nconsole.log('RESULT:' + JSON.stringify(out));\n"
	cmd := exec.Command("ego-browser", "nodejs", "-e", prelude+js+epilogue)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		k.t.Fatalf("ego-browser 脚本失败: %v\n%s\n%s", err, so.String(), se.String())
	}
	for _, ln := range strings.Split(so.String()+"\n"+se.String(), "\n") {
		if _, rest, ok := strings.Cut(ln, "RESULT:"); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(rest), &m); err != nil {
				k.t.Fatalf("脚本结果不是 JSON: %v\n%s", err, ln)
			}
			return m
		}
	}
	k.t.Fatalf("ego-browser 脚本没有输出结果\n%s\n%s", so.String(), se.String())
	return nil
}

// closeBrowser 收尾：关掉任务空间里自己开的页面。
func (k *kit) closeBrowser(space string) {
	cmd := exec.Command("ego-browser", "nodejs", "-e", fmt.Sprintf(`const task = await taskSpace(%q); await task.finish({ keep: [] });`, space))
	_ = cmd.Run()
}
