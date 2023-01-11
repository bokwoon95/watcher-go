package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if strings.HasPrefix(path, ".") {
			return nil
		}
		if filepath.Base(path) == ".git" {
			return fs.SkipDir
		}
		fmt.Println(filepath.ToSlash(path))
		return nil
	})
}
