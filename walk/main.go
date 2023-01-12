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
	//https://stackoverflow.com/questions/17817204/how-to-set-ulimit-n-from-a-golang-program 
	// var rLimit syscall.Rlimit
	// err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	// if err != nil {
	// 	fmt.Println("Error Getting Rlimit ", err)
	// }
	// fmt.Println(rLimit)
	// rLimit.Max = 999999
	// rLimit.Cur = 999999
	// err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	// if err != nil {
	// 	fmt.Println("Error Setting Rlimit ", err)
	// }
	// err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	// if err != nil {
	// 	fmt.Println("Error Getting Rlimit ", err)
	// }
	// fmt.Println("Rlimit Final", rLimit)
}
