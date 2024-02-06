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

// type AsLLBState interface {
// 	// AsState returns an LLB state for the source.
// 	// `name` is used to identify the source in the spec
// 	// `extract` is used to extract a subpath from the source.
// 	// `after` is used to apply an optional filter to the source, or can be used as a general callback to apply additional operations to the source.
// 	// `fOpts` is used to apply a filter to the source.

// 	AsState(name string, parent *Source, after FilterFunc, sOpts SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error)
// }

type FilterFunc = func(string, []string, []string, ...llb.ConstraintsOpt) ResultStateOption

type ResultState struct {
	rs Result[llb.State]
}

func (r ResultState) State() llb.State {
	return r.rs.data
}

func (r ResultState) Err() error {
	return r.rs.err
}

type ResultStateOption func(llb.State) (llb.State, error)

func (r ResultState) With(opts ...ResultStateOption) ResultState {
	if r.rs.IsErr() {
		return r
	}

	for _, opt := range opts {
		st, err := opt(r.rs.data)
		if err != nil {
			r.rs.err = err
		}
		r.rs.data = st
	}

	return r
}

func asResultState(st llb.State) ResultState {
	return ResultState{
		rs: FromT(st),
	}
}

func GetSource(src Source, name string, filter FilterFunc, sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
	if err := src.validate(); err != nil {
		return llb.Scratch(), err
	}

	base := asResultState(llb.Scratch())

	// load the source
	switch {
	case src.HTTP != nil:
		maybeSt := base.With(src.HTTP.AsState(name, opts...)).With(filter(src.Path, src.Includes, src.Excludes, opts...))
		return maybeSt.State(), maybeSt.Err()
	case src.Git != nil:
		maybeSt := base.With(src.Git.AsState(opts...)).With(filter(src.Path, src.Includes, src.Excludes))
		return maybeSt.State(), maybeSt.Err()
	case src.Context != nil:
		rsOpt := src.Context.AsState(&src, sOpt, opts...)
		maybeSt := base.With(rsOpt).
			With(filter(src.Path, []string{}, []string{}))

		return maybeSt.State(), maybeSt.Err()

	case src.DockerImage != nil:
		rsOpt := src.DockerImage.AsState(name, &src, sOpt, opts...)

		rs := base.With(rsOpt)
		if src.DockerImage.Cmd != nil {
			maybeSt := rs.With(filter("/", src.Includes, src.Excludes))
			return maybeSt.State(), maybeSt.Err()
		}

		maybeSt := rs.With(filter(src.Path, src.Includes, src.Excludes))
		return maybeSt.State(), maybeSt.Err()

	case src.Build != nil:
		rsOpt := src.Build.AsState(name, &src, filter, sOpt, opts...)

		maybeSt := base.With(rsOpt).With(filter(src.Path, src.Includes, src.Excludes))
		return maybeSt.State(), maybeSt.Err()
	}

	return llb.Scratch(), fmt.Errorf("no variant defined")
}

func (src *SourceContext) AsState(parent *Source, sOpt SourceOpts, opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(base llb.State) (llb.State, error) {
		st, err := sOpt.GetContext(dockerui.DefaultLocalNameContext, localIncludeExcludeMerge(parent.Includes, parent.Excludes), withConstraints(opts))
		if err != nil {
			return llb.Scratch(), err
		}

		return *st, nil
	}
}

func (src *SourceGit) AsState(opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(base llb.State) (llb.State, error) {
		ref, err := gitutil.ParseGitRef(src.URL)
		if err != nil {
			return llb.Scratch(), fmt.Errorf("could not parse git ref: %w", err)
		}

		var gOpts []llb.GitOption
		if src.KeepGitDir {
			gOpts = append(gOpts, llb.KeepGitDir())
		}
		gOpts = append(gOpts, withConstraints(opts))

		st := llb.Git(ref.Remote, src.Commit, gOpts...)
		return st, nil
	}
	// TODO: Pass git secrets
}

func (src *SourceDockerImage) AsState(name string, parent *Source, sOpt SourceOpts, opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(_ llb.State) (llb.State, error) {
		st := llb.Image(src.Ref, llb.WithMetaResolver(sOpt.Resolver), withConstraints(opts))
		if src.Cmd == nil {
			return st, nil
		}

		st, err := generateSourceFromImage(name, st, src.Cmd, sOpt, parent.Path, opts...)
		if err != nil {
			return llb.Scratch(), err
		}

		return st, nil
	}
}

// return back whether filter is needed, you already know at the call site
// not all AsState() impls need exactly the same call interface
// llb.With() for apply StateOption(s)

func DefaultSourceFilter(extract string, includes, excludes []string, opts ...llb.ConstraintsOpt) ResultStateOption {
	return filterState(extract, includes, excludes, opts...)
}

func TargetMountSourceFilter(_ string, includes, excludes []string, opts ...llb.ConstraintsOpt) ResultStateOption {
	return filterState("/", includes, excludes, opts...)
}

func (src *SourceBuild) AsState(name string, _ *Source, filter FilterFunc, sOpt SourceOpts, opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(_ llb.State) (llb.State, error) {
		st, err := GetSource(src.Source, name, filter, sOpt, opts...)
		if err != nil {
			return llb.Scratch(), err
		}

		st, err = sOpt.Forward(st, src)
		if err != nil {
			return llb.Scratch(), err
		}

		return st, nil
	}
}

func (src *SourceHTTP) AsState(name string, opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(_ llb.State) (llb.State, error) {
		httpOpts := []llb.HTTPOption{withConstraints(opts)}
		httpOpts = append(httpOpts, llb.Filename(name))
		st := llb.HTTP(src.URL, httpOpts...)
		return st, nil
	}
}

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
func generateSourceFromImage(name string, st llb.State, cmd *Command, sOpts SourceOpts, subPath string, opts ...llb.ConstraintsOpt) (llb.State, error) {
	if len(cmd.Steps) == 0 {
		return llb.Scratch(), fmt.Errorf("no steps defined for image source")
	}

	for k, v := range cmd.Env {
		st = st.AddEnv(k, v)
	}
	if cmd.Dir != "" {
		st = st.Dir(cmd.Dir)
	}

	baseRunOpts := []llb.RunOption{CacheDirsToRunOpt(cmd.CacheDirs, "", "")}

	for _, src := range cmd.Mounts {
		srcSt, err := GetSource(src.Spec, name, TargetMountSourceFilter, sOpts, opts...)
		if err != nil {
			return llb.Scratch(), err
		}
		var mountOpt []llb.MountOption
		if src.Spec.Path != "" && len(src.Spec.Includes) == 0 && len(src.Spec.Excludes) == 0 {
			mountOpt = append(mountOpt, llb.SourcePath(src.Spec.Path))
		}
		baseRunOpts = append(baseRunOpts, llb.AddMount(src.Dest, srcSt, mountOpt...))
	}

	out := llb.Scratch()
	for _, step := range cmd.Steps {
		rOpts := []llb.RunOption{llb.Args([]string{
			"/bin/sh", "-c", step.Command,
		})}

		rOpts = append(rOpts, baseRunOpts...)

		for k, v := range step.Env {
			rOpts = append(rOpts, llb.AddEnv(k, v))
		}

		rOpts = append(rOpts, withConstraints(opts))
		cmdSt := st.Run(rOpts...)
		out = cmdSt.AddMount(subPath, out)
	}

	return out, nil
}

func Source2LLBGetter(_ *Spec, src Source, name string, sOpt SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
	return GetSource(src, name, DefaultSourceFilter, sOpt, opts...)
}

func filterState(extract string, includes, excludes []string, opts ...llb.ConstraintsOpt) ResultStateOption {
	return func(st llb.State) (llb.State, error) {
		// if we have no includes, no excludes, and no non-root source path,
		// then this is a no-op
		if len(includes) == 0 && len(excludes) == 0 && extract == "/" {
			return st, nil
		}

		filtered := llb.Scratch().File(
			llb.Copy(
				st,
				extract,
				"/",
				WithIncludes(includes),
				WithExcludes(excludes),
				WithDirContentsOnly(),
			),
			withConstraints(opts),
		)

		return filtered, nil
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
	case src.HTTP != nil:
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
	case s.HTTP != nil:
		fmt.Fprintln(b, "Generated from a http(s) source:")
		fmt.Fprintln(b, "	URL:", s.HTTP.URL)
	case s.Git != nil:
		git := s.Git
		ref, err := gitutil.ParseGitRef(git.URL)
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(b, "Generated from a git repository:")
		fmt.Fprintln(b, "	Remote:", ref.Remote)
		fmt.Fprintln(b, "	Ref:", git.Commit)
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
