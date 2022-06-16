package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type (
	queryFinder struct {
		packageInfo    map[string]string
		queries        []string
		nonUniqueNames map[string]struct{}
	}
)

func main() {
	var (
		sourcePackageName = flag.String("f", "", "source package import path, i.e. github.com/my/package")
	)
	flag.Parse()

	if *sourcePackageName == "" {
		flag.PrintDefaults()
		return
	}

	var (
		sourcePackage *packages.Package
		astPackage    *ast.Package
		fs            *token.FileSet
		err           error
	)

	if sourcePackage, err = Load(*sourcePackageName); err != nil {
		log.Fatalf("prep: %v", err)
	}

	fs = token.NewFileSet()
	if astPackage, err = AST(fs, sourcePackage); err != nil {
		log.Fatalf("failed to load package sources: %v", err)
	}

	finder := &queryFinder{
		packageInfo:    map[string]string{},
		nonUniqueNames: map[string]struct{}{},
	}

	for k, v := range sourcePackage.TypesInfo.Defs {
		if constant, ok := v.(*types.Const); ok {
			if _, ok = finder.packageInfo[k.Name]; ok {
				finder.nonUniqueNames[k.Name] = struct{}{}
				continue
			}
			finder.packageInfo[k.Name] = constant.Val().ExactString()
		}
	}

	for _, file := range astPackage.Files {
		ast.Walk(finder, file)
	}

	path, err := getPathToPackage(*sourcePackageName)
	if err != nil {
		log.Fatalf("prep: %v", err)
	}

	outputFileName := filepath.Join(path, "prepared_statements.go")

	queries := uniqueStrings(finder.queries)
	code := generateCode(astPackage.Name, *sourcePackageName, queries)
	file, err := os.Create(outputFileName)
	if err != nil {
		log.Fatalf("prep: failed to create file: %v", err)
	}
	defer file.Close()

	if _, err := file.Write(code); err != nil {
		log.Fatalf("prep: failed to write generated code to the file: %v", err)
	}
}

func getPathToPackage(importPath string) (string, error) {
	p, err := build.Default.Import(importPath, "", build.FindOnly)
	if err != nil {
		return "", fmt.Errorf("failed to detect absolute path of the package %q: %v", importPath, err)
	}

	return filepath.Clean(p.Dir), nil
}

func generateCode(packageName, importPath string, queries []string) []byte {
	buf := bytes.NewBuffer([]byte{})

	if len(queries) == 0 {
		fmt.Fprintf(buf,
			"//go:generate prep -f %s\n\npackage %s\n\nfunc init() {\n\tprepStatements = []string{}\n}",
			importPath, packageName)

		return buf.Bytes()
	}

	fmt.Fprintf(buf,
		"//go:generate prep -f %s\n\npackage %s\n\nfunc init() {\n\tprepStatements = []string{\n\t\t%s,\n\t}\n}",
		importPath, packageName, strings.Join(queries, ",\n\t\t"))
	return buf.Bytes()
}

// uniqueStrings returns a sorted slice of the unique strings
// from the given strings slice
func uniqueStrings(strings []string) []string {
	m := make(map[string]struct{})
	for _, s := range strings {
		m[s] = struct{}{}
	}

	var unique []string
	for s := range m {
		unique = append(unique, s)
	}

	sort.Strings(unique)
	return unique
}

// maps method name to the interface it implements
var methodImplements = map[string]string{
	"ExecContext":         "ExecContext",
	"QueryContext":        "QueryContext",
	"QueryRowContext":     "QueryRowContext",
	"NamedExecContext":    "NamedExecContext",
	"GetContext":          "GetContext",
	"SelectContext":       "SelectContext",
	"NamedQueryContext":   "NamedQueryContext",
	"PrepareContext":      "PrepareContext",
	"PrepareNamedContext": "PrepareNamedContext",
}

// Visit implements ast.Visitor interface
func (f *queryFinder) Visit(node ast.Node) ast.Visitor {
	fCall, ok := node.(*ast.CallExpr)
	if !ok {
		return f
	}

	selector, ok := fCall.Fun.(*ast.SelectorExpr)
	if !ok {
		return f
	}

	interfaceName := methodImplements[selector.Sel.Name]
	if interfaceName == "" {
		return f
	}

	var query string
	switch selector.Sel.Name {
	case "ExecContext", "QueryContext", "QueryRowContext", "NamedExecContext", "NamedQueryContext", "PrepareContext", "PrepareNamedContext":
		query = f.processQuery(fCall.Args[1])
	case "GetContext", "SelectContext":
		query = f.processQuery(fCall.Args[2])
	}

	if query != "" {
		f.queries = append(f.queries, query)
	}

	return nil
}

// processQuery returns a string value of the expression if the
// expression is either a string literal or a string constant otherwise
// an empty string is returned
func (f *queryFinder) processQuery(queryArg ast.Expr) string {
	switch q := queryArg.(type) {
	case *ast.BasicLit:
		return q.Value
	case *ast.Ident:
		if _, ok := f.nonUniqueNames[q.Name]; ok {
			log.Fatalf("constant already defined, need unique name for %v", q.Name)
		}
		return f.packageInfo[q.Name]
	}
	return ""
}

var errPackageNotFound = errors.New("package not found")

// Load loads package by its import path
func Load(path string) (*packages.Package, error) {
	cfg := &packages.Config{Mode: packages.LoadSyntax}
	pkgs, err := packages.Load(cfg, path)
	if err != nil {
		return nil, err
	}

	if len(pkgs) < 1 {
		return nil, errPackageNotFound
	}

	if len(pkgs[0].Errors) > 0 {
		return nil, pkgs[0].Errors[0]
	}

	return pkgs[0], nil
}

// AST returns package's abstract syntax tree
func AST(fs *token.FileSet, p *packages.Package) (*ast.Package, error) {
	dir := Dir(p)

	pkgs, err := parser.ParseDir(fs, dir, nil, parser.DeclarationErrors)
	if err != nil {
		return nil, err
	}

	if ap, ok := pkgs[p.Name]; ok {
		return ap, nil
	}

	return &ast.Package{Name: p.Name}, nil
}

// Dir returns absolute path of the package in a filesystem
func Dir(p *packages.Package) string {
	files := append(p.GoFiles, p.OtherFiles...)
	if len(files) < 1 {
		return p.PkgPath
	}

	return filepath.Dir(files[0])
}
