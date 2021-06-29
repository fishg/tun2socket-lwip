package main

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const generatedFile = `
// Code generated .* DO NOT EDIT.

// +build %%%GOOS%%%,%%%GOARCH%%%

package %%%PACKAGE%%%

/*
#cgo CFLAGS: -I"%%%NATIVE_PATH%%%" -fPIC -O3
#cgo LDFLAGS: -L"%%%BUILD_PATH%%%" -lnative
*/
import "C"

const BuildNativeMD5 = "%%%NATIVE_MD5%%%"
`

type Kind int

const (
	Header Kind = iota
	Source
)

type source struct {
	kind     Kind
	path     string
	modified time.Time
}

func main() {
	if len(os.Args) < 5 {
		println("Usage: <package-name> <project-root> <target-GOOS> <target-GOARCH>")

		os.Exit(-1)
	}

	packageName := os.Args[1]
	projectRoot, _ := filepath.Abs(os.Args[2])
	GOOS := os.Args[3]
	GOARCH := os.Args[4]
	cc := ""
	ar := ""
	ranlib := ""
	var ccArgs []string
	if len(os.Args) > 6 {
		cc = os.Args[5]
		ar = os.Args[6]

		ccArgs = strings.Split(cc, " ")
		if len(ccArgs) == 1 {
			ccArgs = []string{}
		} else if len(ccArgs) > 1 {
			cc = ccArgs[0]
			ccArgs = ccArgs[1:]
		}
	}

	if len(os.Args) > 7 {
		ranlib = os.Args[7]
	}

	buildDir := filepath.Join(projectRoot, "build", GOOS, GOARCH)
	target := filepath.Join(buildDir, "libnative.a")

	sources, err := collectSources(projectRoot)
	if err != nil {
		println("Collect sources: " + err.Error())

		os.Exit(1)
	}

	changed, err := filterChanged(sources, target)
	if err != nil {
		println("Filter changed: " + err.Error())

		os.Exit(1)
	}

	changed = filterSource(changed)

	if len(changed) == 0 {
		println("Everything update to date")

		return
	}

	if cc == "" {
		cc = os.Getenv("CC")
		if cc == "" {
			cc, err = findCc()
			if err != nil {
				println("Search CC: " + err.Error())

				os.Exit(1)
			}
		}
	}

	println("Using CC: " + cc)
	println("Using CCArgs: " + strings.Join(ccArgs, " "))

	lwipInclude := filepath.Join(projectRoot, "lwip", "include")
	lwipArchInclude := filepath.Join(projectRoot, "lwip", "ports", "unix", "include")
	nativeInclude := filepath.Join(projectRoot, "native")

	externalCFlags, err := parseCommandLine(os.Getenv("CFLAGS"))
	if err != nil {
		println("Parse CFLAGS: " + err.Error())

		os.Exit(1)
	}

	cFlags := []string{"-Ofast", "-I" + lwipInclude, "-I" + nativeInclude, "-I" + lwipArchInclude}
	if GOOS != "windows" {
		cFlags = append([]string{"-fPIC"}, cFlags...)
	}
	cFlags = append(cFlags, externalCFlags...)
	objsDir := filepath.Join(buildDir, "objs")

	var objs []string

	for i, s := range changed {
		if s.kind == Header {
			continue
		}

		obj := filepath.Join(objsDir, s.path+".o")

		if err := os.MkdirAll(filepath.Dir(obj), 0700); err != nil {
			println("Create dir " + filepath.Dir(obj) + ": " + err.Error())

			os.Exit(1)
		}

		fmt.Printf("[%2d/%2d] %s\n", i+1, len(changed), s.path)

		ccCmdArgs := []string{"-c", "-o", obj, filepath.Join(projectRoot, s.path)}
		if len(ccArgs) > 0 {
			ccCmdArgs = append(ccArgs, ccCmdArgs...)
		}

		ccCmdArgs = append(ccCmdArgs, cFlags...)

		runCommand(append([]string{cc}, ccCmdArgs...))

		objs = append(objs, obj)
	}

	if ar == "" {
		ar = os.Getenv("AR")
		if ar == "" {
			ar, err = exec.LookPath("ar")
			if err != nil {
				println("C archiver ar unavailable: " + err.Error())

				os.Exit(1)
			}
		}
	}

	if runtime.GOOS == "darwin" {
		if ranlib == "" {
			ranlib = os.Getenv("RANLIB")
			if ranlib == "" {
				ranlib, err = exec.LookPath("ranlib")
				if err != nil {
					println("ranlib unavailable: " + err.Error())

					os.Exit(1)
				}
			}
		}

		println("Using AR: " + ar)
		runCommand(append([]string{ar, "Scr", target}, objs...))

		println("Using ranlib: " + ranlib)
		runCommand([]string{ranlib, "-no_warning_for_no_symbols", "-c", target})
	} else {
		println("Using AR: " + ar)
		runCommand(append([]string{ar, "rcs", target}, objs...))
	}

	replacer := strings.NewReplacer(
		"%%%PACKAGE%%%", packageName,
		"%%%GOOS%%%", GOOS,
		"%%%GOARCH%%%", GOARCH,
		"%%%NATIVE_PATH%%%", strings.ReplaceAll(filepath.Join(projectRoot, "native"), "\\", "/:"),
		"%%%BUILD_PATH%%%", strings.ReplaceAll(buildDir, "\\", "/"),
		"%%%NATIVE_MD5%%%", fileMd5(target),
	)

	err = ioutil.WriteFile(
		filepath.Join(projectRoot, "build_"+GOOS+"_"+GOARCH+".go"),
		[]byte(strings.TrimSpace(replacer.Replace(generatedFile))),
		0644,
	)
	if err != nil {
		println("Write generated file:", err.Error())

		os.Exit(1)
	}
}

func runCommand(command []string) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		fmt.Printf("%s: %s\n", command, err.Error())

		os.Exit(1)
	}
}

func fileMd5(filepath string) string {
	file, err := os.Open(filepath)
	if err != nil {
		println("Open " + filepath + ": " + err.Error())

		os.Exit(1)
	}

	m := md5.New()

	_, err = io.Copy(m, file)
	if err != nil {
		println("Read " + filepath + ": " + err.Error())

		os.Exit(1)
	}

	return hex.EncodeToString(m.Sum(nil))
}

func collectSources(root string) ([]*source, error) {
	var sources []*source

	return sources, filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && strings.HasPrefix(info.Name(), ".") {
			return filepath.SkipDir
		}

		switch filepath.Ext(path) {
		case ".h":
			p, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}

			s := &source{
				kind:     Header,
				path:     p,
				modified: info.ModTime(),
			}

			sources = append(sources, s)
		case ".c":
			p, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}

			s := &source{
				kind:     Source,
				path:     p,
				modified: info.ModTime(),
			}

			sources = append(sources, s)
		}

		return nil
	})
}

func filterChanged(sources []*source, target string) ([]*source, error) {
	var changed []*source

	stat, err := os.Stat(target)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	modified := time.Time{}
	if stat != nil {
		modified = stat.ModTime()
	}

	for _, s := range sources {
		if s.modified.Before(modified) {
			continue
		}

		if s.kind == Header {
			return sources, nil
		}

		changed = append(changed, s)
	}

	return changed, nil
}

func filterSource(sources []*source) []*source {
	var r []*source

	for _, s := range sources {
		if s.kind == Header {
			continue
		}

		r = append(r, s)
	}

	return r
}

func findCc() (string, error) {
	if p, err := exec.LookPath("clang"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("gcc"); err == nil {
		return p, nil
	}

	return "", errors.New("C Compiler clang/gcc unavailable in filepath")
}

// from https://stackoverflow.com/questions/34118732/parse-a-command-line-string-into-flags-and-arguments-in-golang
func parseCommandLine(command string) ([]string, error) {
	var args []string
	state := "start"
	current := ""
	quote := "\""
	escapeNext := true
	for i := 0; i < len(command); i++ {
		c := command[i]

		if state == "quotes" {
			if string(c) != quote {
				current += string(c)
			} else {
				args = append(args, current)
				current = ""
				state = "start"
			}
			continue
		}

		if escapeNext {
			current += string(c)
			escapeNext = false
			continue
		}

		if c == '\\' {
			escapeNext = true
			continue
		}

		if c == '"' || c == '\'' {
			state = "quotes"
			quote = string(c)
			continue
		}

		if state == "arg" {
			if c == ' ' || c == '\t' {
				args = append(args, current)
				current = ""
				state = "start"
			} else {
				current += string(c)
			}
			continue
		}

		if c != ' ' && c != '\t' {
			state = "arg"
			current += string(c)
		}
	}

	if state == "quotes" {
		return []string{}, errors.New(fmt.Sprintf("Unclosed quote in command line: %s", command))
	}

	if current != "" {
		args = append(args, current)
	}

	return args, nil
}
