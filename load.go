package dalec

import (
	"fmt"
	"path"

	"github.com/goccy/go-yaml"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/pkg/errors"
)

func knownArg(key string) bool {
	switch key {
	case "BUILDKIT_SYNTAX":
		return true
	case "DALEC_DISABLE_DIFF_MERGE":
		return true
	case "TARGETOS", "TARGETARCH", "TARGETPLATFORM", "TARGETVARIANT":
		return true
	default:
		return false
	}
}

const DefaultPatchStrip int = 1

func (s *Spec) SubstituteArgs(env map[string]string) error {
	lex := shell.NewLex('\\')

	args := make(map[string]string)
	for k, v := range s.Args {
		args[k] = v
	}
	for k, v := range env {
		if _, ok := args[k]; !ok {
			if !knownArg(k) {
				return fmt.Errorf("unknown arg %q", k)
			}
		}
		args[k] = v
	}

	for name, src := range s.Sources {
		updated, err := lex.ProcessWordWithMap(src.Ref, args)
		if err != nil {
			return fmt.Errorf("error performing shell expansion on source ref %q: %w", name, err)
		}
		src.Ref = updated
		if err := src.Cmd.processBuildArgs(lex, args, name); err != nil {
			return fmt.Errorf("error performing shell expansion on source %q: %w", name, err)
		}
		s.Sources[name] = src
	}

	updated, err := lex.ProcessWordWithMap(s.Version, args)
	if err != nil {
		return fmt.Errorf("error performing shell expansion on version: %w", err)
	}
	s.Version = updated

	updated, err = lex.ProcessWordWithMap(s.Revision, args)
	if err != nil {
		return fmt.Errorf("error performing shell expansion on revision: %w", err)
	}
	s.Revision = updated

	for k, v := range s.Build.Env {
		updated, err := lex.ProcessWordWithMap(v, args)
		if err != nil {
			return fmt.Errorf("error performing shell expansion on env var %q: %w", k, err)
		}
		s.Build.Env[k] = updated
	}

	for i, step := range s.Build.Steps {
		bs := &step
		if err := bs.processBuildArgs(lex, args, i); err != nil {
			return fmt.Errorf("error performing shell expansion on build step %d: %w", i, err)
		}
		s.Build.Steps[i] = *bs
	}

	for _, t := range s.Tests {
		if err := t.processBuildArgs(lex, args, t.Name); err != nil {
			return err
		}
	}

	for name, target := range s.Targets {
		for _, t := range target.Tests {
			if err := t.processBuildArgs(lex, args, path.Join(name, t.Name)); err != nil {
				return err
			}
		}
	}

	for k, patches := range s.Patches {
		for i, ps := range patches {
			if ps.Strip != nil {
				continue
			}
			strip := DefaultPatchStrip
			s.Patches[k][i].Strip = &strip
		}
	}

	return nil
}

// LoadSpec loads a spec from the given data.
// env is a map of environment variables to use for shell-style expansion in the spec.
func LoadSpec(dt []byte) (*Spec, error) {
	var spec Spec
	if err := yaml.Unmarshal(dt, &spec); err != nil {
		return nil, fmt.Errorf("error unmarshalling spec: %w", err)
	}

	return &spec, spec.Validate()
}

func (s *BuildStep) processBuildArgs(lex *shell.Lex, args map[string]string, i int) error {
	for k, v := range s.Env {
		updated, err := lex.ProcessWordWithMap(v, args)
		if err != nil {
			return fmt.Errorf("error performing shell expansion on env var %q for step %d: %w", k, i, err)
		}
		s.Env[k] = updated
	}
	return nil
}

func (c *CmdSpec) processBuildArgs(lex *shell.Lex, args map[string]string, name string) error {
	if c == nil {
		return nil
	}
	for i, smnt := range c.Mounts {
		updated, err := lex.ProcessWordWithMap(smnt.Spec.Ref, args)
		if err != nil {
			return fmt.Errorf("error performing shell expansion on source ref %q: %w", name, err)
		}
		c.Mounts[i].Spec.Ref = updated
	}
	for k, v := range c.Env {
		updated, err := lex.ProcessWordWithMap(v, args)
		if err != nil {
			return fmt.Errorf("error performing shell expansion on env var %q for source %q: %w", k, name, err)
		}
		c.Env[k] = updated
	}
	for i, step := range c.Steps {
		for k, v := range step.Env {
			updated, err := lex.ProcessWordWithMap(v, args)
			if err != nil {
				return fmt.Errorf("error performing shell expansion on env var %q for source %q: %w", k, name, err)
			}
			step.Env[k] = updated
			c.Steps[i] = step
		}
	}

	return nil
}

func (s Spec) Validate() error {
	for name, src := range s.Sources {
		if src.Cmd != nil {
			for p, cfg := range src.Cmd.CacheDirs {
				if _, err := sharingMode(cfg.Mode); err != nil {
					return errors.Wrapf(err, "invalid sharing mode for source %q with cache mount at path %q", name, p)
				}
			}
		}
	}

	for _, t := range s.Tests {
		for p, cfg := range t.CacheDirs {
			if _, err := sharingMode(cfg.Mode); err != nil {
				return errors.Wrapf(err, "invalid sharing mode for test %q with cache mount at path %q", t.Name, p)
			}
		}
	}

	return nil
}

func (c *CheckOutput) processBuildArgs(lex *shell.Lex, args map[string]string) error {
	for i, contains := range c.Contains {
		updated, err := lex.ProcessWordWithMap(contains, args)
		if err != nil {
			return errors.Wrap(err, "error performing shell expansion on contains")
		}
		c.Contains[i] = updated
	}

	updated, err := lex.ProcessWordWithMap(c.EndsWith, args)
	if err != nil {
		return errors.Wrap(err, "error performing shell expansion on endsWith")
	}
	c.EndsWith = updated

	updated, err = lex.ProcessWordWithMap(c.Matches, args)
	if err != nil {
		return errors.Wrap(err, "error performing shell expansion on matches")
	}
	c.Matches = updated

	updated, err = lex.ProcessWordWithMap(c.Equals, args)
	if err != nil {
		return errors.Wrap(err, "error performing shell expansion on equals")
	}
	c.Equals = updated

	updated, err = lex.ProcessWordWithMap(c.StartsWith, args)
	if err != nil {
		return errors.Wrap(err, "error performing shell expansion on startsWith")
	}
	c.StartsWith = updated
	return nil
}

func (c *TestSpec) processBuildArgs(lex *shell.Lex, args map[string]string, name string) error {
	if err := c.CmdSpec.processBuildArgs(lex, args, name); err != nil {
		return err
	}

	for i, step := range c.Steps {
		stdout := step.Stdout
		if err := stdout.processBuildArgs(lex, args); err != nil {
			return err
		}
		step.Stdout = stdout

		stderr := step.Stderr
		if err := stderr.processBuildArgs(lex, args); err != nil {
			return err
		}
		step.Stderr = stderr

		c.Steps[i] = step
	}

	for name, f := range c.Files {
		if err := f.processBuildArgs(lex, args); err != nil {
			return errors.Wrap(err, name)
		}
		c.Files[name] = f
	}

	return nil
}

func (c *FileCheckOutput) processBuildArgs(lex *shell.Lex, args map[string]string) error {
	check := c.CheckOutput
	if err := check.processBuildArgs(lex, args); err != nil {
		return err
	}
	c.CheckOutput = check
	return nil
}
