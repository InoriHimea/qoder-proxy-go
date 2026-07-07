package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/tetratelabs/wazero"
)

func main() {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile("wasm/qoder_auth_wasm_bg.wasm")
	if err != nil {
		panic(err)
	}
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		panic(err)
	}

	fmt.Println("=== IMPORTS ===")
	imps := compiled.ImportedFunctions()
	type imp struct{ mod, name, sig string }
	var list []imp
	for _, f := range imps {
		m, n, _ := f.Import()
		params := f.ParamTypes()
		results := f.ResultTypes()
		list = append(list, imp{m, n, fmt.Sprintf("params=%v results=%v", params, results)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	for _, e := range list {
		fmt.Printf("  [%s] %s  %s\n", e.mod, e.name, e.sig)
	}
	fmt.Printf("total imports: %d\n\n", len(list))

	fmt.Println("=== EXPORTS (functions) ===")
	exps := compiled.ExportedFunctions()
	var names []string
	for n := range exps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := exps[n]
		fmt.Printf("  %s  params=%v results=%v\n", n, f.ParamTypes(), f.ResultTypes())
	}
	fmt.Printf("total exported funcs: %d\n\n", len(names))

	fmt.Println("=== EXPORTED MEMORIES ===")
	for n := range compiled.ExportedMemories() {
		fmt.Printf("  memory: %s\n", n)
	}
}
