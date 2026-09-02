// Command gen writes THIRD-PARTY-NOTICES.md at the module root. Run it after
// any dependency change: go run ./internal/notices/gen
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/colespringer/waxtap/v3/internal/notices"
)

func main() {
	ctx := context.Background()
	root, err := notices.ModuleRoot(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := notices.Generate(ctx, notices.BinaryPackage, notices.ReleaseTargets)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(root, notices.FileName)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote", path)
}
