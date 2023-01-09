package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

func main() {
	filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if path != "" && path[0] == '.' {
			return nil
		}
		fmt.Println(path, "shiny baban")
		return nil
	})
}
