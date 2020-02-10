// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"bytes"
	"context"
	"fmt"

	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/jsonrpc2"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/protocol"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/span"
	errors "golang.org/x/xerrors"
)

func (s *Server) didOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	_, err := s.didModifyFiles(ctx, []source.FileModification{
		{
			URI:        span.NewURI(params.TextDocument.URI),
			Action:     source.Open,
			Version:    params.TextDocument.Version,
			Text:       []byte(params.TextDocument.Text),
			LanguageID: params.TextDocument.LanguageID,
		},
	})
	return err
}

func (s *Server) didChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := span.NewURI(params.TextDocument.URI)
	text, err := s.changedText(ctx, uri, params.ContentChanges)
	if err != nil {
		return err
	}
	c := source.FileModification{
		URI:     uri,
		Action:  source.Change,
		Version: params.TextDocument.Version,
		Text:    text,
	}
	snapshots, err := s.didModifyFiles(ctx, []source.FileModification{c})
	if err != nil {
		return err
	}
	snapshot := snapshots[uri]
	if snapshot == nil {
		return errors.Errorf("no snapshot for %s", uri)
	}
	// Ideally, we should be able to specify that a generated file should be opened as read-only.
	// Tell the user that they should not be editing a generated file.
	if s.wasFirstChange(uri) && source.IsGenerated(ctx, snapshot, uri) {
		if err := s.client.ShowMessage(ctx, &protocol.ShowMessageParams{
			Message: fmt.Sprintf("Do not edit this file! %s is a generated file.", uri.Filename()),
			Type:    protocol.Warning,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) didChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	var modifications []source.FileModification
	for _, change := range params.Changes {
		modifications = append(modifications, source.FileModification{
			URI:    span.NewURI(change.URI),
			Action: changeTypeToFileAction(change.Type),
			OnDisk: true,
		})
	}
	_, err := s.didModifyFiles(ctx, modifications)
	return err
}

func (s *Server) didSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) error {
	c := source.FileModification{
		URI:     span.NewURI(params.TextDocument.URI),
		Action:  source.Save,
		Version: params.TextDocument.Version,
	}
	if params.Text != nil {
		c.Text = []byte(*params.Text)
	}
	_, err := s.didModifyFiles(ctx, []source.FileModification{c})
	return err
}

func (s *Server) didClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	_, err := s.didModifyFiles(ctx, []source.FileModification{
		{
			URI:     span.NewURI(params.TextDocument.URI),
			Action:  source.Close,
			Version: -1,
			Text:    nil,
		},
	})
	return err
}

func (s *Server) didModifyFiles(ctx context.Context, modifications []source.FileModification) (map[span.URI]source.Snapshot, error) {
	snapshots, err := s.session.DidModifyFiles(ctx, modifications)
	if err != nil {
		return nil, err
	}
	snapshotByURI := make(map[span.URI]source.Snapshot)
	for _, c := range modifications {
		snapshotByURI[c.URI] = nil
	}
	// Avoid diagnosing the same snapshot twice.
	snapshotSet := make(map[source.Snapshot][]span.URI)
	for uri := range snapshotByURI {
		view, err := s.session.ViewOf(uri)
		if err != nil {
			return nil, err
		}
		var snapshot source.Snapshot
		for _, s := range snapshots {
			if s.View() == view {
				if snapshot != nil {
					return nil, errors.Errorf("duplicate snapshots for the same view")
				}
				snapshot = s
			}
		}
		// If the file isn't in any known views (for example, if it's in a dependency),
		// we may not have a snapshot to map it to. As a result, we won't try to
		// diagnose it. TODO(rstambler): Figure out how to handle this better.
		if snapshot == nil {
			continue
		}
		snapshotByURI[uri] = snapshot
		snapshotSet[snapshot] = append(snapshotSet[snapshot], uri)
	}
	for snapshot, uris := range snapshotSet {
		for _, uri := range uris {
			fh, err := snapshot.GetFile(uri)
			if err != nil {
				return nil, err
			}
			// If a modification comes in for the view's go.mod file and the view
			// was never properly initialized, or the view does not have
			// a go.mod file, try to recreate the associated view.
			switch fh.Identity().Kind {
			case source.Mod:
				modfile, _ := snapshot.View().ModFiles()
				if modfile != "" || fh.Identity().URI != modfile {
					continue
				}
				newSnapshot, err := snapshot.View().Rebuild(ctx)
				if err != nil {
					return nil, err
				}
				// Update the snapshot to the rebuilt one.
				snapshot = newSnapshot
				snapshotByURI[uri] = snapshot
			}
		}
		go s.diagnoseSnapshot(snapshot)
	}
	return snapshotByURI, nil
}

func (s *Server) wasFirstChange(uri span.URI) bool {
	if s.changedFiles == nil {
		s.changedFiles = make(map[span.URI]struct{})
	}
	_, ok := s.changedFiles[uri]
	return ok
}

func (s *Server) changedText(ctx context.Context, uri span.URI, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	if len(changes) == 0 {
		return nil, jsonrpc2.NewErrorf(jsonrpc2.CodeInternalError, "no content changes provided")
	}

	// Check if the client sent the full content of the file.
	// We accept a full content change even if the server expected incremental changes.
	if len(changes) == 1 && changes[0].Range == nil && changes[0].RangeLength == 0 {
		return []byte(changes[0].Text), nil
	}
	return s.applyIncrementalChanges(ctx, uri, changes)
}

func (s *Server) applyIncrementalChanges(ctx context.Context, uri span.URI, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	content, _, err := s.session.GetFile(uri).Read(ctx)
	if err != nil {
		return nil, jsonrpc2.NewErrorf(jsonrpc2.CodeInternalError, "file not found (%v)", err)
	}
	for _, change := range changes {
		// Make sure to update column mapper along with the content.
		converter := span.NewContentConverter(uri.Filename(), content)
		m := &protocol.ColumnMapper{
			URI:       uri,
			Converter: converter,
			Content:   content,
		}
		if change.Range == nil {
			return nil, jsonrpc2.NewErrorf(jsonrpc2.CodeInternalError, "unexpected nil range for change")
		}
		spn, err := m.RangeSpan(*change.Range)
		if err != nil {
			return nil, err
		}
		if !spn.HasOffset() {
			return nil, jsonrpc2.NewErrorf(jsonrpc2.CodeInternalError, "invalid range for content change")
		}
		start, end := spn.Start().Offset(), spn.End().Offset()
		if end < start {
			return nil, jsonrpc2.NewErrorf(jsonrpc2.CodeInternalError, "invalid range for content change")
		}
		var buf bytes.Buffer
		buf.Write(content[:start])
		buf.WriteString(change.Text)
		buf.Write(content[end:])
		content = buf.Bytes()
	}
	return content, nil
}

func changeTypeToFileAction(ct protocol.FileChangeType) source.FileAction {
	switch ct {
	case protocol.Changed:
		return source.Change
	case protocol.Created:
		return source.Create
	case protocol.Deleted:
		return source.Delete
	}
	return source.UnknownFileAction
}
