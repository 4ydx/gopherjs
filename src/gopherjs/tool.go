package main

import (
	"bufio"
	"code.google.com/p/go.tools/go/types"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"gopherjs/gcexporter"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

type Translator struct {
	buildContext *build.Context
	typesConfig  *types.Config
	fileSet      *token.FileSet
	packages     map[string]*GopherPackage
}

type GopherPackage struct {
	*build.Package
	SrcLastModified time.Time
	JavaScriptCode  []byte
}

func main() {
	var pkg *GopherPackage

	var previousErr string
	var t *Translator
	t = &Translator{
		buildContext: &build.Context{
			GOROOT:        build.Default.GOROOT,
			GOPATH:        build.Default.GOPATH,
			GOOS:          build.Default.GOOS,
			GOARCH:        build.Default.GOARCH,
			Compiler:      "gc",
			InstallSuffix: "js",
		},
		typesConfig: &types.Config{
			Packages: make(map[string]*types.Package),
			Import: func(imports map[string]*types.Package, path string) (*types.Package, error) {
				return imports[path], nil
			},
			Error: func(err error) {
				if err.Error() != previousErr {
					fmt.Println(err.Error())
				}
				previousErr = err.Error()
			},
		},
		fileSet:  token.NewFileSet(),
		packages: make(map[string]*GopherPackage),
	}

	flag.Parse()

	cmd := flag.Arg(0)
	switch cmd {
	case "install":
		buildPkg, err := t.buildContext.Import(flag.Arg(1), "", 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		pkg = &GopherPackage{Package: buildPkg}
		pkg.PkgObj = pkg.BinDir + "/" + path.Base(pkg.ImportPath) + ".js"

	case "build", "run":
		filename := flag.Arg(1)
		file, err := parser.ParseFile(t.fileSet, filename, nil, parser.ImportsOnly)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		imports := make([]string, len(file.Imports))
		for i, imp := range file.Imports {
			imports[i] = imp.Path.Value[1 : len(imp.Path.Value)-1]
		}

		basename := path.Base(filename)
		pkg = &GopherPackage{
			Package: &build.Package{
				Name:       "main",
				ImportPath: "main",
				Imports:    imports,
				Dir:        path.Dir(filename),
				GoFiles:    []string{basename},
				PkgObj:     basename[:len(basename)-3] + ".js",
			},
		}

	case "help", "":
		os.Stderr.WriteString(`GopherJS is a tool for compiling Go source code to JavaScript.

Usage:

    gopherjs command [arguments]

The commands are:

    build       compile packages and dependencies
    install     compile and install packages and dependencies
    run         compile and run Go program

`)
		return

	default:
		fmt.Fprintf(os.Stderr, "gopherjs: unknown subcommand \"%s\"\nRun 'gopherjs help' for usage.\n", cmd)
		return
	}

	err := t.buildPackage(pkg, cmd != "run")
	if err != nil {
		list, isList := err.(scanner.ErrorList)
		if !isList {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		for _, entry := range list {
			fmt.Fprintln(os.Stderr, entry)
		}
		return
	}

	if cmd == "run" {
		node := exec.Command("node")
		pipe, _ := node.StdinPipe()
		node.Stdout = os.Stdout
		node.Stderr = os.Stderr
		err = node.Start()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		pipe.Write(pkg.JavaScriptCode)
		pipe.Close()
		node.Wait()
	}
}

func (t *Translator) getPackage(importPath string, srcDir string, writeToDisk bool) (*GopherPackage, error) {
	if pkg, found := t.packages[importPath]; found {
		return pkg, nil
	}

	otherPkg, err := t.buildContext.Import(importPath, srcDir, 0)
	if err != nil {
		return nil, err
	}
	pkg := &GopherPackage{Package: otherPkg}
	t.packages[importPath] = pkg
	if err := t.buildPackage(pkg, true); err != nil {
		return nil, err
	}
	return pkg, nil
}

func (t *Translator) buildPackage(pkg *GopherPackage, writeToDisk bool) error {
	if pkg.ImportPath == "unsafe" {
		t.typesConfig.Packages["unsafe"] = types.Unsafe
		return nil
	}

	fileInfo, err := os.Stat(os.Args[0]) // gopherjs itself
	if err != nil {
		return err
	}
	pkg.SrcLastModified = fileInfo.ModTime()

	for _, importedPkgPath := range pkg.Imports {
		compiledPkg, err := t.getPackage(importedPkgPath, pkg.Dir, true)
		if err != nil {
			return err
		}
		if compiledPkg.SrcLastModified.After(pkg.SrcLastModified) {
			pkg.SrcLastModified = compiledPkg.SrcLastModified
		}
	}

	for _, name := range pkg.GoFiles {
		fileInfo, err := os.Stat(pkg.Dir + "/" + name)
		if err != nil {
			return err
		}
		if fileInfo.ModTime().After(pkg.SrcLastModified) {
			pkg.SrcLastModified = fileInfo.ModTime()
		}
	}

	fileInfo, err = os.Stat(pkg.PkgObj)
	if err == nil && fileInfo.ModTime().After(pkg.SrcLastModified) && writeToDisk {
		// package object is up to date, load from disk if library
		if pkg.IsCommand() {
			return nil
		}

		objFile, err := os.Open(pkg.PkgObj)
		if err != nil {
			return err
		}
		defer objFile.Close()

		t.typesConfig.Packages[pkg.ImportPath], err = types.GcImportData(t.typesConfig.Packages, pkg.PkgObj, pkg.ImportPath, bufio.NewReader(objFile))
		if err != nil {
			return err
		}

		// search backwards for $$ line
		buf := make([]byte, 3)
		objFile.Read(buf)
		for string(buf) != "$$\n" {
			if _, err := objFile.Seek(-4, 1); err != nil {
				return nil // EOF
			}
			if _, err := objFile.Read(buf); err != nil {
				return err
			}
		}

		pkg.JavaScriptCode, err = ioutil.ReadAll(objFile)
		if err != nil {
			return err
		}

		return nil
	}

	packageCode, err := translatePackage(pkg.ImportPath, pkg.Dir, pkg.GoFiles, t.fileSet, t.typesConfig)
	if err != nil {
		return err
	}

	var jsCode []byte
	if pkg.IsCommand() {
		jsCode = []byte(strings.TrimSpace(prelude))
		jsCode = append(jsCode, '\n')

		loaded := make(map[*types.Package]bool)
		var loadImportsOf func(*types.Package) error
		loadImportsOf = func(typesPkg *types.Package) error {
			for _, imp := range typesPkg.Imports() {
				if imp.Path() == "unsafe" || imp.Path() == "reflect" || imp.Path() == "go/doc" {
					continue
				}
				if _, alreadyLoaded := loaded[imp]; alreadyLoaded {
					continue
				}
				loaded[imp] = true

				if err := loadImportsOf(imp); err != nil {
					return err
				}

				gopherPkg, err := t.getPackage(imp.Path(), pkg.Dir, false)
				if err != nil {
					return err
				}

				jsCode = append(jsCode, []byte(`Go$packages["`+imp.Path()+`"] = (function() {`)...)
				jsCode = append(jsCode, gopherPkg.JavaScriptCode...)
				exports := make([]string, 0)
				for _, name := range imp.Scope().Names() {
					if ast.IsExported(name) {
						exports = append(exports, fmt.Sprintf("%s: %s", name, name))
					}
				}
				jsCode = append(jsCode, []byte("\treturn { "+strings.Join(exports, ", ")+" };\n")...)
				jsCode = append(jsCode, []byte("})();\n")...)
			}
			return nil
		}
		if err := loadImportsOf(t.typesConfig.Packages[pkg.ImportPath]); err != nil {
			return err
		}
	}
	jsCode = append(jsCode, packageCode...)
	if pkg.IsCommand() {
		jsCode = append(jsCode, []byte("main();")...)
	}
	pkg.JavaScriptCode = jsCode

	if !writeToDisk {
		return nil
	}

	if err := os.MkdirAll(path.Dir(pkg.PkgObj), 0777); err != nil {
		return err
	}
	var perm os.FileMode = 0666
	if pkg.IsCommand() {
		perm = 0777
	}
	file, err := os.OpenFile(pkg.PkgObj, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if pkg.IsCommand() {
		file.Write([]byte("#!/usr/bin/env node\n"))
	}
	if !pkg.IsCommand() {
		gcexporter.Write(t.typesConfig.Packages[pkg.ImportPath], file)
	}
	file.Write(pkg.JavaScriptCode)
	file.Close()
	return nil
}
