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
	base      = flag.String("b", ".", "base path to package")
	input     = flag.String("i", "", "package directory")
	output    = flag.String("o", "", "target directory of the generated package")
	pkgPrefix = flag.String("p", "goja_go_", "target package name prefix")
	tmpl      = flag.String("t", "goja_go.tmpl", "template file")
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
	InputPkg    string
	OutputPkg   string
	StructName  string
	ImportPaths []string
	Imports     []string
	Funcs       []Func
}

func checkFlag(f flag.Flag) {
	if f.Value.String() == "" {
		checkErr(fmt.Errorf("missing flag \"%s\": %s\n", f.Name, f.Usage))
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

func (data *Data) formatType(typ ast.Expr) string {
	fn := func() string {
		switch t := typ.(type) {
		case nil:
			return ""
		case *ast.Ident:
			if !strings.Contains(t.Name, ".") && t.IsExported() {
				return data.InputPkg + "." + t.Name
			} else {
				return t.Name
			}
		case *ast.SelectorExpr:
			return fmt.Sprintf("%s.%s", data.formatType(t.X), t.Sel.Name)
		case *ast.StarExpr:
			return fmt.Sprintf("*%s", data.formatType(t.X))
		case *ast.ArrayType:
			return fmt.Sprintf("[%s]%s", data.formatType(t.Len), data.formatType(t.Elt))
		case *ast.Ellipsis:
			return data.formatType(t.Elt)
		case *ast.FuncType:
			return fmt.Sprintf("func(%s)%s", data.formatFuncFields(t.Params, true), data.formatFuncResults(t.Results))
		case *ast.MapType:
			return fmt.Sprintf("map[%s]%s", data.formatType(t.Key), data.formatType(t.Value))
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

	p := strings.Index(result, ".")
	if p != -1 {
		pkgName := result[:p]

		data.addImport(pkgName)
	}

	return result
}

func (data *Data) formatFuncFields(fields *ast.FieldList, inclType bool) string {
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
			s += data.formatType(field.Type)
		}
		if i != len(fields.List)-1 {
			s += ", "
		}
	}

	return strings.TrimSpace(s)
}

func (data *Data) formatFuncResults(fields *ast.FieldList) string {
	s := ""
	if fields != nil {
		if len(fields.List) > 1 {
			s += "("
		}

		f := data.formatFuncFields(fields, true)

		if strings.Contains(f, ",") {
			f = fmt.Sprintf("(%s)", f)
		}

		s += f

		if len(fields.List) > 1 {
			s += ")"
		}
	}

	s = strings.ReplaceAll(s, "((", "(")
	s = strings.ReplaceAll(s, "))", ")")

	return s
}

func (data *Data) formatFuncDecl(decl *ast.FuncDecl) (Func, error) {
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
		f.Receiver = fmt.Sprintf("(%s %s) ", field.Names[0], data.formatType(field.Type))
	}

	f.Name = decl.Name.Name
	f.Params = fmt.Sprintf("(%s)", data.formatFuncFields(decl.Type.Params, true))
	f.ParamNames = fmt.Sprintf("(%s)", data.formatFuncFields(decl.Type.Params, false))
	f.Results = data.formatFuncResults(decl.Type.Results)

	return f, nil
}

func (data *Data) addImport(foundPkgName string) {
	pkgName := foundPkgName

	if strings.HasPrefix(pkgName, "*") {
		pkgName = pkgName[1:]
	}
	if strings.HasPrefix(pkgName, "[]") {
		pkgName = pkgName[2:]
	}

	for _, df := range data.ImportPaths {
		if strings.HasSuffix(df, "/"+pkgName) {
			pkgName = df

			break
		}
	}

	if slices.Contains(data.Imports, pkgName) || strings.HasPrefix(pkgName, "internal/") {
		return
	}

	data.Imports = append(data.Imports, pkgName)
}

func (data *Data) scan(path string, pkg *ast.Package, kind ast.ObjKind) error {
	funcs := make(map[string]Func)

	for _, file := range pkg.Files {
		for _, i := range file.Imports {
			if i.Path.Value == "" {
				continue
			}

			name := i.Path.Value[1 : len(i.Path.Value)-1]

			data.ImportPaths = append(data.ImportPaths, name)
		}

		for name, object := range file.Scope.Objects {
			if object.Kind == kind && ast.IsExported(name) && !mapContains(funcs, name) {
				fd := object.Decl.(*ast.FuncDecl)

				f, err := data.formatFuncDecl(fd)
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

func main() {
	fmt.Printf("GOJA_GO - create GO modules for GOJA\n\n")

	flag.Parse()

	if flag.NFlag() == 0 {
		flag.Usage()

		os.Exit(1)
	}

	flag.VisitAll(func(f *flag.Flag) {
		checkFlag(*f)
	})

	*input = strings.ReplaceAll(*input, "\\", "/")

	path := filepath.Join(*base, *input)

	fi, err := os.Stat(path)
	checkErr(err)

	if !fi.IsDir() {
		checkErr(fmt.Errorf("not a directory: %s", path))
	}

	outputPkg := strings.ToLower(*pkgPrefix + strings.ReplaceAll(*input, "/", "_"))
	inputPkg := filepath.Base(path)

	data := Data{
		InputPkg:    inputPkg,
		OutputPkg:   outputPkg,
		StructName:  strings.Title(outputPkg),
		ImportPaths: []string{*input},
		Imports:     nil,
		Funcs:       nil,
	}

	pkgs, err := parser.ParseDir(token.NewFileSet(), path, filter, 0)
	checkErr(err)

	for _, pkgName := range []string{"github.com/dop251/goja", *input} {
		data.addImport(pkgName)
	}

	for _, pkg := range pkgs {
		checkErr(data.scan(path, pkg, ast.Fun))
	}

	tmpl, err := template.New(*tmpl).ParseFiles(*tmpl)
	checkErr(err)

	var buffer bytes.Buffer

	checkErr(tmpl.Execute(&buffer, data))

	filename, err := filepath.Abs(filepath.Join(*output, outputPkg, strings.ToLower(outputPkg)+".go"))
	checkErr(err)

	fmt.Printf("%s\n", filename)

	checkErr(os.MkdirAll(filepath.Dir(filename), os.ModePerm))

	checkErr(os.WriteFile(filename, buffer.Bytes(), os.ModePerm))
}
