package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kr/pretty"
	"github.com/myitcv/govim"
	"github.com/myitcv/govim/cmd/govim/config"
	"github.com/myitcv/govim/cmd/govim/internal/lsp/protocol"
	"github.com/myitcv/govim/cmd/govim/types"
	"github.com/myitcv/govim/internal/plugin"
	"github.com/russross/blackfriday/v2"
)

type vimstate struct {
	plugin.Driver
	*govimplugin

	// buffers represents the current state of all buffers in Vim. It is only safe to
	// write and read to/from this map in the callback for a defined function, command
	// or autocommand.
	buffers map[int]*types.Buffer

	// jumpStack is a map from winid to jump position
	jumpStack map[int][]jumpPos

	// omnifunc calls happen in pairs (see :help complete-functions). The return value
	// from the first tells Vim where the completion starts, the return from the second
	// returns the matching words. This is by definition stateful. Hence we persist that
	// state here
	lastCompleteResults *protocol.CompletionList
}

func (v *vimstate) hello(args ...json.RawMessage) (interface{}, error) {
	return "Hello from function", nil
}

func (v *vimstate) helloComm(flags govim.CommandFlags, args ...string) error {
	v.ChannelEx(`echom "Hello from command"`)
	return nil
}

func (v *vimstate) balloonExpr(args ...json.RawMessage) (interface{}, error) {
	b, pos, err := v.mousePos()
	if err != nil {
		return nil, fmt.Errorf("failed to determine mouse position: %v", err)
	}
	go func() {
		params := &protocol.TextDocumentPositionParams{
			TextDocument: b.ToTextDocumentIdentifier(),
			Position:     pos.ToPosition(),
		}
		v.Logf("calling Hover: %v", pretty.Sprint(params))
		hovRes, err := v.server.Hover(context.Background(), params)
		if err != nil {
			v.ChannelCall("balloon_show", fmt.Sprintf("failed to get hover details: %v", err))
		} else {
			v.Logf("got Hover results: %q", hovRes.Contents.Value)
			md := []byte(hovRes.Contents.Value)
			plain := string(blackfriday.Run(md, blackfriday.WithRenderer(plainMarkdown{})))
			plain = strings.TrimSpace(plain)
			v.ChannelCall("balloon_show", strings.Split(plain, "\n"))
		}

	}()
	return "", nil
}

func (g *govimplugin) bufReadPost() error {
	b, err := g.fetchCurrentBufferInfo()
	if err != nil {
		return err
	}
	if cb, ok := g.buffers[b.Num]; ok {
		// reload of buffer, e.g. e!
		b.Version = cb.Version + 1
	} else {
		b.Version = 0
	}
	return g.handleBufferEvent(b)
}

func (g *govimplugin) bufTextChanged() error {
	b, err := g.fetchCurrentBufferInfo()
	if err != nil {
		return err
	}
	cb, ok := g.buffers[b.Num]
	if !ok {
		return fmt.Errorf("have not seen buffer %v (%v) - this should be impossible", b.Num, b.Name)
	}
	b.Version = cb.Version + 1
	return g.handleBufferEvent(b)
}

func (g *govimplugin) handleBufferEvent(b *types.Buffer) error {
	g.buffers[b.Num] = b

	if b.Version == 0 {
		params := &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:     string(b.URI()),
				Version: float64(b.Version),
				Text:    string(b.Contents),
			},
		}
		return g.server.DidOpen(context.Background(), params)
	}

	params := &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: b.ToTextDocumentIdentifier(),
			Version:                float64(b.Version),
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{
				Text: string(b.Contents),
			},
		},
	}
	return g.server.DidChange(context.Background(), params)
}

func (g *govimplugin) formatCurrentBuffer() error {
	var err error
	tool := g.ParseString(g.ChannelExpr(config.GlobalFormatOnSave))
	vp := g.Viewport()
	b := g.buffers[vp.Current.BufNr]

	var edits []protocol.TextEdit

	switch config.FormatOnSave(tool) {
	case config.FormatOnSaveGoFmt:
		params := &protocol.DocumentFormattingParams{
			TextDocument: b.ToTextDocumentIdentifier(),
		}
		g.Logf("Calling gopls.Formatting: %v", pretty.Sprint(params))
		edits, err = g.server.Formatting(context.Background(), params)
		if err != nil {
			return fmt.Errorf("failed to call gopls.Formatting: %v", err)
		}
	case config.FormatOnSaveGoImports:
		params := &protocol.CodeActionParams{
			TextDocument: b.ToTextDocumentIdentifier(),
		}
		g.Logf("Calling gopls.CodeAction: %v", pretty.Sprint(params))
		actions, err := g.server.CodeAction(context.Background(), params)
		if err != nil {
			return fmt.Errorf("failed to call gopls.CodeAction: %v", err)
		}
		want := 1
		if got := len(actions); want != got {
			return fmt.Errorf("got %v actions; expected %v", got, want)
		}
		edits = (*actions[0].Edit.Changes)[string(b.URI())]
	default:
		return fmt.Errorf("unknown format tool specified for %v: %v", config.GlobalFormatOnSave, tool)
	}

	preEventIgnore := g.ParseString(g.ChannelExpr("&eventignore"))
	g.ChannelEx("set eventignore=all")
	defer g.ChannelExf("set eventignore=%v", preEventIgnore)
	g.ToggleOnViewportChange()
	defer g.ToggleOnViewportChange()
	for ie := len(edits) - 1; ie >= 0; ie-- {
		e := edits[ie]
		g.Logf("%v", pretty.Sprint(e))
		start, err := types.PointFromPosition(b, e.Range.Start)
		if err != nil {
			return fmt.Errorf("failed to derive start point from position: %v", err)
		}
		end, err := types.PointFromPosition(b, e.Range.End)
		if err != nil {
			return fmt.Errorf("failed to derive end point from position: %v", err)
		}

		if start.Col() != 1 || end.Col() != 1 {
			// Whether this is a delete or not, we will implement support for this later
			return fmt.Errorf("saw an edit where start col != end col (range start: %v, range end: %v start: %v, end: %v). We can't currently handle this", e.Range.Start, e.Range.End, start, end)
		}

		if start.Line() != end.Line() {
			if e.NewText != "" {
				return fmt.Errorf("saw an edit where start line != end line with replacement text %q; We can't currently handle this", e.NewText)
			}
			// This is a delete of line
			if res := g.ParseInt(g.ChannelCall("deletebufline", b.Num, start.Line(), end.Line()-1)); res != 0 {
				return fmt.Errorf("deletebufline(%v, %v, %v) failed", b.Num, start.Line(), end.Line()-1)
			}
		} else {
			// do we have anything to do?
			if e.NewText == "" {
				continue
			}
			// we are within the same line so strip the newline
			if e.NewText[len(e.NewText)-1] == '\n' {
				e.NewText = e.NewText[:len(e.NewText)-1]
			}
			repl := strings.Split(e.NewText, "\n")
			g.ChannelCall("append", start.Line()-1, repl)
		}
	}
	return nil
}

func (v *vimstate) complete(args ...json.RawMessage) (interface{}, error) {
	// Params are: findstart int, base string
	findstart := v.ParseInt(args[0]) == 1

	if findstart {
		b, pos, err := v.cursorPos()
		if err != nil {
			return nil, fmt.Errorf("failed to get current position: %v", err)
		}
		params := &protocol.CompletionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{
					URI: string(b.URI()),
				},
				Position: pos.ToPosition(),
			},
		}
		v.Logf("gopls.Completion(%v)", pretty.Sprint(params))
		res, err := v.server.Completion(context.Background(), params)
		if err != nil {
			return nil, fmt.Errorf("called to gopls.Completion failed: %v", err)
		}

		v.lastCompleteResults = res
		return pos.Col(), nil
	} else {
		var matches []completionResult
		for _, i := range v.lastCompleteResults.Items {
			matches = append(matches, completionResult{
				Abbr: i.Label,
				Word: i.TextEdit.NewText,
				Info: i.Detail,
			})
		}

		return matches, nil
	}
}

type completionResult struct {
	Abbr string `json:"abbr"`
	Word string `json:"word"`
	Info string `json:"info"`
}

func (v *vimstate) gotoDef(flags govim.CommandFlags, args ...string) error {
	// We expect at most one argument that is the mode config.GoToDefMode
	var mode config.GoToDefMode
	if len(args) == 1 {
		mode = config.GoToDefMode(args[0])
		switch mode {
		case config.GoToDefModeTab, config.GoToDefModeSplit, config.GoToDefModeVsplit:
		default:
			return fmt.Errorf("unknown mode %q supplied", mode)
		}
	}

	cb, pos, err := v.cursorPos()
	if err != nil {
		return fmt.Errorf("failed to determine cursor position: %v", err)
	}
	params := &protocol.TextDocumentPositionParams{
		TextDocument: cb.ToTextDocumentIdentifier(),
		Position:     pos.ToPosition(),
	}
	locs, err := v.server.Definition(context.Background(), params)
	if err != nil {
		return fmt.Errorf("failed to call gopls.Definition: %v\nparams were: %v", err, pretty.Sprint(params))
	}
	v.Logf("called golspl.Definition(%v)", pretty.Sprint(params))
	v.Logf("result: %v", pretty.Sprint(locs))

	switch len(locs) {
	case 0:
		v.ChannelEx(`echorerr "No definition exists under cursor"`)
		return nil
	case 1:
	default:
		return fmt.Errorf("got multiple locations (%v); don't know how to handle this", len(locs))
	}

	loc := locs[0]

	// re-use the logic from vim-go:
	//
	// https://github.com/fatih/vim-go/blob/f04098811b8a7aba3dba699ed98f6f6e39b7d7ac/autoload/go/def.vim#L106

	oldSwitchBuf := v.ParseString(v.ChannelExpr("&switchbuf"))
	defer v.ChannelExf(`let &switchbuf=%q`, oldSwitchBuf)
	v.ChannelEx("normal! m'")

	cmd := "edit"
	if v.ParseInt(v.ChannelExpr("&modified")) == 1 {
		cmd = "hide edit"
	}

	// TODO implement remaining logic from vim-go if it
	// makes sense to do so

	// if a:mode == "tab"
	//   let &switchbuf = "useopen,usetab,newtab"
	//   if bufloaded(filename) == 0
	//     tab split
	//   else
	//      let cmd = 'sbuf'
	//   endif
	// elseif a:mode == "split"
	//   split
	// elseif a:mode == "vsplit"
	//   vsplit
	// endif

	v.ChannelExf("%v %v", cmd, strings.TrimPrefix(loc.URI, "file://"))

	vp := v.Viewport()
	nb := v.buffers[vp.Current.BufNr]
	newPos, err := types.PointFromPosition(nb, loc.Range.Start)
	if err != nil {
		return fmt.Errorf("failed to derive point from position: %v", err)
	}
	v.ChannelCall("cursor", newPos.Line(), newPos.Col())
	v.ChannelEx("normal! zz")

	return nil
}