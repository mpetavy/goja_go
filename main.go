package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"github.com/mpetavy/common"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/mod/modfile"
)

var (
	gomodFile = flag.String("g", "", "path to go.mod file")
	pkgName   = flag.String("n", "", "package name")
	output    = flag.String("o", "", "target directory of the generated package")
	prefix    = flag.String("p", "goja_go_", "target package name prefix")
	tmpl      = flag.String("t", "goja_go.tmpl", "template file")
)

type Func struct {
	Name       string
	JsName     string
	Receiver   string
	Signature  string
	Params     string
	ParamNames string
	Results    string
}

type Data struct {
	InputPkg     string
	OutputPkg    string
	StructName   string
	JsStructName string
	ImportPaths  []string
	Imports      []string
	Funcs        []Func
}

//go:embed go.mod
var resources embed.FS

func init() {
	common.Init("", "", "", "", "create GOJA JS bridges to GO modules", "", "", "", &resources, nil, nil, run, 0)
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

func upper1st(s string) string {
	rs := []rune(s)
	rs[0] = unicode.ToUpper(rs[0])

	return string(rs)
}

func lower1st(s string) string {
	rs := []rune(s)
	rs[0] = unicode.ToLower(rs[0])

	return string(rs)
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
		data.addImport(result[:p])
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
	f.JsName = lower1st(f.Name)
	f.Params = fmt.Sprintf("(%s)", data.formatFuncFields(decl.Type.Params, true))
	f.ParamNames = fmt.Sprintf("(%s)", data.formatFuncFields(decl.Type.Params, false))
	f.Results = data.formatFuncResults(decl.Type.Results)

	return f, nil
}

func (data *Data) addImport(imprt string) {
	imprt = strings.TrimPrefix(imprt, "*")
	imprt = strings.TrimPrefix(imprt, "[]")

	for _, df := range data.ImportPaths {
		if strings.HasSuffix(df, "/"+imprt) {
			imprt = df

			break
		}
	}

	if slices.Contains(data.Imports, imprt) || strings.HasPrefix(imprt, "internal/") {
		return
	}

	data.Imports = append(data.Imports, imprt)
}

func (data *Data) scan(pkg *ast.Package, kind ast.ObjKind) error {
	for _, file := range pkg.Files {
		for _, i := range file.Imports {
			if i.Path.Value == "" {
				continue
			}

			name := i.Path.Value[1 : len(i.Path.Value)-1]

			data.ImportPaths = append(data.ImportPaths, name)
		}

		for name, object := range file.Scope.Objects {
			if object.Kind == kind && ast.IsExported(name) && !data.containesFunc(name) {
				fd := object.Decl.(*ast.FuncDecl)

				f, err := data.formatFuncDecl(fd)
				if common.Error(err) {
					return err
				}

				if f.Name == "" {
					continue
				}

				data.Funcs = append(data.Funcs, f)
			}
		}
	}

	sort.Strings(data.Imports)

	sort.Slice(data.Funcs, func(i, j int) bool {
		return data.Funcs[i].Name < data.Funcs[j].Name
	})

	return nil
}

func (data *Data) containesFunc(name string) bool {
	for _, f := range data.Funcs {
		if f.Name == name {
			return true
		}
	}

	return false
}

func findPackagePath() (string, string, error) {
	fi, err := os.Stat(*gomodFile)
	if common.Error(err) {
		return "", "", err
	}

	if fi.IsDir() {
		*gomodFile = filepath.Join(*gomodFile, "go.mod")
	}

	ba, err := os.ReadFile(*gomodFile)
	if common.Error(err) {
		return "", "", err
	}

	gomod, err := modfile.Parse(*gomodFile, ba, nil)
	if common.Error(err) {
		return "", "", err
	}

	for _, r := range gomod.Replace {
		if strings.HasPrefix(r.Old.String(), *pkgName) {
			return filepath.Join(filepath.Dir(*gomodFile), r.New.String()), filepath.Join(filepath.Dir(*gomodFile), r.New.Path), nil
		}
	}

	cmd := exec.Command("go", "env", "GOMODCACHE")
	stdout, err := cmd.Output()
	if common.Error(err) {
		return "", "", err
	}

	gomodcache := strings.TrimSpace(string(stdout))

	for _, r := range gomod.Require {
		if strings.HasPrefix(r.Mod.String(), *pkgName) {
			return filepath.Join(string(gomodcache), r.Mod.String()), filepath.Join(string(gomodcache), r.Mod.Path), nil
		}
	}

	return "", "", fmt.Errorf("unknown package name: %s", *pkgName)
}

func getPackageName() string {
	s := *pkgName
	s = strings.ToLower(strings.ReplaceAll(s, "/", "_"))
	s = strings.ToLower(strings.ReplaceAll(s, ".", "_"))

	return *prefix + s
}

func run() error {
	*pkgName = strings.ReplaceAll(*pkgName, "\\", "/")

	pathVersion, path, err := findPackagePath()
	if common.Error(err) {
		return err
	}

	fi, err := os.Stat(pathVersion)
	if common.Error(err) {
		return err
	}

	if !fi.IsDir() {
		return fmt.Errorf("not a directory: %s", pathVersion)
	}

	outputPkg := getPackageName()

	inputPkg := filepath.Base(path)

	data := Data{
		InputPkg:     inputPkg,
		OutputPkg:    outputPkg,
		StructName:   upper1st(outputPkg),
		JsStructName: lower1st(outputPkg),
		ImportPaths:  []string{*pkgName},
		Imports:      nil,
		Funcs:        nil,
	}

	astFiles, err := parser.ParseDir(token.NewFileSet(), pathVersion, filter, 0)
	if common.Error(err) {
		return err
	}

	data.addImport("github.com/dop251/goja")
	data.addImport(*pkgName)

	for _, astFile := range astFiles {
		err := data.scan(astFile, ast.Fun)
		if common.Error(err) {
			return err
		}
	}

	tmpl, err := template.New(*tmpl).ParseFiles(*tmpl)
	if common.Error(err) {
		return err
	}

	var buffer bytes.Buffer

	err = tmpl.Execute(&buffer, data)
	if common.Error(err) {
		return err
	}

	filename, err := filepath.Abs(filepath.Join(*output, outputPkg, strings.ToLower(outputPkg)+".go"))
	if common.Error(err) {
		return err
	}

	fmt.Printf("%s\n", filename)

	err = os.MkdirAll(filepath.Dir(filename), os.ModePerm)
	if common.Error(err) {
		return err
	}

	err = os.WriteFile(filename, buffer.Bytes(), os.ModePerm)
	if common.Error(err) {
		return err
	}

	return nil
}

func main() {
	common.Run([]string{"g", "n"})
}
