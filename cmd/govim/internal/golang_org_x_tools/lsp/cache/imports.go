package cache

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/event"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/event/keys"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/imports"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/span"
)

type importsState struct {
	view *View
	ctx  context.Context

	mu                      sync.Mutex
	processEnv              *imports.ProcessEnv
	cleanupProcessEnv       func()
	cacheRefreshDuration    time.Duration
	cacheRefreshTimer       *time.Timer
	cachedModFileIdentifier string
	cachedBuildFlags        []string
}

func (s *importsState) runProcessEnvFunc(ctx context.Context, snapshot *snapshot, fn func(*imports.Options) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use temporary go.mod files, but always go to disk for the contents.
	// Rebuilding the cache is expensive, and we don't want to do it for
	// transient changes.
	var modFH, sumFH source.FileHandle
	var modFileIdentifier string
	var err error
	// TODO(heschik): Change the goimports logic to use a persistent workspace
	// module for workspace module mode.
	//
	// Get the go.mod file that corresponds to this view's root URI. This is
	// broken because it assumes that the view's root is a module, but this is
	// not more broken than the previous state--it is a temporary hack that
	// should be removed ASAP.
	var match *moduleRoot
	for _, m := range snapshot.modules {
		if m.rootURI == s.view.rootURI {
			match = m
		}
	}
	if match != nil {
		modFH, err = snapshot.GetFile(ctx, match.modURI)
		if err != nil {
			return err
		}
		modFileIdentifier = modFH.FileIdentity().Hash
		if match.sumURI != "" {
			sumFH, err = snapshot.GetFile(ctx, match.sumURI)
			if err != nil {
				return err
			}
		}
	}
	// v.goEnv is immutable -- changes make a new view. Options can change.
	// We can't compare build flags directly because we may add -modfile.
	s.view.optionsMu.Lock()
	localPrefix := s.view.options.Local
	currentBuildFlags := s.view.options.BuildFlags
	changed := !reflect.DeepEqual(currentBuildFlags, s.cachedBuildFlags) ||
		s.view.options.VerboseOutput != (s.processEnv.Logf != nil) ||
		modFileIdentifier != s.cachedModFileIdentifier
	s.view.optionsMu.Unlock()

	// If anything relevant to imports has changed, clear caches and
	// update the processEnv. Clearing caches blocks on any background
	// scans.
	if changed {
		// As a special case, skip cleanup the first time -- we haven't fully
		// initialized the environment yet and calling GetResolver will do
		// unnecessary work and potentially mess up the go.mod file.
		if s.cleanupProcessEnv != nil {
			if resolver, err := s.processEnv.GetResolver(); err == nil {
				resolver.(*imports.ModuleResolver).ClearForNewMod()
			}
			s.cleanupProcessEnv()
		}
		s.cachedModFileIdentifier = modFileIdentifier
		s.cachedBuildFlags = currentBuildFlags
		s.cleanupProcessEnv, err = s.populateProcessEnv(ctx, modFH, sumFH)
		if err != nil {
			return err
		}
	}

	// Run the user function.
	opts := &imports.Options{
		// Defaults.
		AllErrors:   true,
		Comments:    true,
		Fragment:    true,
		FormatOnly:  false,
		TabIndent:   true,
		TabWidth:    8,
		Env:         s.processEnv,
		LocalPrefix: localPrefix,
	}

	if err := fn(opts); err != nil {
		return err
	}

	if s.cacheRefreshTimer == nil {
		// Don't refresh more than twice per minute.
		delay := 30 * time.Second
		// Don't spend more than a couple percent of the time refreshing.
		if adaptive := 50 * s.cacheRefreshDuration; adaptive > delay {
			delay = adaptive
		}
		s.cacheRefreshTimer = time.AfterFunc(delay, s.refreshProcessEnv)
	}

	return nil
}

// populateProcessEnv sets the dynamically configurable fields for the view's
// process environment. Assumes that the caller is holding the s.view.importsMu.
func (s *importsState) populateProcessEnv(ctx context.Context, modFH, sumFH source.FileHandle) (cleanup func(), err error) {
	cleanup = func() {}
	pe := s.processEnv

	s.view.optionsMu.Lock()
	pe.BuildFlags = append([]string(nil), s.view.options.BuildFlags...)
	if s.view.options.VerboseOutput {
		pe.Logf = func(format string, args ...interface{}) {
			event.Log(ctx, fmt.Sprintf(format, args...))
		}
	} else {
		pe.Logf = nil
	}
	s.view.optionsMu.Unlock()

	pe.Env = map[string]string{}
	for k, v := range s.view.goEnv {
		pe.Env[k] = v
	}
	pe.Env["GO111MODULE"] = s.view.go111module

	var modURI span.URI
	var modContent []byte
	if modFH != nil {
		modURI = modFH.URI()
		modContent, err = modFH.Read()
		if err != nil {
			return nil, err
		}
	}
	modmod, err := s.view.needsModEqualsMod(ctx, modURI, modContent)
	if err != nil {
		return cleanup, err
	}
	if modmod {
		// -mod isn't really a build flag, but we can get away with it given
		// the set of commands that goimports wants to run.
		pe.BuildFlags = append([]string{"-mod=mod"}, pe.BuildFlags...)
	}

	// Add -modfile to the build flags, if we are using it.
	if s.view.workspaceMode&tempModfile != 0 && modFH != nil {
		var tmpURI span.URI
		tmpURI, cleanup, err = tempModFile(modFH, sumFH)
		if err != nil {
			return nil, err
		}
		pe.BuildFlags = append(pe.BuildFlags, fmt.Sprintf("-modfile=%s", tmpURI.Filename()))
	}

	return cleanup, nil
}

func (s *importsState) refreshProcessEnv() {
	start := time.Now()

	s.mu.Lock()
	env := s.processEnv
	if resolver, err := s.processEnv.GetResolver(); err == nil {
		resolver.ClearForNewScan()
	}
	s.mu.Unlock()

	event.Log(s.ctx, "background imports cache refresh starting")
	if err := imports.PrimeCache(context.Background(), env); err == nil {
		event.Log(s.ctx, fmt.Sprintf("background refresh finished after %v", time.Since(start)))
	} else {
		event.Log(s.ctx, fmt.Sprintf("background refresh finished after %v", time.Since(start)), keys.Err.Of(err))
	}
	s.mu.Lock()
	s.cacheRefreshDuration = time.Since(start)
	s.cacheRefreshTimer = nil
	s.mu.Unlock()
}
