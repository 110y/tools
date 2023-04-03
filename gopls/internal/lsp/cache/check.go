// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/gopls/internal/lsp/filecache"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/lsp/source"
	"golang.org/x/tools/gopls/internal/lsp/source/methodsets"
	"golang.org/x/tools/gopls/internal/lsp/source/xrefs"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/bug"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/event/tag"
	"golang.org/x/tools/internal/gcimporter"
	"golang.org/x/tools/internal/packagesinternal"
	"golang.org/x/tools/internal/tokeninternal"
	"golang.org/x/tools/internal/typeparams"
	"golang.org/x/tools/internal/typesinternal"
)

// Various optimizations that should not affect correctness.
const (
	preserveImportGraph = true // hold on to the import graph for open packages
)

// A typeCheckBatch holds data for a logical type-checking operation, which may
// type-check many unrelated packages.
//
// It shares state such as parsed files and imports, to optimize type-checking
// for packages with overlapping dependency graphs.
type typeCheckBatch struct {
	syntaxIndex map[PackageID]int // requested ID -> index in ids
	pre         preTypeCheck
	post        postTypeCheck

	s        *snapshot
	meta     *metadataGraph // captured once at the start of type checking
	fset     *token.FileSet // describes all parsed or imported files
	cpulimit chan struct{}  // concurrency limiter for CPU-bound operations

	mu             sync.Mutex
	syntaxPackages map[PackageID]*futurePackage // results of processing a requested package; may hold (nil, nil)
	importPackages map[PackageID]*futurePackage // package results to use for importing
}

// A futurePackage is a future result of type checking or importing a package,
// to be cached in a map.
//
// The goroutine that creates the futurePackage is responsible for evaluating
// its value, and closing the done channel.
type futurePackage struct {
	done chan struct{}
	v    pkgOrErr
}

type pkgOrErr struct {
	pkg *types.Package
	err error
}

// TypeCheck type-checks the specified packages.
//
// The resulting packages slice always contains len(ids) entries, though some
// of them may be nil if (and only if) the resulting error is non-nil.
//
// An error is returned if any of the requested packages fail to type-check.
// This is different from having type-checking errors: a failure to type-check
// indicates context cancellation or otherwise significant failure to perform
// the type-checking operation.
func (s *snapshot) TypeCheck(ctx context.Context, ids ...PackageID) ([]source.Package, error) {
	pkgs := make([]source.Package, len(ids))

	var (
		needIDs []PackageID // ids to type-check
		indexes []int       // original index of requested ids
	)

	// Check for existing active packages.
	//
	// Since gopls can't depend on package identity, any instance of the
	// requested package must be ok to return.
	//
	// This is an optimization to avoid redundant type-checking: following
	// changes to an open package many LSP clients send several successive
	// requests for package information for the modified package (semantic
	// tokens, code lens, inlay hints, etc.)
	for i, id := range ids {
		if pkg := s.getActivePackage(id); pkg != nil {
			pkgs[i] = pkg
		} else {
			needIDs = append(needIDs, id)
			indexes = append(indexes, i)
		}
	}

	post := func(i int, pkg *Package) {
		if alt := s.memoizeActivePackage(pkg.m.ID, pkg); alt != nil && alt != pkg {
			// pkg is an open package, but we've lost a race and an existing package
			// has already been memoized.
			pkg = alt
		}
		pkgs[indexes[i]] = pkg
	}
	return pkgs, s.forEachPackage(ctx, needIDs, nil, post)
}

// getImportGraph returns a shared import graph use for this snapshot, or nil.
//
// This is purely an optimization: holding on to more imports allows trading
// memory for CPU and latency. Currently, getImportGraph returns an import
// graph containing all packages imported by open packages, since these are
// highly likely to be needed when packages change.
//
// Furthermore, since we memoize active packages, including their imports in
// the shared import graph means we don't run the risk of pinning duplicate
// copies of common imports, if active packages are computed in separate type
// checking batches.
func (s *snapshot) getImportGraph(ctx context.Context) *importGraph {
	if !preserveImportGraph {
		return nil
	}
	s.mu.Lock()

	// Evaluate the shared import graph for the snapshot. There are three major
	// codepaths here:
	//
	//  1. importGraphDone == nil, importGraph == nil: it is this goroutine's
	//     responsibility to type-check the shared import graph.
	//  2. importGraphDone == nil, importGraph != nil: it is this goroutine's
	//     responsibility to resolve the import graph, which may result in
	//     type-checking only if the existing importGraph (carried over from the
	//     preceding snapshot) is invalid.
	//  3. importGraphDone != nil: some other goroutine is doing (1) or (2), wait
	//     for the work to be done.
	done := s.importGraphDone
	if done == nil {
		done = make(chan struct{})
		s.importGraphDone = done
		release := s.Acquire() // must acquire to use the snapshot asynchronously
		go func() {
			defer release()
			importGraph, err := s.resolveImportGraph() // may be nil
			if err != nil {
				if ctx.Err() == nil {
					event.Error(ctx, "computing the shared import graph", err)
				}
				importGraph = nil
			}
			s.mu.Lock()
			s.importGraph = importGraph
			s.mu.Unlock()
			close(done)
		}()
	}
	s.mu.Unlock()

	select {
	case <-done:
		return s.importGraph
	case <-ctx.Done():
		return nil
	}
}

// resolveImportGraph evaluates the shared import graph to use for
// type-checking in this snapshot. This may involve re-using the import graph
// of the previous snapshot (stored in s.importGraph), or computing a fresh
// import graph.
//
// resolveImportGraph should only be called from getImportGraph.
func (s *snapshot) resolveImportGraph() (*importGraph, error) {
	ctx := s.backgroundCtx
	ctx, done := event.Start(event.Detach(ctx), "cache.resolveImportGraph")
	defer done()

	if err := s.awaitLoaded(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	meta := s.meta
	lastImportGraph := s.importGraph
	s.mu.Unlock()

	openPackages := make(map[PackageID]bool)
	for _, fh := range s.overlays() {
		for _, id := range meta.ids[fh.URI()] {
			openPackages[id] = true
		}
	}

	// TODO(rfindley): when building package handles becomes more expensive (due
	// to precise pruning support), we may want to parallelize this loop.
	deps := make(map[PackageID]source.Hash)
	for id := range openPackages {
		m := meta.metadata[id]
		for _, id := range m.DepsByPkgPath {
			if _, ok := deps[id]; !ok {
				ph, err := s.buildPackageHandle(ctx, id)
				if err != nil {
					if ctx.Err() != nil {
						return nil, err
					} else {
						continue
					}
				}
				deps[id] = ph.key
			}
		}
	}

	// Subtlety: we erase the upward cone of open packages from the shared import
	// graph, to increase reusability.
	//
	// This is easiest to understand via an example: suppose A imports B, and B
	// imports C. Now suppose A and B are open. If we preserve the entire set of
	// shared deps by open packages, deps will be {B, C}. But this means that any
	// change to the open package B will invalidate the shared import graph,
	// meaning we will experience no benefit from sharing when B is edited.
	// Consider that this will be a common scenario, when A is foo_test and B is
	// foo. Better to just preserve the shared import C.
	//
	// With precise pruning, we may want to truncate this search based on
	// reachability.
	//
	// TODO(rfindley): this logic could use a unit test.
	volatileDeps := make(map[PackageID]bool)
	var isVolatile func(PackageID) bool
	isVolatile = func(id PackageID) (volatile bool) {
		if v, ok := volatileDeps[id]; ok {
			return v
		}
		defer func() {
			volatileDeps[id] = volatile
		}()
		if openPackages[id] {
			return true
		}
		m := meta.metadata[id]
		if m != nil {
			for _, dep := range m.DepsByPkgPath {
				if isVolatile(dep) {
					return true
				}
			}
		}
		return false
	}
	for dep := range deps {
		isVolatile(dep)
	}
	for id, volatile := range volatileDeps {
		if volatile {
			delete(deps, id)
		}
	}

	// We reuse the last import graph if and only if none of the dependencies
	// have changed. Doing better would involve analyzing dependencies to find
	// subgraphs that are still valid. Not worth it, especially when in the
	// common case nothing has changed.
	unchanged := lastImportGraph != nil && len(deps) == len(lastImportGraph.deps)
	var ids []PackageID
	for id, key := range deps {
		ids = append(ids, id)
		if unchanged {
			prevKey, ok := lastImportGraph.deps[id]
			unchanged = ok && prevKey == key
		}
	}

	if unchanged {
		return lastImportGraph, nil
	}

	b, err := s.forEachPackageInternal(ctx, nil, ids, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	next := &importGraph{
		fset:    b.fset,
		deps:    deps,
		imports: make(map[PackageID]pkgOrErr),
	}
	for id, fut := range b.importPackages {
		if fut.v.pkg == nil && fut.v.err == nil {
			panic(fmt.Sprintf("internal error: import node %s is not evaluated", id))
		}
		next.imports[id] = fut.v
	}
	return next, nil
}

// An importGraph holds selected results of a type-checking pass, to be re-used
// by subsequent snapshots.
type importGraph struct {
	fset    *token.FileSet            // fileset used for type checking imports
	deps    map[PackageID]source.Hash // hash of direct dependencies for this graph
	imports map[PackageID]pkgOrErr    // results of type checking
}

// Package visiting functions used by forEachPackage; see the documentation of
// forEachPackage for details.
type (
	preTypeCheck  = func(int, *packageHandle) bool // false => don't type check
	postTypeCheck = func(int, *Package)
)

// forEachPackage does a pre- and post- order traversal of the packages
// specified by ids using the provided pre and post functions.
//
// The pre func is is optional. If set, pre is evaluated after the package
// handle has been constructed, but before type-checking. If pre returns false,
// type-checking is skipped for this package handle.
//
// post is called with a syntax package after type-checking completes
// successfully. It is only called if pre returned true.
//
// Both pre and post may be called concurrently.
func (s *snapshot) forEachPackage(ctx context.Context, ids []PackageID, pre preTypeCheck, post postTypeCheck) error {
	ctx, done := event.Start(ctx, "cache.forEachPackage", tag.PackageCount.Of(len(ids)))
	defer done()

	if len(ids) == 0 {
		return nil // short cut: many call sites do not handle empty ids
	}

	impGraph := s.getImportGraph(ctx)
	_, err := s.forEachPackageInternal(ctx, impGraph, nil, ids, pre, post)
	return err
}

// forEachPackageInternal is used by both forEachPackage and loadImportGraph to
// type-check a graph of packages.
//
// If a non-nil importGraph is provided, imports in this graph will be reused.
func (s *snapshot) forEachPackageInternal(ctx context.Context, importGraph *importGraph, importIDs, syntaxIDs []PackageID, pre preTypeCheck, post postTypeCheck) (*typeCheckBatch, error) {
	b := &typeCheckBatch{
		s:              s,
		pre:            pre,
		post:           post,
		fset:           fileSetWithBase(reservedForParsing),
		syntaxIndex:    make(map[PackageID]int),
		cpulimit:       make(chan struct{}, runtime.GOMAXPROCS(0)),
		syntaxPackages: make(map[PackageID]*futurePackage),
		importPackages: make(map[PackageID]*futurePackage),
	}

	if importGraph != nil {
		// Clone the file set every time, to ensure we do not leak files.
		b.fset = tokeninternal.CloneFileSet(importGraph.fset)
		// Pre-populate future cache with 'done' futures.
		done := make(chan struct{})
		close(done)
		for id, res := range importGraph.imports {
			b.importPackages[id] = &futurePackage{done, res}
		}
	} else {
		b.fset = fileSetWithBase(reservedForParsing)
	}

	// Capture metadata once to avoid locking the snapshot when accessing
	// metadata.
	s.mu.Lock()
	b.meta = s.meta
	s.mu.Unlock()

	for i, id := range syntaxIDs {
		b.syntaxIndex[id] = i
	}

	// Start a single goroutine for each requested package.
	//
	// Other packages are reached recursively, and will not be evaluated if they
	// are not needed.
	var g errgroup.Group
	for _, id := range importIDs {
		id := id
		g.Go(func() error {
			_, err := b.getImportPackage(ctx, id)
			return err
		})
	}
	for i, id := range syntaxIDs {
		i := i
		id := id
		g.Go(func() error {
			_, err := b.handleSyntaxPackage(ctx, i, id)
			return err
		})
	}
	return b, g.Wait()
}

// TODO(rfindley): re-order the declarations below to read better from top-to-bottom.

// getImportPackage returns the *types.Package to use for importing the
// package referenced by id.
//
// This may be the package produced by type-checking syntax (as in the case
// where id is in the set of requested IDs), a package loaded from export data,
// or a package type-checked for import only.
func (b *typeCheckBatch) getImportPackage(ctx context.Context, id PackageID) (pkg *types.Package, err error) {
	b.mu.Lock()
	f, ok := b.importPackages[id]
	if ok {
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			return f.v.pkg, f.v.err
		}
	}

	f = &futurePackage{done: make(chan struct{})}
	b.importPackages[id] = f
	b.mu.Unlock()

	defer func() {
		f.v = pkgOrErr{pkg, err}
		close(f.done)
	}()

	if index, ok := b.syntaxIndex[id]; ok {
		pkg, err := b.handleSyntaxPackage(ctx, index, id)
		if err != nil {
			return nil, err
		}
		if pkg != nil {
			return pkg, nil
		}
		// type-checking was short-circuited by the pre- func.
	}

	// unsafe cannot be imported or type-checked.
	if id == "unsafe" {
		return types.Unsafe, nil
	}

	ph, err := b.s.buildPackageHandle(ctx, id)
	if err != nil {
		return nil, err
	}

	data, err := filecache.Get(exportDataKind, ph.key)
	if err == filecache.ErrNotFound {
		// No cached export data: type-check as fast as possible.
		return b.checkPackageForImport(ctx, ph)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read cache data for %s: %v", ph.m.ID, err)
	}
	return b.importPackage(ctx, ph.m, data)
}

// handleSyntaxPackage handles one package from the ids slice.
//
// If type checking occurred while handling the package, it returns the
// resulting types.Package so that it may be used for importing.
//
// handleSyntaxPackage returns (nil, nil) if pre returned false.
func (b *typeCheckBatch) handleSyntaxPackage(ctx context.Context, i int, id PackageID) (pkg *types.Package, err error) {
	b.mu.Lock()
	f, ok := b.syntaxPackages[id]
	if ok {
		b.mu.Unlock()
		<-f.done
		return f.v.pkg, f.v.err
	}

	f = &futurePackage{done: make(chan struct{})}
	b.syntaxPackages[id] = f
	b.mu.Unlock()
	defer func() {
		f.v = pkgOrErr{pkg, err}
		close(f.done)
	}()

	ph, err := b.s.buildPackageHandle(ctx, id)
	if err != nil {
		return nil, err
	}

	if b.pre != nil && !b.pre(i, ph) {
		return nil, nil // skip: export data only
	}

	if err := b.awaitPredecessors(ctx, ph.m); err != nil {
		// One failed precessesor should not fail the entire type checking
		// operation. Errors related to imports will be reported as type checking
		// diagnostics.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	// Wait to acquire a CPU token.
	//
	// Note: it is important to acquire this token only after awaiting
	// predecessors, to avoid starvation.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b.cpulimit <- struct{}{}:
		defer func() {
			<-b.cpulimit // release CPU token
		}()
	}

	// We need a syntax package.
	syntaxPkg, err := b.checkPackage(ctx, ph)
	if err != nil {
		return nil, err
	}
	b.post(i, syntaxPkg)
	return syntaxPkg.pkg.types, nil
}

// importPackage loads the given package from its export data in p.exportData
// (which must already be populated).
func (b *typeCheckBatch) importPackage(ctx context.Context, m *source.Metadata, data []byte) (*types.Package, error) {
	ctx, done := event.Start(ctx, "cache.typeCheckBatch.importPackage", tag.Package.Of(string(m.ID)))
	defer done()

	impMap := b.importMap(m.ID)

	var firstErr error // TODO(rfindley): unused: revisit or remove.
	thisPackage := types.NewPackage(string(m.PkgPath), string(m.Name))
	getPackage := func(path, name string) *types.Package {
		if path == string(m.PkgPath) {
			return thisPackage
		}

		id := impMap[path]
		imp, err := b.getImportPackage(ctx, id)
		if err == nil {
			return imp
		}
		// inv: err != nil
		if firstErr == nil {
			firstErr = err
		}

		// Context cancellation, or a very bad error such as a file permission
		// error.
		//
		// Returning nil here will cause the import to fail (and panic if
		// gcimporter.debug is set), but that is preferable to the confusing errors
		// produced when shallow import encounters an empty package.
		return nil
	}

	// Importing is potentially expensive, and might not encounter cancellations
	// via dependencies (e.g. if they have already been evaluated).
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// TODO(rfindley): collect "deep" hashes here using the provided
	// callback, for precise pruning.
	imported, err := gcimporter.IImportShallow(b.fset, getPackage, data, string(m.PkgPath), func(*types.Package, string) {})
	if err != nil {
		return nil, fmt.Errorf("import failed for %q: %v", m.ID, err)
	}
	return imported, nil
}

// checkPackageForImport type checks, but skips function bodies and does not
// record syntax information.
func (b *typeCheckBatch) checkPackageForImport(ctx context.Context, ph *packageHandle) (*types.Package, error) {
	ctx, done := event.Start(ctx, "cache.typeCheckBatch.checkPackageForImport", tag.Package.Of(string(ph.m.ID)))
	defer done()

	onError := func(e error) {
		// Ignore errors for exporting.
	}
	cfg := b.typesConfig(ctx, ph.inputs, onError)
	cfg.IgnoreFuncBodies = true
	pgfs, err := b.s.parseCache.parseFiles(ctx, b.fset, source.ParseFull, ph.inputs.compiledGoFiles...)
	if err != nil {
		return nil, err
	}
	pkg := types.NewPackage(string(ph.inputs.pkgPath), string(ph.inputs.name))
	check := types.NewChecker(cfg, b.fset, pkg, nil)

	files := make([]*ast.File, len(pgfs))
	for i, pgf := range pgfs {
		files[i] = pgf.File
	}

	// Type checking is expensive, and we may not have ecountered cancellations
	// via parsing (e.g. if we got nothing but cache hits for parsed files).
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	_ = check.Files(files) // ignore errors

	// If the context was cancelled, we may have returned a ton of transient
	// errors to the type checker. Swallow them.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Asynchronously record export data.
	go func() {
		exportData, err := gcimporter.IExportShallow(b.fset, pkg)
		if err != nil {
			bug.Reportf("exporting package %v: %v", ph.m.ID, err)
			return
		}
		if err := filecache.Set(exportDataKind, ph.key, exportData); err != nil {
			event.Error(ctx, fmt.Sprintf("storing export data for %s", ph.m.ID), err)
		}
	}()
	return pkg, nil
}

// checkPackage "fully type checks" to produce a syntax package.
func (b *typeCheckBatch) checkPackage(ctx context.Context, ph *packageHandle) (*Package, error) {
	ctx, done := event.Start(ctx, "cache.typeCheckBatch.checkPackage", tag.Package.Of(string(ph.m.ID)))
	defer done()

	// TODO(rfindley): refactor to inline typeCheckImpl here. There is no need
	// for so many layers to build up the package
	// (checkPackage->typeCheckImpl->doTypeCheck).
	pkg, err := typeCheckImpl(ctx, b, ph.inputs)

	if err == nil {
		// Write package data to disk asynchronously.
		go func() {
			toCache := map[string][]byte{
				xrefsKind:       pkg.xrefs,
				methodSetsKind:  pkg.methodsets.Encode(),
				diagnosticsKind: encodeDiagnostics(pkg.diagnostics),
			}

			if ph.m.ID != "unsafe" { // unsafe cannot be exported
				exportData, err := gcimporter.IExportShallow(pkg.fset, pkg.types)
				if err != nil {
					bug.Reportf("exporting package %v: %v", ph.m.ID, err)
				} else {
					toCache[exportDataKind] = exportData
				}
			}

			for kind, data := range toCache {
				if err := filecache.Set(kind, ph.key, data); err != nil {
					event.Error(ctx, fmt.Sprintf("storing %s data for %s", kind, ph.m.ID), err)
				}
			}
		}()
	}

	return &Package{ph.m, pkg}, err
}

// awaitPredecessors awaits all packages for m.DepsByPkgPath, returning an
// error if awaiting failed due to context cancellation or if there was an
// unrecoverable error loading export data.
//
// TODO(rfindley): inline, now that this is only called in one place.
func (b *typeCheckBatch) awaitPredecessors(ctx context.Context, m *source.Metadata) error {
	// await predecessors concurrently, as some of them may be non-syntax
	// packages, and therefore will not have been started by the type-checking
	// batch.
	var g errgroup.Group
	for _, depID := range m.DepsByPkgPath {
		depID := depID
		g.Go(func() error {
			_, err := b.getImportPackage(ctx, depID)
			return err
		})
	}
	return g.Wait()
}

// importMap returns the map of package path -> package ID relative to the
// specified ID.
func (b *typeCheckBatch) importMap(id PackageID) map[string]source.PackageID {
	impMap := make(map[string]source.PackageID)
	var populateDeps func(m *source.Metadata)
	populateDeps = func(parent *source.Metadata) {
		for _, id := range parent.DepsByPkgPath {
			m := b.meta.metadata[id]
			if _, ok := impMap[string(m.PkgPath)]; ok {
				continue
			}
			impMap[string(m.PkgPath)] = m.ID
			populateDeps(m)
		}
	}
	m := b.meta.metadata[id]
	populateDeps(m)
	return impMap
}

// A packageHandle holds package information, some of which may not be fully
// evaluated.
//
// The only methods on packageHandle that are safe to call before calling await
// are Metadata and await itself.
type packageHandle struct {
	m *source.Metadata

	inputs typeCheckInputs

	// key is the hashed key for the package.
	//
	// It includes the all bits of the transitive closure of
	// dependencies's sources. This is more than type checking
	// really depends on: export data of direct deps should be
	// enough. (The key for analysis actions could similarly
	// hash only Facts of direct dependencies.)
	key source.Hash
}

// buildPackageHandle returns a handle for the future results of
// type-checking the package identified by id in the given mode.
// It assumes that the given ID already has metadata available, so it does not
// attempt to reload missing or invalid metadata. The caller must reload
// metadata if needed.
func (s *snapshot) buildPackageHandle(ctx context.Context, id PackageID) (*packageHandle, error) {
	s.mu.Lock()
	entry, hit := s.packages.Get(id)
	m := s.meta.metadata[id]
	s.mu.Unlock()

	if m == nil {
		return nil, fmt.Errorf("no metadata for %s", id)
	}

	if hit {
		return entry.(*packageHandle), nil
	}

	inputs, err := s.typeCheckInputs(ctx, m)
	if err != nil {
		return nil, err
	}
	// All the file reading has now been done.
	// Create a handle for the result of type checking.
	phKey := computePackageKey(s, inputs)
	ph := &packageHandle{
		m:      m,
		inputs: inputs,
		key:    phKey,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check that the metadata has not changed
	// (which should invalidate this handle).
	//
	// (In future, handles should form a graph with edges from a
	// packageHandle to the handles for parsing its files and the
	// handles for type-checking its immediate deps, at which
	// point there will be no need to even access s.meta.)
	if s.meta.metadata[ph.m.ID] != ph.m {
		// TODO(rfindley): this should be bug.Errorf.
		return nil, fmt.Errorf("stale metadata for %s", ph.m.ID)
	}

	// Check cache again in case another goroutine got there first.
	if prev, ok := s.packages.Get(id); ok {
		prevPH := prev.(*packageHandle)
		if prevPH.m != ph.m {
			return nil, bug.Errorf("existing package handle does not match for %s", ph.m.ID)
		}
		return prevPH, nil
	}

	s.packages.Set(id, ph, nil)
	return ph, nil
}

// typeCheckInputs contains the inputs of a call to typeCheckImpl, which
// type-checks a package.
//
// Part of the purpose of this type is to keep type checking in-sync with the
// package handle key, by explicitly identifying the inputs to type checking.
type typeCheckInputs struct {
	id PackageID

	// Used for type checking:
	pkgPath                  PackagePath
	name                     PackageName
	goFiles, compiledGoFiles []source.FileHandle
	sizes                    types.Sizes
	deps                     map[PackageID]*packageHandle
	depsByImpPath            map[ImportPath]PackageID
	goVersion                string // packages.Module.GoVersion, e.g. "1.18"

	// Used for type check diagnostics:
	relatedInformation bool
	linkTarget         string
	moduleMode         bool
}

func (s *snapshot) typeCheckInputs(ctx context.Context, m *source.Metadata) (typeCheckInputs, error) {
	deps := make(map[PackageID]*packageHandle)
	for _, depID := range m.DepsByPkgPath {
		depHandle, err := s.buildPackageHandle(ctx, depID)
		if err != nil {
			// If err is non-nil, we either have an invalid dependency, or a
			// catastrophic failure to read a file (context cancellation or
			// permission issues).
			//
			// We don't want one bad dependency to prevent us from type-checking the
			// package -- we should instead get an import error. So we only abort
			// this operation if the context is cancelled.
			//
			// We could do a better job of handling permission errors on files, but
			// this is rare, and it is reasonable to treat the same an invalid
			// dependency.
			event.Error(ctx, fmt.Sprintf("%s: no dep handle for %s", m.ID, depID), err, source.SnapshotLabels(s)...)
			if ctx.Err() != nil {
				return typeCheckInputs{}, ctx.Err() // cancelled
			}
			continue
		}
		deps[depID] = depHandle
	}

	// Read both lists of files of this package.
	//
	// Parallelism is not necessary here as the files will have already been
	// pre-read at load time.
	//
	// goFiles aren't presented to the type checker--nor
	// are they included in the key, unsoundly--but their
	// syntax trees are available from (*pkg).File(URI).
	// TODO(adonovan): consider parsing them on demand?
	// The need should be rare.
	goFiles, err := readFiles(ctx, s, m.GoFiles)
	if err != nil {
		return typeCheckInputs{}, err
	}
	compiledGoFiles, err := readFiles(ctx, s, m.CompiledGoFiles)
	if err != nil {
		return typeCheckInputs{}, err
	}

	goVersion := ""
	if m.Module != nil && m.Module.GoVersion != "" {
		goVersion = m.Module.GoVersion
	}

	return typeCheckInputs{
		id:              m.ID,
		pkgPath:         m.PkgPath,
		name:            m.Name,
		goFiles:         goFiles,
		compiledGoFiles: compiledGoFiles,
		sizes:           m.TypesSizes,
		deps:            deps,
		depsByImpPath:   m.DepsByImpPath,
		goVersion:       goVersion,

		relatedInformation: s.view.Options().RelatedInformationSupported,
		linkTarget:         s.view.Options().LinkTarget,
		moduleMode:         s.moduleMode(),
	}, nil
}

// readFiles reads the content of each file URL from the source
// (e.g. snapshot or cache).
func readFiles(ctx context.Context, fs source.FileSource, uris []span.URI) (_ []source.FileHandle, err error) {
	fhs := make([]source.FileHandle, len(uris))
	for i, uri := range uris {
		fhs[i], err = fs.ReadFile(ctx, uri)
		if err != nil {
			return nil, err
		}
	}
	return fhs, nil
}

// computePackageKey returns a key representing the act of type checking
// a package named id containing the specified files, metadata, and
// combined dependency hash.
func computePackageKey(s *snapshot, inputs typeCheckInputs) source.Hash {
	hasher := sha256.New()

	// In principle, a key must be the hash of an
	// unambiguous encoding of all the relevant data.
	// If it's ambiguous, we risk collisions.

	// package identifiers
	fmt.Fprintf(hasher, "package: %s %s %s\n", inputs.id, inputs.name, inputs.pkgPath)

	// module Go version
	fmt.Fprintf(hasher, "go %s\n", inputs.goVersion)

	// import map
	importPaths := make([]string, 0, len(inputs.depsByImpPath))
	for impPath := range inputs.depsByImpPath {
		importPaths = append(importPaths, string(impPath))
	}
	sort.Strings(importPaths)
	for _, impPath := range importPaths {
		fmt.Fprintf(hasher, "import %s %s", impPath, string(inputs.depsByImpPath[ImportPath(impPath)]))
	}

	// deps, in PackageID order
	depIDs := make([]string, 0, len(inputs.deps))
	for depID := range inputs.deps {
		depIDs = append(depIDs, string(depID))
	}
	sort.Strings(depIDs)
	for _, depID := range depIDs {
		dep := inputs.deps[PackageID(depID)]
		fmt.Fprintf(hasher, "dep: %s key:%s\n", dep.m.PkgPath, dep.key)
	}

	// file names and contents
	fmt.Fprintf(hasher, "compiledGoFiles: %d\n", len(inputs.compiledGoFiles))
	for _, fh := range inputs.compiledGoFiles {
		fmt.Fprintln(hasher, fh.FileIdentity())
	}
	fmt.Fprintf(hasher, "goFiles: %d\n", len(inputs.goFiles))
	for _, fh := range inputs.goFiles {
		fmt.Fprintln(hasher, fh.FileIdentity())
	}

	// types sizes
	sz := inputs.sizes.(*types.StdSizes)
	fmt.Fprintf(hasher, "sizes: %d %d\n", sz.WordSize, sz.MaxAlign)

	fmt.Fprintf(hasher, "relatedInformation: %t\n", inputs.relatedInformation)
	fmt.Fprintf(hasher, "linkTarget: %s\n", inputs.linkTarget)
	fmt.Fprintf(hasher, "moduleMode: %t\n", inputs.moduleMode)

	var hash [sha256.Size]byte
	hasher.Sum(hash[:0])
	return hash
}

// typeCheckImpl type checks the parsed source files in compiledGoFiles.
// (The resulting pkg also holds the parsed but not type-checked goFiles.)
// deps holds the future results of type-checking the direct dependencies.
func typeCheckImpl(ctx context.Context, b *typeCheckBatch, inputs typeCheckInputs) (*syntaxPackage, error) {
	ctx, done := event.Start(ctx, "cache.typeCheck", tag.Package.Of(string(inputs.id)))
	defer done()

	pkg, err := doTypeCheck(ctx, b, inputs)
	if err != nil {
		return nil, err
	}
	pkg.methodsets = methodsets.NewIndex(pkg.fset, pkg.types)
	pkg.xrefs = xrefs.Index(pkg.compiledGoFiles, pkg.types, pkg.typesInfo)

	// Our heuristic for whether to show type checking errors is:
	//  + If any file was 'fixed', don't show type checking errors as we
	//    can't guarantee that they reference accurate locations in the source.
	//  + If there is a parse error _in the current file_, suppress type
	//    errors in that file.
	//  + Otherwise, show type errors even in the presence of parse errors in
	//    other package files. go/types attempts to suppress follow-on errors
	//    due to bad syntax, so on balance type checking errors still provide
	//    a decent signal/noise ratio as long as the file in question parses.

	// Track URIs with parse errors so that we can suppress type errors for these
	// files.
	unparseable := map[span.URI]bool{}
	for _, e := range pkg.parseErrors {
		diags, err := parseErrorDiagnostics(pkg, e)
		if err != nil {
			event.Error(ctx, "unable to compute positions for parse errors", err, tag.Package.Of(string(inputs.id)))
			continue
		}
		for _, diag := range diags {
			unparseable[diag.URI] = true
			pkg.diagnostics = append(pkg.diagnostics, diag)
		}
	}

	if pkg.hasFixedFiles {
		return pkg, nil
	}

	unexpanded := pkg.typeErrors
	pkg.typeErrors = nil
	for _, e := range expandErrors(unexpanded, inputs.relatedInformation) {
		diags, err := typeErrorDiagnostics(inputs.moduleMode, inputs.linkTarget, pkg, e)
		if err != nil {
			// If we fail here and there are no parse errors, it means we are hiding
			// a valid type-checking error from the user. This must be a bug.
			if len(pkg.parseErrors) == 0 {
				bug.Reportf("failed to compute position for type error %v: %v", e, err)
			}
			continue
		}
		pkg.typeErrors = append(pkg.typeErrors, e.primary)
		for _, diag := range diags {
			// If the file didn't parse cleanly, it is highly likely that type
			// checking errors will be confusing or redundant. But otherwise, type
			// checking usually provides a good enough signal to include.
			if !unparseable[diag.URI] {
				pkg.diagnostics = append(pkg.diagnostics, diag)
			}
		}
	}

	return pkg, nil
}

var goVersionRx = regexp.MustCompile(`^go([1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func doTypeCheck(ctx context.Context, b *typeCheckBatch, inputs typeCheckInputs) (*syntaxPackage, error) {
	pkg := &syntaxPackage{
		id:    inputs.id,
		fset:  b.fset, // must match parse call below
		types: types.NewPackage(string(inputs.pkgPath), string(inputs.name)),
		typesInfo: &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Implicits:  make(map[ast.Node]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
			Scopes:     make(map[ast.Node]*types.Scope),
		},
	}
	typeparams.InitInstanceInfo(pkg.typesInfo)

	// Collect parsed files from the type check pass, capturing parse errors from
	// compiled files.
	var err error
	pkg.goFiles, err = b.s.parseCache.parseFiles(ctx, b.fset, source.ParseFull, inputs.goFiles...)
	if err != nil {
		return nil, err
	}
	pkg.compiledGoFiles, err = b.s.parseCache.parseFiles(ctx, b.fset, source.ParseFull, inputs.compiledGoFiles...)
	if err != nil {
		return nil, err
	}
	for _, pgf := range pkg.compiledGoFiles {
		if pgf.ParseErr != nil {
			pkg.parseErrors = append(pkg.parseErrors, pgf.ParseErr)
		}
	}

	// Use the default type information for the unsafe package.
	if inputs.pkgPath == "unsafe" {
		// Don't type check Unsafe: it's unnecessary, and doing so exposes a data
		// race to Unsafe.completed.
		pkg.types = types.Unsafe
		return pkg, nil
	}

	if len(pkg.compiledGoFiles) == 0 {
		// No files most likely means go/packages failed.
		//
		// TODO(rfindley): in the past, we would capture go list errors in this
		// case, to present go list errors to the user. However we had no tests for
		// this behavior. It is unclear if anything better can be done here.
		return nil, fmt.Errorf("no parsed files for package %s", inputs.pkgPath)
	}

	onError := func(e error) {
		pkg.typeErrors = append(pkg.typeErrors, e.(types.Error))
	}
	cfg := b.typesConfig(ctx, inputs, onError)

	check := types.NewChecker(cfg, pkg.fset, pkg.types, pkg.typesInfo)

	var files []*ast.File
	for _, cgf := range pkg.compiledGoFiles {
		files = append(files, cgf.File)
	}

	// Type checking is expensive, and we may not have ecountered cancellations
	// via parsing (e.g. if we got nothing but cache hits for parsed files).
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Type checking errors are handled via the config, so ignore them here.
	_ = check.Files(files) // 50us-15ms, depending on size of package

	// If the context was cancelled, we may have returned a ton of transient
	// errors to the type checker. Swallow them.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect imports by package path for the DependencyTypes API.
	pkg.importMap = make(map[PackagePath]*types.Package)
	var collectDeps func(*types.Package)
	collectDeps = func(p *types.Package) {
		pkgPath := PackagePath(p.Path())
		if _, ok := pkg.importMap[pkgPath]; ok {
			return
		}
		pkg.importMap[pkgPath] = p
		for _, imp := range p.Imports() {
			collectDeps(imp)
		}
	}
	collectDeps(pkg.types)

	return pkg, nil
}

func (b *typeCheckBatch) typesConfig(ctx context.Context, inputs typeCheckInputs, onError func(e error)) *types.Config {
	cfg := &types.Config{
		Sizes: inputs.sizes,
		Error: onError,
		Importer: importerFunc(func(path string) (*types.Package, error) {
			// While all of the import errors could be reported
			// based on the metadata before we start type checking,
			// reporting them via types.Importer places the errors
			// at the correct source location.
			id, ok := inputs.depsByImpPath[ImportPath(path)]
			if !ok {
				// If the import declaration is broken,
				// go list may fail to report metadata about it.
				// See TestFixImportDecl for an example.
				return nil, fmt.Errorf("missing metadata for import of %q", path)
			}
			depPH := inputs.deps[id]
			if depPH == nil {
				// e.g. missing metadata for dependencies in buildPackageHandle
				return nil, missingPkgError(path, inputs.moduleMode)
			}
			if !source.IsValidImport(inputs.pkgPath, depPH.m.PkgPath) {
				return nil, fmt.Errorf("invalid use of internal package %q", path)
			}
			return b.getImportPackage(ctx, id)
		}),
	}

	if inputs.goVersion != "" {
		goVersion := "go" + inputs.goVersion
		// types.NewChecker panics if GoVersion is invalid. An unparsable mod
		// file should probably stop us before we get here, but double check
		// just in case.
		if goVersionRx.MatchString(goVersion) {
			typesinternal.SetGoVersion(cfg, goVersion)
		}
	}

	// We want to type check cgo code if go/types supports it.
	// We passed typecheckCgo to go/packages when we Loaded.
	typesinternal.SetUsesCgo(cfg)
	return cfg
}

// depsErrors creates diagnostics for each metadata error (e.g. import cycle).
// These may be attached to import declarations in the transitive source files
// of pkg, or to 'requires' declarations in the package's go.mod file.
//
// TODO(rfindley): move this to load.go
func depsErrors(ctx context.Context, m *source.Metadata, meta *metadataGraph, fs source.FileSource, workspacePackages map[PackageID]PackagePath) ([]*source.Diagnostic, error) {
	// Select packages that can't be found, and were imported in non-workspace packages.
	// Workspace packages already show their own errors.
	var relevantErrors []*packagesinternal.PackageError
	for _, depsError := range m.DepsErrors {
		// Up to Go 1.15, the missing package was included in the stack, which
		// was presumably a bug. We want the next one up.
		directImporterIdx := len(depsError.ImportStack) - 1
		if directImporterIdx < 0 {
			continue
		}

		directImporter := depsError.ImportStack[directImporterIdx]
		if _, ok := workspacePackages[PackageID(directImporter)]; ok {
			continue
		}
		relevantErrors = append(relevantErrors, depsError)
	}

	// Don't build the import index for nothing.
	if len(relevantErrors) == 0 {
		return nil, nil
	}

	// Subsequent checks require Go files.
	if len(m.CompiledGoFiles) == 0 {
		return nil, nil
	}

	// Build an index of all imports in the package.
	type fileImport struct {
		cgf *source.ParsedGoFile
		imp *ast.ImportSpec
	}
	allImports := map[string][]fileImport{}
	for _, uri := range m.CompiledGoFiles {
		pgf, err := parseGoURI(ctx, fs, uri, source.ParseHeader)
		if err != nil {
			return nil, err
		}
		fset := tokeninternal.FileSetFor(pgf.Tok)
		// TODO(adonovan): modify Imports() to accept a single token.File (cgf.Tok).
		for _, group := range astutil.Imports(fset, pgf.File) {
			for _, imp := range group {
				if imp.Path == nil {
					continue
				}
				path := strings.Trim(imp.Path.Value, `"`)
				allImports[path] = append(allImports[path], fileImport{pgf, imp})
			}
		}
	}

	// Apply a diagnostic to any import involved in the error, stopping once
	// we reach the workspace.
	var errors []*source.Diagnostic
	for _, depErr := range relevantErrors {
		for i := len(depErr.ImportStack) - 1; i >= 0; i-- {
			item := depErr.ImportStack[i]
			if _, ok := workspacePackages[PackageID(item)]; ok {
				break
			}

			for _, imp := range allImports[item] {
				rng, err := imp.cgf.NodeRange(imp.imp)
				if err != nil {
					return nil, err
				}
				fixes, err := goGetQuickFixes(m.Module != nil, imp.cgf.URI, item)
				if err != nil {
					return nil, err
				}
				errors = append(errors, &source.Diagnostic{
					URI:            imp.cgf.URI,
					Range:          rng,
					Severity:       protocol.SeverityError,
					Source:         source.TypeError,
					Message:        fmt.Sprintf("error while importing %v: %v", item, depErr.Err),
					SuggestedFixes: fixes,
				})
			}
		}
	}

	modFile, err := nearestModFile(ctx, m.CompiledGoFiles[0], fs)
	if err != nil {
		return nil, err
	}
	pm, err := parseModURI(ctx, fs, modFile)
	if err != nil {
		return nil, err
	}

	// Add a diagnostic to the module that contained the lowest-level import of
	// the missing package.
	for _, depErr := range relevantErrors {
		for i := len(depErr.ImportStack) - 1; i >= 0; i-- {
			item := depErr.ImportStack[i]
			m := meta.metadata[PackageID(item)]
			if m == nil || m.Module == nil {
				continue
			}
			modVer := module.Version{Path: m.Module.Path, Version: m.Module.Version}
			reference := findModuleReference(pm.File, modVer)
			if reference == nil {
				continue
			}
			rng, err := pm.Mapper.OffsetRange(reference.Start.Byte, reference.End.Byte)
			if err != nil {
				return nil, err
			}
			fixes, err := goGetQuickFixes(true, pm.URI, item)
			if err != nil {
				return nil, err
			}
			errors = append(errors, &source.Diagnostic{
				URI:            pm.URI,
				Range:          rng,
				Severity:       protocol.SeverityError,
				Source:         source.TypeError,
				Message:        fmt.Sprintf("error while importing %v: %v", item, depErr.Err),
				SuggestedFixes: fixes,
			})
			break
		}
	}
	return errors, nil
}

// missingPkgError returns an error message for a missing package that varies
// based on the user's workspace mode.
func missingPkgError(pkgPath string, moduleMode bool) error {
	// TODO(rfindley): improve this error. Previous versions of this error had
	// access to the full snapshot, and could provide more information (such as
	// the initialization error).
	if moduleMode {
		// Previously, we would present the initialization error here.
		return fmt.Errorf("no required module provides package %q", pkgPath)
	} else {
		// Previously, we would list the directories in GOROOT and GOPATH here.
		return fmt.Errorf("cannot find package %q in GOROOT or GOPATH", pkgPath)
	}
}

type extendedError struct {
	primary     types.Error
	secondaries []types.Error
}

func (e extendedError) Error() string {
	return e.primary.Error()
}

// expandErrors duplicates "secondary" errors by mapping them to their main
// error. Some errors returned by the type checker are followed by secondary
// errors which give more information about the error. These are errors in
// their own right, and they are marked by starting with \t. For instance, when
// there is a multiply-defined function, the secondary error points back to the
// definition first noticed.
//
// This function associates the secondary error with its primary error, which can
// then be used as RelatedInformation when the error becomes a diagnostic.
//
// If supportsRelatedInformation is false, the secondary is instead embedded as
// additional context in the primary error.
func expandErrors(errs []types.Error, supportsRelatedInformation bool) []extendedError {
	var result []extendedError
	for i := 0; i < len(errs); {
		original := extendedError{
			primary: errs[i],
		}
		for i++; i < len(errs); i++ {
			spl := errs[i]
			if len(spl.Msg) == 0 || spl.Msg[0] != '\t' {
				break
			}
			spl.Msg = spl.Msg[1:]
			original.secondaries = append(original.secondaries, spl)
		}

		// Clone the error to all its related locations -- VS Code, at least,
		// doesn't do it for us.
		result = append(result, original)
		for i, mainSecondary := range original.secondaries {
			// Create the new primary error, with a tweaked message, in the
			// secondary's location. We need to start from the secondary to
			// capture its unexported location fields.
			relocatedSecondary := mainSecondary
			if supportsRelatedInformation {
				relocatedSecondary.Msg = fmt.Sprintf("%v (see details)", original.primary.Msg)
			} else {
				relocatedSecondary.Msg = fmt.Sprintf("%v (this error: %v)", original.primary.Msg, mainSecondary.Msg)
			}
			relocatedSecondary.Soft = original.primary.Soft

			// Copy over the secondary errors, noting the location of the
			// current error we're cloning.
			clonedError := extendedError{primary: relocatedSecondary, secondaries: []types.Error{original.primary}}
			for j, secondary := range original.secondaries {
				if i == j {
					secondary.Msg += " (this error)"
				}
				clonedError.secondaries = append(clonedError.secondaries, secondary)
			}
			result = append(result, clonedError)
		}

	}
	return result
}

// An importFunc is an implementation of the single-method
// types.Importer interface based on a function value.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
