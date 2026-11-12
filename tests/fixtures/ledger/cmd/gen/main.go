// gen 生成合成账本夹具：go run ./tests/fixtures/ledger/cmd/gen -version 2 -out /path/to/LEDGER_FIXTURE_V2
// 缺省 -version 为 0，表示为每个已冻结版本各生成一份到 <out>/V<n>，并打印 export 行。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

func main() {
	version := flag.Int("version", 0, "ledger structure version to derive (0 = every frozen version)")
	out := flag.String("out", "", "output directory (required)")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gen: -out is required")
		os.Exit(2)
	}
	if *version != 0 {
		if _, err := ledgerfixture.Generate(*out, *version); err != nil {
			fmt.Fprintln(os.Stderr, "gen:", err)
			os.Exit(1)
		}
		fmt.Printf("export LEDGER_FIXTURE_V%d=%s\n", *version, *out)
		return
	}
	var vs []int
	for v := range ledgerfixture.Frozen {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	for _, v := range vs {
		dir := filepath.Join(*out, fmt.Sprintf("V%d", v))
		if _, err := ledgerfixture.Generate(dir, v); err != nil {
			fmt.Fprintln(os.Stderr, "gen:", err)
			os.Exit(1)
		}
		fmt.Printf("export LEDGER_FIXTURE_V%d=%s\n", v, dir)
	}
}
