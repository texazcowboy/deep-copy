package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"go/types"
	"io"
	"log"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

var (
	pointerReceiverF = flag.Bool("pointer-receiver", false, "the generated receiver type")

	typesF  typesVal
	skipsF  skipsVal
	outputF outputVal
)

type typesVal []string

func (f *typesVal) String() string {
	return strings.Join(*f, ",")
}

func (f *typesVal) Set(v string) error {
	*f = append(*f, v)
	return nil
}

type skipsVal []map[string]struct{}

func (f *skipsVal) String() string {
	parts := make([]string, 0, len(*f))
	for _, m := range *f {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		parts = append(parts, strings.Join(keys, ","))
	}

	return strings.Join(parts, ",")
}

func (f *skipsVal) Set(v string) error {
	parts := strings.Split(v, ",")
	set := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		set[p] = struct{}{}
	}

	*f = append(*f, set)

	return nil
}

type outputVal struct {
	io.WriteCloser
	name string
}

func (f *outputVal) String() string {
	return f.name
}

func (f *outputVal) Set(v string) error {
	if v == "-" || v == "" {
		f.WriteCloser = os.Stdout
		f.name = "stdout"
		return nil
	}

	file, err := os.Create(v)
	if err != nil {
		return fmt.Errorf("opening file: %v", v)
	}

	f.name = v
	f.WriteCloser = file

	return nil
}

func init() {
	flag.Var(&typesF, "type", "the concrete type. Multiple flags can be specified")
	flag.Var(&skipsF, "skip", "comma-separated field/slice/map selectors to shallow copy. Multiple flags can be specified")
	flag.Var(&outputF, "o", "the output file to write to. Defaults to STDOUT")
}

func main() {
	flag.Parse()

	if len(typesF) == 0 || typesF[0] == "" {
		log.Fatalln("no type given")
	}

	if flag.NArg() != 1 {
		log.Fatalln("No package path given")
	}

	b, err := run(flag.Args()[0], typesF, skipsF, *pointerReceiverF)
	if err != nil {
		log.Fatalln("Error generating deep copy method:", err)
	}

	if outputF.WriteCloser == nil {
		if err := outputF.Set("-"); err != nil {
			log.Fatalln("Error initializing output file:", err)
		}
	}
	if _, err := outputF.Write(b); err != nil {
		log.Fatalln("Error writing result to file:", err)
	}
	outputF.Close()
}

func run(path string, types typesVal, skips skipsVal, pointer bool) ([]byte, error) {
	packages, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("loading package: %v", err)
	}
	if len(packages) == 0 {
		return nil, errors.New("no package found")
	}

	imports := map[string]string{}
	fns := [][]byte{}

	for i, kind := range types {
		var s map[string]struct{}
		if i < len(skips) {
			s = skips[i]
		}

		fn, err := generateFunc(packages[0], kind, imports, s, pointer)
		if err != nil {
			return nil, fmt.Errorf("generating method: %v", err)
		}

		fns = append(fns, fn)
	}

	b, err := generateFile(packages[0], imports, fns)
	if err != nil {
		return nil, fmt.Errorf("generating file content: %v", err)
	}

	return b, nil
}

func load(patterns string) ([]*packages.Package, error) {
	return packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
	}, patterns)
}

func generateFunc(p *packages.Package, kind string, imports map[string]string, skips map[string]struct{}, pointer bool) ([]byte, error) {
	var buf bytes.Buffer

	var ptr string
	if pointer {
		ptr = "*"
	}
	fmt.Fprintf(&buf, `// DeepCopy generates a deep copy of %s%s
func (o %s%s) DeepCopy() %s%s {
	var cp %s
`, ptr, kind, ptr, kind, ptr, kind, kind)

	name := p.Name
	obj, err := locateType(name, kind, p)
	if err != nil {
		return nil, err
	}

	sink := "o"
	fmt.Fprintf(&buf, "cp = %s%s\n", ptr, sink)
	walkType(sink, "cp", name, obj, &buf, imports, skips, true)

	if pointer {
		buf.WriteString("return &cp\n}")
	} else {
		buf.WriteString("return cp\n}")
	}

	return buf.Bytes(), nil
}

func generateFile(p *packages.Package, imports map[string]string, fn [][]byte) ([]byte, error) {
	var file bytes.Buffer

	fmt.Fprintf(&file, "// generated by %s; DO NOT EDIT.\n\npackage %s\n\n", strings.Join(os.Args, " "), p.Name)

	if len(imports) > 0 {
		file.WriteString("import (\n")
		for name, path := range imports {
			if strings.HasSuffix(path, name) {
				fmt.Fprintf(&file, "%q\n", path)
			} else {
				fmt.Fprintf(&file, "%s %q\n", name, path)
			}
		}
		file.WriteString(")\n")
	}

	for _, fn := range fn {
		file.Write(fn)
		file.WriteString("\n\n")
	}

	b, err := format.Source(file.Bytes())
	if err != nil {
		return nil, fmt.Errorf("error formatting source: %w\nsource:\n%s", err, file.String())
	}

	return b, nil
}

type object interface {
	types.Type
	Obj() *types.TypeName
}

type pointer interface {
	Elem() types.Type
}

type methoder interface {
	Method(i int) *types.Func
	NumMethods() int
}

func locateType(x, sel string, p *packages.Package) (object, error) {
	for _, t := range p.TypesInfo.Defs {
		if t == nil {
			continue
		}
		m := exprFilter(t.Type(), sel, x)
		if m == nil {
			continue
		}

		return m, nil
	}

	return nil, errors.New("type not found")
}

func reducePointer(typ types.Type) (types.Type, bool) {
	if pointer, ok := typ.(pointer); ok {
		return pointer.Elem(), true
	}
	return typ, false
}

func objFromType(typ types.Type) object {
	typ, _ = reducePointer(typ)

	m, ok := typ.(object)
	if !ok {
		return nil
	}

	return m
}

func exprFilter(t types.Type, sel string, x string) object {
	m := objFromType(t)
	if m == nil {
		return nil
	}

	obj := m.Obj()
	if obj.Pkg() == nil || x != obj.Pkg().Name() || sel != obj.Name() {
		return nil
	}

	return m
}

func walkType(source, sink, x string, m types.Type, w io.Writer, imports map[string]string, skips map[string]struct{}, initial bool) {
	if m == nil {
		return
	}

	var needExported bool
	switch v := m.(type) {
	case *types.Named:
		if v.Obj().Pkg() != nil && v.Obj().Pkg().Name() != x {
			needExported = true
		}
	}

	if v, ok := m.(methoder); ok && !initial && reuseDeepCopy(source, sink, v, false, w) {
		return
	}

	under := m.Underlying()
	switch v := under.(type) {
	case *types.Struct:
		for i := 0; i < v.NumFields(); i++ {
			field := v.Field(i)
			if needExported && !field.Exported() {
				continue
			}
			fname := field.Name()
			sel := sink + "." + fname
			sel = sel[strings.Index(sel, ".")+1:]
			if _, ok := skips[sel]; ok {
				continue
			}
			walkType(source+"."+fname, sink+"."+fname, x, field.Type(), w, imports, skips, false)
		}
	case *types.Slice:
		kind, _ := getElemType(v.Elem(), x, imports, false)

		sel := sink + "[i]"
		if initial {
			sel = "[i]"
		}

		var skipSlice bool
		sel = sel[strings.Index(sel, ".")+1:]
		if _, ok := skips[sel]; ok {
			skipSlice = true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make([]%s, len(%s))
`, source, sink, kind, source)

		var b bytes.Buffer

		if !skipSlice {
			walkType(source+"[i]", sink+"[i]", x, v.Elem(), &b, imports, skips, false)
		}

		if b.Len() == 0 {
			fmt.Fprintf(w, `copy(%s, %s)
`, sink, source)
		} else {
			fmt.Fprintf(w, `    for i := range %s {
`, source)

			b.WriteTo(w)

			fmt.Fprintf(w, "}\n")
		}

		fmt.Fprintf(w, "}\n")
	case *types.Pointer:
		kind, _ := getElemType(v.Elem(), x, imports, true)

		fmt.Fprintf(w, "if %s != nil {\n", source)

		if e, ok := v.Elem().(methoder); !ok || initial || !reuseDeepCopy(source, sink, e, true, w) {

			fmt.Fprintf(w, `%s = new(%s)
	*%s = *%s
`, sink, kind, sink, source)

			walkType(source, sink, x, v.Elem(), w, imports, skips, false)
		}

		fmt.Fprintf(w, "}\n")
	case *types.Chan:
		kind, _ := getElemType(v.Elem(), x, imports, false)

		fmt.Fprintf(w, `if %s != nil {
	%s = make(chan %s, cap(%s))
}
`, source, sink, kind, source)
	case *types.Map:
		kkind, kbasic := getElemType(v.Key(), x, imports, false)
		vkind, vbasic := getElemType(v.Elem(), x, imports, false)

		sel := sink + "[k]"
		if initial {
			sel = "[k]"
		}
		sel = sel[strings.Index(sel, ".")+1:]
		if _, ok := skips[sel]; ok {
			kbasic, vbasic = true, true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make(map[%s]%s, len(%s))
	for k, v := range %s {
`, source, sink, kkind, vkind, source, source)

		ksink, vsink := "k", "v"
		if !kbasic {
			ksink = "cpk"
			fmt.Fprintf(w, "var %s %s\n", ksink, kkind)
			walkType("k", ksink, x, v.Key(), w, imports, skips, false)
		}
		if !vbasic {
			vsink = "cpv"
			fmt.Fprintf(w, "var %s %s\n", vsink, vkind)
			walkType("v", vsink, x, v.Elem(), w, imports, skips, false)
		}

		fmt.Fprintf(w, "%s[%s] = %s", sink, ksink, vsink)

		fmt.Fprintf(w, "}\n}\n")
	}

}

func getElemType(t types.Type, x string, imports map[string]string, rawkind bool) (string, bool) {
	obj := objFromType(t)
	var name, kind string
	if obj != nil {
		pkg := obj.Obj().Pkg()
		if pkg != nil {
			name = pkg.Name()
			if name != x {
				if path, ok := imports[name]; ok && path != pkg.Path() {
					name = strings.ReplaceAll(pkg.Path(), "/", "_")
				}
				imports[name] = pkg.Path()
				kind += name + "."
			}
		}
		kind += obj.Obj().Name()
	} else {
		kind += t.String()
	}

	var pointer, noncopy bool
	switch t.(type) {
	case *types.Pointer:
		pointer = true
	case *types.Basic, *types.Interface:
		noncopy = true
	}

	if !rawkind && pointer && kind[0] != '*' {
		kind = "*" + kind
	}

	return kind, noncopy
}

func reuseDeepCopy(source, sink string, v methoder, pointer bool, w io.Writer) bool {
	for i := 0; i < v.NumMethods(); i++ {
		m := v.Method(i)
		if m.Name() != "DeepCopy" {
			continue
		}

		sig, ok := m.Type().(*types.Signature)
		if !ok {
			continue
		}

		if sig.Params().Len() != 0 || sig.Results().Len() != 1 {
			continue
		}

		ret := sig.Results().At(0)
		retType, retPointer := reducePointer(ret.Type())
		sigType, _ := reducePointer(sig.Recv().Type())

		if !types.Identical(retType, sigType) {
			return false
		}

		if pointer == retPointer {
			fmt.Fprintf(w, "%s = %s.DeepCopy()\n", sink, source)
		} else if pointer {
			fmt.Fprintf(w, `retV := %s.DeepCopy()
	%s = &retV
`, source, sink)
		} else {
			fmt.Fprintf(w, `{
	retV := %s.DeepCopy()
	%s = *retV
}
`, source, sink)
		}

		return true
	}

	return false
}
