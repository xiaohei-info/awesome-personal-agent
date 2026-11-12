package items

import (
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

func init() {
	client.Register("attach", attach)
	client.Register("proposals", proposals)
}

const attachUsage = "usage: apa attach <id> --to <item id> [--reason <t>] [--evidence <owner's words> | --grant <id>]"

// attach：建议的第三种处置，补到已有事项。POST /items/{id}/attach；目标是否合法由内核判定（404/409）。
func attach(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("a proposal id is required first", attachUsage)
	}
	var to, reason string
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&to, "to", "", "")
	fs.StringVar(&reason, "reason", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || to == "" {
		return usage("--to <item id> is required", attachUsage)
	}
	body := map[string]any{"to": to}
	if reason != "" {
		body["reason"] = reason
	}
	var out map[string]any
	if err := c.Do(bg, "POST", "/items/"+url.PathEscape(args[0])+"/attach", body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

const proposalsUsage = "usage: apa proposals apply (--recommended | --file <plan.json>) [--source <id>] [--project <id>] [--evidence <owner's words> | --grant <id>]"

// proposals apply：批量处理建议，POST /proposals/batch。每条各自成败，输出内核返回的逐条结果。
func proposals(c *client.Ctx, args []string) error {
	if len(args) == 0 || args[0] != "apply" {
		return usage("proposals takes the subcommand apply", proposalsUsage)
	}
	var (
		rec                   bool
		file, source, project string
	)
	fs := flag.NewFlagSet("proposals apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&rec, "recommended", false, "")
	fs.StringVar(&file, "file", "", "")
	fs.StringVar(&source, "source", "", "")
	fs.StringVar(&project, "project", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || rec == (file != "") {
		return usage("give exactly one of --recommended and --file", proposalsUsage)
	}
	var body map[string]any
	if rec {
		filter := map[string]any{}
		if source != "" {
			filter["source"] = source
		}
		if project != "" {
			filter["project"] = project
		}
		body = map[string]any{"mode": "recommended", "filter": filter}
	} else {
		if source != "" || project != "" {
			return usage("--source and --project only apply to --recommended", proposalsUsage)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return usage("cannot read the plan file: "+err.Error(), proposalsUsage)
		}
		var plan struct {
			Actions []json.RawMessage `json:"actions"`
		}
		if err := json.Unmarshal(raw, &plan); err != nil || plan.Actions == nil {
			return usage("the plan file must be a JSON object with an actions list", proposalsUsage)
		}
		body = map[string]any{"mode": "explicit", "actions": plan.Actions}
	}
	var out map[string]any
	if err := c.Do(bg, "POST", "/proposals/batch", body, &out); err != nil {
		return err
	}
	return c.Print(out)
}
