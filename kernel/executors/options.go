package executors

import (
	"maps"
	"regexp"
	"slices"
)

type OptionField struct {
	Label     Label    `json:"label"`
	Type      string   `json:"type"`
	Choices   []string `json:"choices,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	DisplayAs string   `json:"display_as,omitempty"`
}

type InstanceOptions struct {
	Fields map[string]OptionField       `json:"fields,omitempty"`
	Args   map[string][]string          `json:"args_if_set,omitempty"`
	Env    map[string]map[string]string `json:"env_if_set,omitempty"`
	Cwd    []string                     `json:"cwd_choices,omitempty"`
}

var fieldName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
var reservedEnv = regexp.MustCompile(`^(APA_|LD_|DYLD_|LC_|XDG_|PYTHON|NODE_|RUBY|PERL|BASH|GIT_|SSH_|JAVA_|JDK_|GCONV_|GLIBC_|SSL_|GODEBUG$|GOFLAGS$|PATH$|HOME$|USER$|LOGNAME$|LANG$|TZ$|TMPDIR$|TMP$|TEMP$|ENV$|SHELL$|IFS$)|(_PATH|_PROXY|_OPTIONS)$`)
var optionValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\-]{0,199}$`)
var optionFlag = regexp.MustCompile(`^--?[a-zA-Z][a-zA-Z0-9_-]*$`)
var sensitive = regexp.MustCompile(`(?i)pass(?:word|wd)|secret|credential|token|api_?key|private_?key`)

func (o *InstanceOptions) validate() error {
	if o == nil {
		return nil
	}
	models := 0
	for name, field := range o.Fields {
		if !fieldName.MatchString(name) || sensitive.MatchString(name) || name == "cwd" || !field.Label.valid() {
			return invalid("invalid instance field %q", name)
		}
		switch field.Type {
		case "enum":
			if len(field.Choices) == 0 || field.Pattern != "" {
				return invalid("%s needs choices", name)
			}
			for _, value := range field.Choices {
				if !optionValue.MatchString(value) {
					return invalid("invalid choice in %s", name)
				}
			}
		case "string":
			if _, err := regexp.Compile("^(?:" + field.Pattern + ")$"); err != nil || field.Pattern == "" || len(field.Choices) > 0 {
				return invalid("invalid pattern for %s", name)
			}
		default:
			return invalid("unknown field type %s", field.Type)
		}
		if field.DisplayAs != "" {
			models++
			if field.DisplayAs != "model" || models > 1 {
				return invalid("invalid display_as for %s", name)
			}
		}
	}
	for key, args := range o.Args {
		if _, ok := o.Fields[key]; !ok || len(args) != 2 || !optionFlag.MatchString(args[0]) || args[1] != "{"+key+"}" {
			return invalid("invalid args_if_set for %s", key)
		}
	}
	seen := map[string]bool{}
	for key, env := range o.Env {
		for name, value := range env {
			if _, ok := o.Fields[key]; !ok || !envName.MatchString(name) || reservedEnv.MatchString(name) || sensitive.MatchString(name) || value != "{"+key+"}" || seen[name] {
				return invalid("invalid env_if_set for %s", key)
			}
			seen[name] = true
		}
	}
	for _, dir := range o.Cwd {
		if dir != "run_dir" && dir != "project_checkout" {
			return invalid("invalid cwd choice")
		}
	}
	return nil
}

func (o *InstanceOptions) check(values map[string]string) error {
	for key, value := range values {
		if o != nil && key == "cwd" && slices.Contains(o.Cwd, value) {
			continue
		}
		if o == nil || !optionValue.MatchString(value) {
			return invalid("invalid option %s", key)
		}
		field := o.Fields[key]
		if field.Type == "enum" && slices.Contains(field.Choices, value) {
			continue
		}
		if field.Type == "string" && regexp.MustCompile("^(?:"+field.Pattern+")$").MatchString(value) {
			continue
		}
		return invalid("value outside declared range for %s", key)
	}
	return nil
}

func (o *InstanceOptions) expand(values map[string]string) (args []string, env map[string]string, model string) {
	if o == nil {
		return
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		value := values[key]
		if argv := o.Args[key]; len(argv) > 0 {
			args = append(args, argv[0], value)
		}
		for name := range o.Env[key] {
			if env == nil {
				env = map[string]string{}
			}
			env[name] = value
		}
		if o.Fields[key].DisplayAs == "model" {
			model = value
		}
	}
	return
}
