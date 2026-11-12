// Package archtest proves the agent cannot start a process (design decision 13).
//
// It has two parts:
//
//  1. a source scan of the module's own Go files, which fails on an os/exec
//     import, on os.StartProcess, syscall.Exec or syscall.ForkExec, and on
//     InsecureSkipVerify anywhere;
//  2. a binary symbol check, which builds the agent for every release target
//     and fails if the linker kept any function that starts a process. The
//     linker drops what nothing can reach, so an absent symbol means no code
//     in the binary, dependencies included, can reach it.
//
// This package is the only one allowed to import os/exec, because it runs the
// go tool. It is excluded from its own scan and is never linked into the agent.
package archtest

import (
	"bufio"
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// forbiddenCalls are package-qualified identifiers the source must not use.
var forbiddenCalls = map[string][]string{
	"os":      {"StartProcess"},
	"syscall": {"Exec", "ForkExec", "StartProcess"},
}

// forbiddenImports must not be imported by any file in the module.
var forbiddenImports = []string{"os/exec"}

// forbiddenIdents must not appear as an identifier anywhere.
var forbiddenIdents = []string{"InsecureSkipVerify"}

// forbiddenSymbols must not be in a linked agent binary.
var forbiddenSymbols = []string{
	"os.StartProcess",
	"os/exec.(*Cmd).Start",
	"syscall.forkExec",
	"syscall.Exec",
}

// targets are the release targets from design decision 16.
var targets = []struct{ goos, goarch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

func TestSourceHasNoProcessOrInsecureTLS(t *testing.T) {
	root := moduleRoot(t)
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	files := goFiles(t, root, self)
	if len(files) == 0 {
		t.Fatal("found no Go files to scan; the module root is wrong")
	}

	var violations []string
	for _, path := range files {
		violations = append(violations, scanFile(t, root, path)...)
	}

	if len(violations) > 0 {
		t.Errorf("the agent must never start a process or skip TLS verification (design decision 13):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

func TestBinaryHasNoProcessSymbols(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent for four targets; skipped under -short")
	}

	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not on PATH")
	}
	root := moduleRoot(t)

	for _, target := range targets {
		t.Run(target.goos+"_"+target.goarch, func(t *testing.T) {
			t.Parallel()

			env := append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.goos, "GOARCH="+target.goarch)
			binary := filepath.Join(t.TempDir(), "autoheal-agent")

			// No -s: the symbol table is what this test reads.
			build := exec.Command(goTool, "build", "-trimpath", "-o", binary, "./cmd/autoheal-agent")
			build.Dir = root
			build.Env = env
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build for %s/%s failed: %v\n%s", target.goos, target.goarch, err, output)
			}

			nm := exec.Command(goTool, "tool", "nm", binary)
			nm.Dir = root
			nm.Env = env
			output, err := nm.Output()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					t.Fatalf("go tool nm failed: %v\n%s", err, exitErr.Stderr)
				}
				t.Fatalf("go tool nm failed: %v", err)
			}

			symbols := symbolNames(output)
			if !symbols["main.main"] {
				t.Fatalf("main.main is missing from the symbol table of the %s/%s build; the check would prove nothing", target.goos, target.goarch)
			}

			var found []string
			for _, symbol := range forbiddenSymbols {
				if symbols[symbol] {
					found = append(found, symbol)
				}
			}
			if len(found) > 0 {
				t.Errorf("the %s/%s binary contains %s, so something in it can start a process.\n"+
					"Packages in its build that import os/exec:\n  %s\n"+
					"Find the path with: GOOS=%s GOARCH=%s CGO_ENABLED=0 go build -ldflags=-dumpdep ./cmd/autoheal-agent 2>&1 | grep -B1 -E '%s'\n"+
					"Do not relax this check: replace the collector that reaches it (design decision 13).",
					target.goos, target.goarch, strings.Join(found, ", "),
					strings.Join(execImporters(t, goTool, root, env), "\n  "),
					target.goos, target.goarch, strings.Join(quoteMeta(found), "|"))
			}
		})
	}
}

// moduleRoot walks up from the test's folder to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the archtest folder")
		}
		dir = parent
	}
}

// goFiles lists the module's .go files, skipping this package, testdata,
// vendor, hidden folders, dist and nested modules.
func goFiles(t *testing.T, root, self string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path == self {
				return filepath.SkipDir
			}
			if path != root {
				if name == "testdata" || name == "vendor" || name == "dist" ||
					strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// scanFile parses one file, ignoring build constraints so every platform's
// files are checked, and returns its violations.
func scanFile(t *testing.T, root, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}

	rel, _ := filepath.Rel(root, path)
	position := func(node ast.Node) string {
		p := fset.Position(node.Pos())
		return rel + ":" + strconv.Itoa(p.Line)
	}

	var violations []string

	// Local names of the imported packages that have forbidden members.
	localNames := map[string]string{}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		for _, forbidden := range forbiddenImports {
			if importPath == forbidden {
				violations = append(violations, position(spec)+": imports "+forbidden)
			}
		}
		if _, ok := forbiddenCalls[importPath]; ok {
			name := filepath.Base(importPath)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			localNames[name] = importPath
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := node.X.(*ast.Ident); ok {
				if importPath, ok := localNames[pkg.Name]; ok {
					for _, name := range forbiddenCalls[importPath] {
						if node.Sel.Name == name {
							violations = append(violations, position(node)+": uses "+importPath+"."+name)
						}
					}
				}
			}
		case *ast.Ident:
			for _, name := range forbiddenIdents {
				if node.Name == name {
					violations = append(violations, position(node)+": uses "+name)
				}
			}
		}
		return true
	})

	return violations
}

// symbolNames returns the set of symbol names in `go tool nm` output, whose
// lines are "[address] type name".
func symbolNames(output []byte) map[string]bool {
	names := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			names[fields[len(fields)-1]] = true
		}
	}
	return names
}

// execImporters lists the packages in the agent's build that import os/exec,
// for the failure message.
func execImporters(t *testing.T, goTool, root string, env []string) []string {
	list := exec.Command(goTool, "list", "-deps", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, "./cmd/autoheal-agent")
	list.Dir = root
	list.Env = env
	output, err := list.Output()
	if err != nil {
		return []string{"(go list failed: " + err.Error() + ")"}
	}
	var importers []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		for _, imported := range fields[min(1, len(fields)):] {
			if imported == "os/exec" {
				importers = append(importers, fields[0]+" imports "+imported)
			}
		}
	}
	if len(importers) == 0 {
		return []string{"(none; the symbol comes in through the standard library)"}
	}
	return importers
}

func quoteMeta(symbols []string) []string {
	quoted := make([]string, len(symbols))
	for i, symbol := range symbols {
		quoted[i] = strings.NewReplacer("(", `\(`, ")", `\)`, "*", `\*`, ".", `\.`).Replace(symbol)
	}
	return quoted
}
