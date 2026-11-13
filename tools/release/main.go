package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
)

// 用法：go run ./tools/release [-out dist/release/apa] [-skip-web] [-goos X -goarch Y] [-no-verify] [-pack dist/]
// 构建发布二进制并在干净环境验证；交叉编译时无法在本机运行，须加 -no-verify。
func main() { os.Exit(run(".", os.Args[1:], os.Stdout, os.Stderr)) }

func run(root string, args []string, stdout, stderr io.Writer) int {
	var c Config
	fl := flag.NewFlagSet("release", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("out", "build/release/apa", "output binary path")
	noVerify := fl.Bool("no-verify", false, "skip the clean-environment run (required when cross-compiling)")
	fl.BoolVar(&c.SkipWeb, "skip-web", false, "reuse an existing web/dist instead of running npm")
	pack := fl.String("pack", "", "build the full versioned release package (binary, sources, exts, plugins, deploy, docs, SHA256SUMS) into this directory")
	fl.StringVar(&c.GOOS, "goos", "", "target GOOS (default host)")
	fl.StringVar(&c.GOARCH, "goarch", "", "target GOARCH (default host)")
	if fl.Parse(args) != nil {
		return 2
	}
	c.Root, c.Out, c.Log = root, *out, stderr
	cross := (c.GOOS != "" && c.GOOS != runtime.GOOS) || (c.GOARCH != "" && c.GOARCH != runtime.GOARCH)
	if *pack != "" {
		c.Out = *pack
		dir, err := Package(c)
		if err == nil && !*noVerify && !cross {
			err = VerifyPackage(dir)
		}
		if err != nil {
			fmt.Fprintln(stderr, "release: package:", err)
			return 1
		}
		fmt.Fprintln(stdout, "release: ok", dir)
		return 0
	}
	if err := Build(c); err != nil {
		fmt.Fprintln(stderr, "release: build:", err)
		return 1
	}
	if !*noVerify && !cross {
		if err := Verify(c.Out); err != nil {
			fmt.Fprintln(stderr, "release: verify:", err)
			return 1
		}
	}
	fmt.Fprintln(stdout, "release: ok", c.Out)
	return 0
}
