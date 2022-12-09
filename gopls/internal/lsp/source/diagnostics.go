// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"

	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/span"
)

type SuggestedFix struct {
	Title      string
	Edits      map[span.URI][]protocol.TextEdit
	Command    *protocol.Command
	ActionKind protocol.CodeActionKind
}

type RelatedInformation struct {
	URI     span.URI
	Range   protocol.Range
	Message string
}

func Analyze(ctx context.Context, snapshot Snapshot, pkg Package, includeConvenience bool) (map[span.URI][]*Diagnostic, error) {
	// Exit early if the context has been canceled. This also protects us
	// from a race on Options, see golang/go#36699.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	options := snapshot.View().Options()
	categories := []map[string]*Analyzer{
		options.DefaultAnalyzers,
		options.StaticcheckAnalyzers,
	}
	if includeConvenience { // e.g. for codeAction
		categories = append(categories, options.ConvenienceAnalyzers) // e.g. fillstruct
	}
	var analyzers []*Analyzer
	for _, cat := range categories {
		for _, a := range cat {
			analyzers = append(analyzers, a)
		}
	}

	analysisDiagnostics, err := snapshot.Analyze(ctx, pkg.ID(), analyzers)
	if err != nil {
		return nil, err
	}

	reports := map[span.URI][]*Diagnostic{}
	// Report diagnostics and errors from root analyzers.
	for _, diag := range analysisDiagnostics {
		reports[diag.URI] = append(reports[diag.URI], diag)
	}
	return reports, nil
}

func FileDiagnostics(ctx context.Context, snapshot Snapshot, uri span.URI) (VersionedFileIdentity, []*Diagnostic, error) {
	fh, err := snapshot.GetVersionedFile(ctx, uri)
	if err != nil {
		return VersionedFileIdentity{}, nil, err
	}
	pkg, _, err := GetTypedFile(ctx, snapshot, fh, NarrowestPackage)
	if err != nil {
		return VersionedFileIdentity{}, nil, err
	}
	diagnostics, err := snapshot.DiagnosePackage(ctx, pkg)
	if err != nil {
		return VersionedFileIdentity{}, nil, err
	}
	analysisDiags, err := Analyze(ctx, snapshot, pkg, false)
	if err != nil {
		return VersionedFileIdentity{}, nil, err
	}
	fileDiags := append(diagnostics[fh.URI()], analysisDiags[fh.URI()]...)
	return fh.VersionedFileIdentity(), fileDiags, nil
}
