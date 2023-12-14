package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/template"
)

var (
	input  = flag.String("i", "", "directory which holds the source files of package")
	output = flag.String("o", ".", "target file location of the generated package module file")
	tmpl   = flag.String("t", "gomodule.tmpl", "template file for the generating package module file")

	notypes  = flag.Bool("notypes", false, "Don't list package types")
	nofuncs  = flag.Bool("nofuncs", false, "Don't list package functions")
	novars   = flag.Bool("novars", false, "Don't list package variables")
	noconsts = flag.Bool("noconsts", false, "Don't list package consts")

	data Data
)

type Func struct {
	Name       string
	Receiver   string
	Signature  string
	Params     string
	ParamNames string
	Results    string
}

type Data struct {
	GoPackage    string
	PackageName  string
	StructName   string
	ReceiverName string
	Imports      []string
	Funcs        []Func
}

func checkFlag(f flag.Flag) {
	if f.Value.String() == "" {
		checkErr(fmt.Errorf("missing flag \"%s\" definition: %s\n", f.Name, f.Usage))
	}
}

func checkErr(err error) {
	if err == nil {
		return
	}

	fmt.Fprintf(os.Stderr, err.Error())

	os.Exit(1)
}

func filter(info os.FileInfo) bool {
	name := info.Name()

	if info.IsDir() {
		return false
	}

	if name == *output {
		return false
	}

	if filepath.Ext(name) != ".go" {
		return false
	}

	if strings.HasSuffix(name, "_test.go") {
		return false
	}

	return true
}

func mapContains[K comparable, V any](m map[K]V, k K) bool {
	_, ok := m[k]

	return ok
}

func mapSorted[K cmp.Ordered, V any](m map[K]V) []V {
	ks := make([]K, 0)
	for k := range m {
		ks = append(ks, k)
	}

	sort.Slice(ks, func(i, j int) bool {
		return ks[i] < ks[j]
	})

	vs := make([]V, 0)
	for _, k := range ks {
		vs = append(vs, m[k])
	}

	return vs
}

func formatType(typ ast.Expr) string {
	fn := func() string {
		switch t := typ.(type) {
		case nil:
			return ""
		case *ast.Ident:
			if !strings.Contains(t.Name, ".") && t.IsExported() {
				return data.GoPackage + "." + t.Name
			} else {
				return t.Name
			}
		case *ast.SelectorExpr:
			return fmt.Sprintf("%s.%s", formatType(t.X), t.Sel.Name)
		case *ast.StarExpr:
			return fmt.Sprintf("*%s", formatType(t.X))
		case *ast.ArrayType:
			return fmt.Sprintf("[%s]%s", formatType(t.Len), formatType(t.Elt))
		case *ast.Ellipsis:
			return formatType(t.Elt)
		case *ast.FuncType:
			return fmt.Sprintf("func(%s)%s", formatFuncFields(t.Params, true), formatFuncResults(t.Results))
		case *ast.MapType:
			return fmt.Sprintf("map[%s]%s", formatType(t.Key), formatType(t.Value))
		case *ast.ChanType:
			// FIXME
			panic(fmt.Errorf("unsupported chan type %#v", t))
		case *ast.BasicLit:
			return t.Value
		default:
			panic(fmt.Errorf("unsupported type %#v", t))
		}
	}

	result := fn()

	return result
}

func formatFuncFields(fields *ast.FieldList, inclType bool) string {
	s := ""
	for i, field := range fields.List {
		for j, name := range field.Names {
			s += name.Name
			if j != len(field.Names)-1 {
				s += ","
			}
			s += " "
		}

		if inclType {
			s += formatType(field.Type)
		}
		if i != len(fields.List)-1 {
			s += ", "
		}
	}

	return strings.TrimSpace(s)
}

func formatFuncResults(fields *ast.FieldList) string {
	s := ""
	if fields != nil {
		if len(fields.List) > 1 {
			s += "("
		}
		s += formatFuncFields(fields, true)
		if len(fields.List) > 1 {
			s += ")"
		}
	}
	return s
}

func formatFuncDecl(decl *ast.FuncDecl) (Func, error) {
	f := Func{}

	if decl.Recv != nil {
		if len(decl.Recv.List) != 1 {
			return f, fmt.Errorf("strange receiver for %s: %#v", decl.Name.Name, decl.Recv)
		}
		field := decl.Recv.List[0]
		if len(field.Names) == 0 {
			// function definition in interface (ignore)
			return f, nil
		} else if len(field.Names) != 1 {
			return f, fmt.Errorf("strange receiver field for %s: %#v", decl.Name.Name, field)
		}
		f.Receiver = fmt.Sprintf("(%s %s) ", field.Names[0], formatType(field.Type))
	}

	f.Name = decl.Name.Name
	f.Params = fmt.Sprintf("(%s)", formatFuncFields(decl.Type.Params, true))
	f.ParamNames = fmt.Sprintf("(%s)", formatFuncFields(decl.Type.Params, false))
	f.Results = formatFuncResults(decl.Type.Results)

	return f, nil
}

func addImport(src string, pkgName string) {
	if !slices.Contains(data.Imports, pkgName) && !strings.HasPrefix(pkgName, "internal/") {
		data.Imports = append(data.Imports, pkgName)
	}
}

func scan(data *Data, path string, pkg *ast.Package, kind ast.ObjKind) error {
	funcs := make(map[string]Func)

	for _, f := range pkg.Files {
		for _, i := range f.Imports {
			if i.Path.Value == "" {
				continue
			}

			name := i.Path.Value[1 : len(i.Path.Value)-1]

			addImport(f.Name.Name, name)
		}

		for name, object := range f.Scope.Objects {
			if object.Kind == kind && ast.IsExported(name) && !mapContains(funcs, name) {
				fd := object.Decl.(*ast.FuncDecl)

				f, err := formatFuncDecl(fd)
				checkErr(err)

				if f.Name == "" {
					continue
				}

				funcs[f.Name] = f
			}
		}
	}

	sort.Strings(data.Imports)

	data.Funcs = mapSorted(funcs)

	return nil
}

func firstUpper(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstLower(s string) string {
	return strings.ToLower(s[:1]) + s[1:]
}

func (td Data) Fn() string {
	return "Hello world!"
}

func main() {
	flag.Parse()

	flag.VisitAll(func(f *flag.Flag) {
		checkFlag(*f)
	})

	fi, err := os.Stat(*input)
	checkErr(err)

	if !fi.IsDir() {
		checkErr(fmt.Errorf("not a directory: %s", *input))
	}

	pkgName := "Goja_" + filepath.Base(*input)

	data = Data{
		GoPackage:    filepath.Base(*input),
		PackageName:  strings.ToLower(pkgName),
		StructName:   pkgName,
		ReceiverName: strings.ToLower(pkgName[:1]),
		Imports:      []string{"github.com/dop251/goja", data.GoPackage},
		Funcs:        nil,
	}

	pkgs, err := parser.ParseDir(token.NewFileSet(), *input, filter, 0)
	checkErr(err)

	for _, pkg := range pkgs {
		checkErr(scan(&data, *input, pkg, ast.Fun))
	}

	tmpl, err := template.New(*tmpl).ParseFiles(*tmpl)
	checkErr(err)

	var buffer bytes.Buffer

	err = tmpl.Execute(&buffer, data)
	checkErr(err)
	if err != nil {
		panic(err)
	}

	filename := strings.ToLower(pkgName) + ".go"

	err = os.WriteFile(filename, buffer.Bytes(), os.ModePerm)
	checkErr(err)

	//astFile, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	//checkErr(err)
	//
	//fset := token.NewFileSet()
	//ast.Inspect(astFile, func(node ast.Node) bool {
	//	switch n := node.(type) {
	//	case *ast.CallExpr:
	//		fmt.Println(n) // prints every func call expression
	//	}
	//	return true
	//
	//	var s string
	//	switch x := node.(type) {
	//	//case *ast.FuncDecl:
	//	//	s = x.Name.Name
	//	//case *ast.BasicLit:
	//	//	s = x.Value
	//	//case *ast.Ident:
	//	//	s = x.Name
	//	case *ast.CallExpr:
	//		s = x.
	//	}
	//	if s != "" {
	//		fmt.Printf("%s:\t%s\n", fset.Position(node.Pos()), s)
	//	}
	//	return true
	//})
}
