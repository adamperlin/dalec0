package dalec

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/gitutil"
)

type AsLLBState interface {
	AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter
	IsDir() bool
}

func mapGetter(getter LLBGetter, f StateFunc) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		st, err := getter(sOpt, opts...)
		if err != nil {
			return llb.Scratch(), err
		}

		return f(st), nil
	}
}

func defaultTargetPath(path string, forMount bool) string {
	if !forMount && path != "" {
		return path
	}

	return "/"
}

// given a source, returns the target path in the generated LLB
// that should be returned
func (src Source) TargetPath(forMount bool) (string, error) {
	switch {
	case src.HTTPS != nil,
		src.Build != nil,
		src.Git != nil:
		return defaultTargetPath(src.Path, forMount), nil
	case src.Context != nil:
		path := src.Path
		if path != src.Context.Path && src.Context.Path != "" {
			// use nested context path if root source path is empty
			path = src.Context.Path
		}
		return defaultTargetPath(path, forMount), nil
	case src.DockerImage != nil:
		// path already handled for docker images
		return "/", nil
	}

	return "", fmt.Errorf("no variant defined")
}

// a StateFunc is any transformation which maps
// llb state -> llb state
type StateFunc func(llb.State) llb.State

func GetSource(src Source, name string, forMount bool, opts ...llb.ConstraintsOpt) (LLBGetter, bool, error) {
	if err := src.validate(); err != nil {
		return nil, false, err
	}

	filterWith := func(fopts filterOpts) StateFunc {
		return func(st llb.State) llb.State {
			return filterState(st, fopts, opts...)
		}
	}

	targetPath, err := src.TargetPath(forMount)
	if err != nil {
		return nil, false, err
	}

	// load the source
	switch {
	case src.HTTPS != nil:
		llbGetter := src.HTTPS.AsState(name, src.Path, forMount, src.Includes, src.Excludes)
		llbGetter = mapGetter(llbGetter, filterWith(filterOpts{
			srcPath:  targetPath,
			includes: src.Includes,
			excludes: src.Excludes,
		}))
		return llbGetter, src.HTTPS.IsDir(), nil
	case src.Git != nil:
		llbGetter := src.Git.AsState(name, src.Path, forMount, src.Includes, src.Excludes)
		llbGetter = mapGetter(llbGetter, filterWith(filterOpts{
			srcPath:  targetPath,
			includes: src.Includes,
			excludes: src.Excludes,
		}))

		return llbGetter, src.Git.IsDir(), nil
	case src.Context != nil:
		llbGetter := src.Context.AsState(name, src.Path, forMount, src.Includes, src.Excludes)
		llbGetter = mapGetter(llbGetter, filterWith(filterOpts{
			srcPath: targetPath,
			// no includes or excludes are part of the filter options
			// for SourceContext since context sources already handle
			// includes and excludes
			includes: []string{},
			excludes: []string{},
		}))

		return llbGetter, src.Context.IsDir(), nil
	case src.DockerImage != nil:
		// include-exclude-handled = false
		llbGetter := src.DockerImage.AsState(name, src.Path, forMount, src.Includes, src.Excludes)
		// clarify this case:
		if src.DockerImage.Cmd == nil {
			llbGetter = mapGetter(llbGetter, filterWith(filterOpts{
				srcPath:  targetPath,
				includes: src.Includes,
				excludes: src.Excludes,
			}))
		}

		return llbGetter, src.DockerImage.IsDir(), nil
	case src.Build != nil:
		llbGetter := src.Build.AsState(name, src.Path, forMount, src.Includes, src.Excludes)
		llbGetter = mapGetter(llbGetter, filterWith(filterOpts{
			srcPath:  targetPath,
			includes: src.Includes,
			excludes: src.Excludes,
		}))

		return llbGetter, src.Build.IsDir(), nil
	}

	return nil, false, fmt.Errorf("no source variant defined")
}

func (src *SourceContext) AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		st, err := sOpt.GetContext(dockerui.DefaultLocalNameContext, localIncludeExcludeMerge(includes, excludes))
		if err != nil {
			return llb.Scratch(), err
		}

		return *st, nil
	}
}

func (src *SourceContext) IsDir() bool {
	return true
}

func (src *SourceGit) AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		// TODO: Pass git secrets
		ref, err := gitutil.ParseGitRef(src.URL)
		if err != nil {
			return llb.Scratch(), fmt.Errorf("could not parse git ref: %w", err)
		}

		var gOpts []llb.GitOption
		if src.KeepGitDir {
			gOpts = append(gOpts, llb.KeepGitDir())
		}
		gOpts = append(gOpts, withConstraints(opts))
		return llb.Git(ref.Remote, src.Commit, gOpts...), nil
	}
}

func (src *SourceGit) IsDir() bool {
	return true
}

func (src *SourceDockerImage) AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		st := llb.Image(src.Ref, llb.WithMetaResolver(sOpt.Resolver), withConstraints(opts))

		if src.Cmd == nil {
			return st, nil
		}

		eSt, err := generateSourceFromImage(name, st, src.Cmd, sOpt, opts...)
		if err != nil {
			return llb.Scratch(), err
		}
		if path != "" {
			return eSt.AddMount(path, llb.Scratch()), nil
		}
		return eSt.Root(), nil
	}
}

func (src *SourceDockerImage) IsDir() bool {
	return true
}

func (src *SourceBuild) AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		subSourceGetter, _, err := GetSource(src.Source, name, forMount, opts...)
		if err != nil {
			return llb.Scratch(), err
		}

		st, err := subSourceGetter(sOpt, opts...)
		if err != nil {
			return llb.Scratch(), err
		}

		return sOpt.Forward(st, src)
	}
}

func (src *SourceBuild) IsDir() bool {
	return true
}

func (src *SourceHTTPS) AsState(name string, path string, forMount bool, includes, excludes []string) LLBGetter {
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
		httpOpts := []llb.HTTPOption{withConstraints(opts)}
		httpOpts = append(httpOpts, llb.Filename(name))
		return llb.HTTP(src.URL, httpOpts...), nil
	}
}

func (src *SourceHTTPS) IsDir() bool {
	return false
}

type LLBGetter func(sOpts SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error)

type ForwarderFunc func(llb.State, *SourceBuild) (llb.State, error)

type SourceOpts struct {
	Resolver   llb.ImageMetaResolver
	Forward    ForwarderFunc
	GetContext func(string, ...llb.LocalOption) (*llb.State, error)
}

func shArgs(cmd string) llb.RunOption {
	return llb.Args([]string{"sh", "-c", cmd})
}

// must not be called with a nil cmd pointer
func generateSourceFromImage(name string, st llb.State, cmd *Command, sOpts SourceOpts, opts ...llb.ConstraintsOpt) (llb.ExecState, error) {
	var zero llb.ExecState
	if len(cmd.Steps) == 0 {
		return zero, fmt.Errorf("no steps defined for image source")
	}
	for k, v := range cmd.Env {
		st = st.AddEnv(k, v)
	}
	if cmd.Dir != "" {
		st = st.Dir(cmd.Dir)
	}

	baseRunOpts := []llb.RunOption{CacheDirsToRunOpt(cmd.CacheDirs, "", "")}

	for _, src := range cmd.Mounts {
		srcSt, err := source2LLBGetter(src.Spec, name, true)(sOpts, opts...)
		if err != nil {
			return zero, err
		}
		var mountOpt []llb.MountOption
		if src.Spec.Path != "" && len(src.Spec.Includes) == 0 && len(src.Spec.Excludes) == 0 {
			mountOpt = append(mountOpt, llb.SourcePath(src.Spec.Path))
		}
		baseRunOpts = append(baseRunOpts, llb.AddMount(src.Dest, srcSt, mountOpt...))
	}

	var cmdSt llb.ExecState
	for _, step := range cmd.Steps {
		rOpts := []llb.RunOption{llb.Args([]string{
			"/bin/sh", "-c", step.Command,
		})}

		rOpts = append(rOpts, baseRunOpts...)

		for k, v := range step.Env {
			rOpts = append(rOpts, llb.AddEnv(k, v))
		}

		rOpts = append(rOpts, withConstraints(opts))
		cmdSt = st.Run(rOpts...)
	}

	return cmdSt, nil
}

func Source2LLBGetter(_ *Spec, src Source, name string) LLBGetter {
	return source2LLBGetter(src, name, false)
}

type filterOpts struct {
	srcPath  string
	includes []string
	excludes []string
}

func filterState(st llb.State, fopts filterOpts, opts ...llb.ConstraintsOpt) llb.State {
	// if we have no includes and no excludes, and no non-root source path,
	// then this is a no-op
	if len(fopts.includes) == 0 && len(fopts.excludes) == 0 && fopts.srcPath == "/" {
		return st
	}

	filtered := llb.Scratch().File(
		llb.Copy(
			st,
			fopts.srcPath,
			"/",
			WithIncludes(fopts.includes),
			WithExcludes(fopts.excludes),
			WithDirContentsOnly(),
		),
		withConstraints(opts),
	)

	return filtered
}

func source2LLBGetter(src Source, name string, forMount bool) LLBGetter {
	//return GetSource(src, name, forMount)
	return func(sOpt SourceOpts, opts ...llb.ConstraintsOpt) (ret llb.State, retErr error) {
		getter, _, err := GetSource(src, name, forMount, opts...)
		if err != nil {
			return llb.Scratch(), err
		}
		return getter(sOpt, opts...)
	}
}

func sharingMode(mode string) (llb.CacheMountSharingMode, error) {
	switch mode {
	case "shared", "":
		return llb.CacheMountShared, nil
	case "private":
		return llb.CacheMountPrivate, nil
	case "locked":
		return llb.CacheMountLocked, nil
	default:
		return 0, fmt.Errorf("invalid sharing mode: %s", mode)
	}
}

func WithCreateDestPath() llb.CopyOption {
	return copyOptionFunc(func(i *llb.CopyInfo) {
		i.CreateDestPath = true
	})
}

func SourceIsDir(src Source) (bool, error) {
	switch {
	case src.DockerImage != nil,
		src.Git != nil,
		src.Build != nil,
		src.Context != nil:
		return true, nil
	case src.HTTPS != nil:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported source type")
	}
}

// Doc returns the details of how the source was created.
// This should be included, where applicable, in build in build specs (such as RPM spec files)
// so that others can reproduce the build.
func (s Source) Doc() (io.Reader, error) {
	b := bytes.NewBuffer(nil)
	switch {
	case s.Context != nil:
		fmt.Fprintln(b, "Generated from a local docker build context and is unreproducible.")
	case s.Build != nil:
		fmt.Fprintln(b, "Generated from a docker build:")
		fmt.Fprintln(b, "	Docker Build Target:", s.Build.Target)
		sub, err := s.Build.Source.Doc()
		if err != nil {
			return nil, err
		}

		scanner := bufio.NewScanner(sub)
		for scanner.Scan() {
			fmt.Fprintf(b, "			%s\n", scanner.Text())
		}
		if scanner.Err() != nil {
			return nil, scanner.Err()
		}

		if len(s.Build.Args) > 0 {
			sorted := SortMapKeys(s.Build.Args)
			fmt.Fprintln(b, "	Build Args:")
			for _, k := range sorted {
				fmt.Fprintf(b, "		%s=%s\n", k, s.Build.Args[k])
			}
		}

		switch {
		case s.Build.Inline != "":
			fmt.Fprintln(b, "	Dockerfile:")

			scanner := bufio.NewScanner(strings.NewReader(s.Build.Inline))
			for scanner.Scan() {
				fmt.Fprintf(b, "		%s\n", scanner.Text())
			}
			if scanner.Err() != nil {
				return nil, scanner.Err()
			}
		default:
			p := "Dockerfile"
			if s.Build.DockerFile != "" {
				p = s.Build.DockerFile
			}
			fmt.Fprintln(b, "	Dockerfile path in context:", p)
		}
	case s.HTTPS != nil:
		fmt.Fprintln(b, "Generated from a http(s) source:")
		fmt.Fprintln(b, "	URL:", s.HTTPS.URL)
	case s.Git != nil:
		git := s.Git
		ref, err := gitutil.ParseGitRef(git.URL)
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(b, "Generated from a git repository:")
		fmt.Fprintln(b, "	Ref:", ref.Commit)
		if s.Path != "" {
			fmt.Fprintln(b, "	Extraced path:", s.Path)
		}
	case s.DockerImage != nil:
		img := s.DockerImage
		if img.Cmd == nil {
			fmt.Fprintln(b, "Generated from a docker image:")
			fmt.Fprintln(b, "	Image:", img.Ref)
			if s.Path != "" {
				fmt.Fprintln(b, "	Extraced path:", s.Path)
			}
		} else {
			fmt.Fprintln(b, "Generated from running a command(s) in a docker image:")
			fmt.Fprintln(b, "	Image:", img.Ref)
			if s.Path != "" {
				fmt.Fprintln(b, "	Extraced path:", s.Path)
			}
			if len(img.Cmd.Env) > 0 {
				fmt.Fprintln(b, "	With the following environment variables set for all commands:")

				sorted := SortMapKeys(img.Cmd.Env)
				for _, k := range sorted {
					fmt.Fprintf(b, "		%s=%s\n", k, img.Cmd.Env[k])
				}
			}
			if img.Cmd.Dir != "" {
				fmt.Fprintln(b, "	Working Directory:", img.Cmd.Dir)
			}
			fmt.Fprintln(b, "	Command(s):")
			for _, step := range img.Cmd.Steps {
				fmt.Fprintf(b, "		%s\n", step.Command)
				if len(step.Env) > 0 {
					fmt.Fprintln(b, "			With the following environment variables set for this command:")
					sorted := SortMapKeys(step.Env)
					for _, k := range sorted {
						fmt.Fprintf(b, "				%s=%s\n", k, step.Env[k])
					}
				}
			}
			if len(img.Cmd.Mounts) > 0 {
				fmt.Fprintln(b, "	With the following items mounted:")
				for _, src := range img.Cmd.Mounts {
					sub, err := src.Spec.Doc()
					if err != nil {
						return nil, err
					}

					fmt.Fprintln(b, "		Destination Path:", src.Dest)
					scanner := bufio.NewScanner(sub)
					for scanner.Scan() {
						fmt.Fprintf(b, "			%s\n", scanner.Text())
					}
					if scanner.Err() != nil {
						return nil, scanner.Err()
					}
				}
			}
			return b, nil
		}
	default:
		// This should be unrecable.
		// We could panic here, but ultimately this is just a doc string and parsing user generated content.
		fmt.Fprintln(b, "Generated from an unknown source type")
	}

	return b, nil
}

func patchSource(worker, sourceState llb.State, sourceToState map[string]llb.State, patchNames []PatchSpec, opts ...llb.ConstraintsOpt) llb.State {
	for _, p := range patchNames {
		patchState := sourceToState[p.Source]
		// on each iteration, mount source state to /src to run `patch`, and
		// set the state under /src to be the source state for the next iteration
		sourceState = worker.Run(
			llb.AddMount("/patch", patchState, llb.Readonly, llb.SourcePath(p.Source)),
			llb.Dir("src"),
			shArgs(fmt.Sprintf("patch -p%d < /patch", *p.Strip)),
			WithConstraints(opts...),
		).AddMount("/src", sourceState)
	}

	return sourceState
}

// `sourceToState` must be a complete map from source name -> llb state for each source in the dalec spec.
// `worker` must be an LLB state with a `patch` binary present.
// PatchSources returns a new map containing the patched LLB state for each source in the source map.
func PatchSources(worker llb.State, spec *Spec, sourceToState map[string]llb.State, opts ...llb.ConstraintsOpt) map[string]llb.State {
	// duplicate map to avoid possibly confusing behavior of mutating caller's map
	states := DuplicateMap(sourceToState)
	pgID := identity.NewID()
	sorted := SortMapKeys(spec.Sources)

	for _, sourceName := range sorted {
		sourceState := states[sourceName]

		patches, patchesExist := spec.Patches[sourceName]
		if !patchesExist {
			continue
		}
		pg := llb.ProgressGroup(pgID, "Patch spec source: "+sourceName+" ", false)
		states[sourceName] = patchSource(worker, sourceState, states, patches, pg, withConstraints(opts))
	}

	return states
}
