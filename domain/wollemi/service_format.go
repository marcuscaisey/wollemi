package wollemi

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/tcncloud/wollemi/ports/filesystem"
	"github.com/tcncloud/wollemi/ports/please"
)

func (this *Service) Format(paths []string) error {
	return this.GoFormat(false, paths)
}

func (this *Service) GoFormat(rewrite bool, paths []string) error {
	for i, path := range paths {
		paths[i] = strings.TrimSuffix(path, "/")
	}

	if len(paths) == 0 {
		paths = []string{"..."}
	}

	this.log.WithField("rewrite", rewrite).
		WithField("go_package", this.gopkg).
		WithField("go_source", this.gosrc).
		Debug("run")

	collect := make(chan *Directory, 1000)
	walk := make(chan *Directory, 1000)
	parse := make(chan *Directory, 1000)

	for i := 0; i < 1; i++ {
		go func() {
			var buf bytes.Buffer

			for dir := range parse {
				dir.InRunPath = inRunPath(dir.Path, paths...)
				dir.Rewrite = rewrite

				collect <- this.Parse(&buf, dir)
			}
		}()
	}

	directories := make(map[string]*Directory)
	external := make(map[string][]string)
	internal := make(map[string]string)
	genfiles := make(map[string]string)

	var collector sync.WaitGroup

	collector.Add(1)
	go func() {
		defer collector.Done()
		defer close(parse)

		delegated := make(map[string]struct{})
		parsing := 0

		for walk != nil || parsing > 0 {
			select {
			case dir, ok := <-walk:
				if !ok {
					walk = nil
				} else {
					nonBlockingSend(parse, dir)
					parsing++
				}
			case dir := <-collect:
				parsing--

				if !dir.Ok {
					continue
				}

				directories[dir.Path] = dir

				if dir.Gopkg != nil {
					for _, group := range [][]string{
						dir.Gopkg.Imports,
						dir.Gopkg.TestImports,
						dir.Gopkg.XTestImports,
					} {
					Group:
						for _, godep := range group {
							path := godep

							if strings.HasPrefix(path, this.gopkg) {
								path = strings.TrimPrefix(path, this.gopkg+"/")
							} else {
								path = filepath.Join("third_party/go", path)
							}

							if _, ok := external[godep]; ok {
								continue
							}

							if inRunPath(path, paths...) {
								continue
							}

							chunks := strings.Split(path, "/")

							for i := len(chunks); i > 0; i-- {
								path := filepath.Join(chunks[0:i]...)

								if _, ok := delegated[path]; ok {
									continue Group
								}

								if _, ok := directories[path]; ok {
									continue Group
								}

								buildPath := filepath.Join(path, "BUILD.plz")

								_, err := this.filesystem.Stat(buildPath)
								if os.IsNotExist(err) {
									continue
								}

								if err != nil {
									this.log.WithError(err).
										WithField("path", path).
										Warn("could not stat build file")

									continue
								}

								delegated[path] = struct{}{}
								parsing++

								nonBlockingSend(parse, &Directory{Rule: godep, Path: path, Ok: true})
								break
							}
						}
					}
				}

				dir.Build.GetRules(func(rule please.Rule) {
					switch rule.Kind() {
					case "go_copy", "go_mock", "go_library", "go_test", "grpc_library":
						name := rule.AttrString("name")

						target, path := dir.Path, dir.Path
						if filepath.Base(path) != name {
							path = filepath.Join(path, name)
							target += ":" + name
						}

						internal[path] = "//" + target

						if rule.Kind() == "go_copy" {
							genfiles[path+".cp.go"] = target
						}
					case "go_get", "go_get_with_sources":
						get := strings.TrimSuffix(rule.AttrString("get"), "/...")
						name := rule.AttrString("name")

						target := dir.Path
						if filepath.Base(dir.Path) != name {
							target += ":" + name
						}

						if rule.Kind() == "go_get_with_sources" {
							get = rule.AttrStrings("outs")[0]
						}

						if get != "" && rule.AttrLiteral("binary") != "True" {
							external[get] = append(external[get], "//"+target)
						}
					}
				})
			}
		}
	}()

	if err := this.Walk(walk, paths...); err != nil {
		return fmt.Errorf("could not walk: %v", err)
	}

	collector.Wait()

	get := NewChanFunc(1, 0)
	gen := NewChanFunc(runtime.NumCPU()-1, 0)

	getTarget := (func() func(*filesystem.Config, string, bool) (string, string) {
		var inner func(*filesystem.Config, string, bool) (string, string)

		inner = func(config *filesystem.Config, path string, isFile bool) (string, string) {
			if target, ok := config.KnownDependency[path]; ok {
				return target, path
			}

			if isFile && filepath.Ext(path) == ".go" {
				if target, ok := genfiles[path]; ok {
					return target, path
				} else {
					return "", path
				}
			}

			if strings.HasPrefix(path, this.gopkg+"/") {
				relpath := strings.TrimPrefix(path, this.gopkg+"/")

				if target, ok := internal[relpath]; ok {
					return target, path
				}

				return fmt.Sprintf("//%s", relpath), path
			}

			targets, ok := external[path]
			if ok {
				if len(targets) > 1 {
					this.log.WithField("choices", targets).
						WithField("godep", path).
						WithField("chose", targets[0]).
						Warn("ambiguous godep")
				}

				return targets[0], path
			}

			path = filepath.Dir(path)
			if path == "." {
				return "", path
			}

			return inner(config, path, isFile)
		}

		return func(config *filesystem.Config, path string, isFile bool) (string, string) {
			var target string

			get.RunBlock(func() {
				target, path = inner(config, path, isFile)
			})

			return target, path
		}
	}())

	for path, dir := range directories {
		if !dir.InRunPath {
			continue
		}

		log := this.log.WithField("path", path)

		path, dir := path, dir

		gen.Run(func() {
			config := this.filesystem.Config(path)

			rulesByKind := make(map[string][]please.Rule)

			dir.Build.GetRules(func(rule please.Rule) {
				kind := rule.Kind()

				switch kind {
				case "go_binary", "go_library", "go_test":
					rulesByKind[kind] = append(rulesByKind[kind], rule)
				}
			})

			if dir.Ok && dir.Rewrite && dir.Gopkg != nil {
				gopkg := dir.Gopkg

				if len(gopkg.GoFiles) > 0 {
					var kind string

					switch gopkg.Name {
					case "main":
						kind = "go_binary"
					default:
						kind = "go_library"
					}

					rules := rulesByKind[kind]
					if len(rules) == 0 {
						name := filepath.Base(dir.Path)
						rule := this.please.NewRule(kind, name)
						rulesByKind[kind] = []please.Rule{rule}
					}
				}

				if len(gopkg.TestGoFiles)+len(gopkg.XTestGoFiles) > 0 {
					kind := "go_test"
					rules := rulesByKind[kind]
					if len(rules) == 0 {
						rule := this.please.NewRule(kind, "test")
						rulesByKind[kind] = []please.Rule{rule}
					}
				}
			Rules:
				for _, kind := range []string{"go_binary", "go_library", "go_test"} {
					rules := rulesByKind[kind]
					if len(rules) == 0 {
						continue
					}

					if len(rules) > 1 {
						var ok bool

						switch kind {
						case "go_binary":
						case "go_library":
						case "go_test":
						default:
							ok = true
						}

						if !ok {
							names := make([]string, 0, len(rules))

							for _, rule := range rules {
								names = append(names, rule.Name())
							}

							log.WithField("kind", kind).
								WithField("names", names).
								WithField("go_rewrite", false).
								Warn("multiples of rule kind not supported")

							continue
						}
					}

					rule := rules[0]

					for _, comment := range rule.Comment().Before {
						token := strings.TrimSpace(comment.Token)

						if strings.EqualFold(token, "# wollemi:keep") {
							continue Rules
						}
					}

					var goFiles []string
					var imports []string
					var external bool
					var includePattern string
					var excludePattern string

					switch kind {
					case "go_binary", "go_library":
						goFiles = gopkg.GoFiles
						imports = gopkg.Imports
						includePattern = "*.go"
						excludePattern = "*_test.go"
					case "go_test":
						includePattern = "*_test.go"
						goFiles = gopkg.XTestGoFiles
						imports = gopkg.XTestImports
						external = len(goFiles) > 0

						if !external {
							includePattern = "*.go"
							goFiles = append(gopkg.GoFiles, gopkg.TestGoFiles...)
							imports = append(gopkg.Imports, gopkg.TestImports...)
						}
					}

					ruleName := rule.Name()

					if rule.AttrString("name") == "" {
						rule.SetAttr("name", please.String(ruleName))
					}

					var remove bool

					switch kind {
					case "go_binary", "go_library":
						remove = len(goFiles) == 0
					case "go_test":
						remove = len(gopkg.TestGoFiles)+len(gopkg.XTestGoFiles) == 0
					}

					if remove {
						log.WithField("build_rule", ruleName).
							WithField("reason", "no source files").
							Warn("removed")

						dir.Build.DelRule(ruleName)

						continue
					}

					log := log.WithField("build_rule", ruleName)

					n := len(goFiles)

					include := make([]string, 0, n)
					exclude := make([]string, 0, n)
					targets := make([]string, 0, n)

					if includePattern != "" {
						include = append(include, includePattern)
					}

					if excludePattern != "" {
						exclude = append(exclude, excludePattern)
					}

					var srcLen int

					for _, filename := range goFiles {
						relpath := filepath.Join(path, filename)

						log := log.WithField("file", filename)

						target, _ := getTarget(config, relpath, true)
						if target == "" {
							info, err := this.filesystem.Lstat(relpath)
							if err != nil {
								log.WithError(err).Warn("could not lstat")

								continue
							}

							if info.Mode()&os.ModeSymlink == 0 { // is not symlink
								srcLen++

								if includePattern != "" {
									ok, err := filepath.Match(includePattern, filename)
									if err != nil {
										log.WithError(err).
											WithField("pattern", includePattern).
											Warn("could not match include pattern")

										continue
									}

									if ok {
										continue
									}

									if excludePattern != "" {
										ok, err := filepath.Match(excludePattern, filename)
										if err != nil {
											log.WithError(err).
												WithField("pattern", excludePattern).
												Warn("could not match exclude pattern")

											continue
										}

										if ok {
											continue
										}
									}
								}

								include = append(include, filename)
							}
						} else {
							targetPath, name := split(target)
							if targetPath == path {
								target = ":" + name
							}

							exclude = append(exclude, filename)
							targets = append(targets, target)
						}
					}

					if srcLen == 0 {
						continue
					}

					srcs := please.Glob(include, exclude, targets...)
					deps := make([]string, 0, len(imports))
					dedup := make(map[string]struct{})
					unresolved := make([]string, 0, len(imports))

					for _, godep := range imports {
						target, _ := getTarget(config, godep, false)
						if target == "" {
							log.WithField("godep", godep).
								Error("could not resolve godep")

							unresolved = append(unresolved, godep)

							continue
						}

						targetPath, name := split(target)
						if targetPath == path {
							target = ":" + name
						}

						if _, ok := dedup[target]; !ok {
							dedup[target] = struct{}{}
							deps = append(deps, target)
						}
					}

					// skip rewrite because we have unresolved dependencies
					if len(unresolved) > 0 {
						continue
					}

					rule.SetAttr("srcs", srcs)

					switch rule.Kind() {
					case "go_test":
						if external {
							rule.SetAttr("external", &please.Ident{Name: "True"})
						} else {
							rule.DelAttr("external")
						}
					case "go_binary", "go_library":
						if rule.AttrStrings("visibility") == nil {
							visibility := config.DefaultVisibility

							if visibility == "" {
								visibility = "PUBLIC"

								for _, root := range paths {
									dir, name := split(root)
									if name == "..." && dir != "" {
										if path == dir || strings.HasPrefix(path, dir+"/") {
											visibility = fmt.Sprintf("//%s/...", dir)
											break
										}
									}
								}
							}

							rule.SetAttr("visibility", please.Strings(visibility))
						}
					}

					sort.Slice(deps, func(i, j int) bool {
						iPath, iName := split(deps[i])
						jPath, jName := split(deps[j])

						if iPath == jPath {
							return iName < jName
						}

						return iPath < jPath
					})

					rule.SetAttr("deps", please.Strings(deps...))

					// TODO: only if new rule?
					dir.Build.SetRule(rule)
				}
			}

			if err := this.please.Write(dir.Build); err != nil {
				log.WithError(err).Warn("could not write")
			}
		})
	}

	gen.Close()
	get.Close()

	return nil
}
