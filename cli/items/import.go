package items

import (
	"flag"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

func init() { client.Register("import", importCmd) }

// 单次请求的上限，同 HTTP 的 POST /import（§7.1）。
const (
	importMaxLines = 500
	importMaxBytes = 8 << 20
)

const importUsage = "usage: apa import --file <JSONL plan file, - for stdin> --source <source name> [--dry-run] --grant <import grant id>"

// importCmd 把计划文件按 500 条（且不超过 8 MiB）一批提交到 POST /import，第一批返回的批次号沿用到后面各批，
// 逐行输出 created、existing 或 invalid。校验、授权与幂等都在内核；--dry-run 只预检，不建批次、不占授权单的条数。
func importCmd(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file, source := fs.String("file", "", ""), fs.String("source", "", "")
	dry := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *file == "" || *source == "" {
		return usage("--file and --source are required", importUsage)
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(c.Env.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return usage("the plan file cannot be read", importUsage)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	q := url.Values{"source": {*source}}
	if *dry {
		q.Set("dry_run", "1")
	}
	batch := ""
	for start := 0; start < len(lines); {
		end, n, size := start, 0, 0
		for end < len(lines) {
			l := len(lines[end]) + 1
			if l > importMaxBytes {
				return usage("a line of the plan file is larger than 8 MiB", importUsage)
			}
			if lines[end] != "" && (n == importMaxLines || size+l > importMaxBytes) {
				break
			}
			if strings.TrimSpace(lines[end]) != "" {
				n++
			}
			size += l
			end++
		}
		if batch != "" {
			q.Set("batch", batch)
		}
		var out struct {
			Batch   string           `json:"batch"`
			Results []map[string]any `json:"results"`
		}
		if n == 0 { // 只剩空行：没有可提交的记录
			break
		}
		body := client.Raw(strings.Join(lines[start:end], "\n"))
		if err := c.DoHeaders(bg, map[string]string{"Content-Type": "application/x-ndjson"}, "POST", "/import?"+q.Encode(), body, &out); err != nil {
			return err
		}
		if batch == "" {
			batch = out.Batch
		}
		for _, r := range out.Results { // 行号换成计划文件里的行号
			if ln, ok := r["line"].(float64); ok {
				r["line"] = int(ln) + start
			}
			if err := c.Print(r); err != nil {
				return err
			}
		}
		start = end
	}
	return nil
}
