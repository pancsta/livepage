// Package livepage serves a go-app component as a live HTML page. The browser
// connects over a WebSocket (github.com/coder/websocket), forwards DOM events
// (click, change, submit) to the model, and receives re-rendered HTML, which is
// morphed into the document using Idiomorph. jQuery and Idiomorph are served by
// the package and injected into the <head> of every page.
//
// Usage:
//
//	type Model struct {
//		livepage.ModelBase
//		mach *am.Machine
//	}
//	// implement Render and Mach; use go-app event handlers like OnClick
//
//	page, err := livepage.New(ctx, model, livepage.Opts{})
//	http.ListenAndServe(":8080", page.Handler())
//
// State changes outside of DOM events should call [Page.HRefresh].
//
// Enable [Opts.DiffHtml] to push compact HTML diffs instead of full documents.
package livepage

import (
	"context"
	"embed"
	"encoding/json"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"
	amhelp "github.com/pancsta/asyncmachine-go/pkg/helpers"
	am "github.com/pancsta/asyncmachine-go/pkg/machine"
	h "github.com/pancsta/go-app/pkg/app"
	"github.com/sergi/go-diff/diffmatchpatch"
)

//go:embed assets/jquery.js assets/idiomorph.min.js assets/livepage.js
var assetsFs embed.FS

const (
	// DefaultDebounce is the default delay of pushing HTML to the clients.
	DefaultDebounce = 200 * time.Millisecond
	// PathWs is the WebSocket endpoint, relative to [Opts.Prefix].
	PathWs = "/ws"
	// PathAssets is the directory with served scripts, relative to
	// [Opts.Prefix].
	PathAssets = "/assets/"

	evalSource = "livepage.dom"
	maxMsgSize = 1 << 20
)

// DomEvent is a DOM event forwarded from the browser.
type DomEvent struct {
	// Type is one of click, change, submit.
	Type string

	// attrs
	Id    string
	Name  string
	Value string
	// data-livepage attr
	Data string
	// Handler is the ID of a go-app event handler bound to the target.
	Handler string

	// event payload
	Key        string  `json:",omitempty"`
	Code       string  `json:",omitempty"`
	Button     int     `json:",omitempty"`
	ClientX    float64 `json:",omitempty"`
	ClientY    float64 `json:",omitempty"`
	DeltaX     float64 `json:",omitempty"`
	DeltaY     float64 `json:",omitempty"`
	ScrollTop  float64 `json:",omitempty"`
	ScrollLeft float64 `json:",omitempty"`

	// meta
	Path string

	// internal
	Event *am.Event `json:"-"`
}

type htmlMsg struct {
	Type string `json:"type"`
	Html string `json:"html"`
}

type diffMsg struct {
	Type string   `json:"type"`
	Ops  []diffOp `json:"ops"`
	// Hash is the FNV-1a hash of the resulting HTML.
	Hash string `json:"hash"`
}

type diffOp struct {
	// Op is -1 delete, 1 insert.
	Op int8 `json:"op"`
	// Pos is a UTF-16 offset in the previous HTML document.
	Pos int `json:"pos"`
	// Len is the number of UTF-16 units to delete.
	Len int `json:"len,omitempty"`
	// Text is inserted content.
	Text string `json:"text,omitempty"`
}

// ///// ///// /////

// ///// COMPO

// ///// ///// /////

// CompoLiveApi is a live component. It has to embed [Compo] (or h.Compo and
// implement Update on its own).
type CompoLiveApi interface {
	h.Composer

	// Mach is the machine used to serialize DOM handlers and rendering.
	Mach() *am.Machine
	// Update flags the component for re-render.
	Update()
	PageName() string
}

type Compo struct {
	h.Compo
	app atomic.Pointer[h.Context]
}

// OnMount stores the component's context.
func (c *Compo) OnMount(ctx h.Context) {
	c.app.Store(&ctx)
}

// App returns the component's app context, if mounted.
func (c *Compo) App() *h.Context {
	return c.app.Load()
}

// Update flags the component for re-render. No-op until mounted.
func (c *Compo) Update() {
	if app := c.app.Load(); app != nil {
		app.Update()
	}
}

func (c *Compo) PageName() string {
	return "livepage"
}

// Opts configures a [Page].
type Opts struct {
	// HtmlPage wraps the rendered component into a whole HTML document. Scripts
	// are injected into its <head>. Defaults to [DefaultHtmlPage].
	HtmlPage func(body string) string
	// Debounce delay of pushing HTML to the clients. Defaults to
	// [DefaultDebounce].
	Debounce time.Duration
	// DiffHtml sends HTML diffs instead of full documents, using go-diff on
	// the server and a JS equivalent in the browser. Default false.
	DiffHtml bool
	// Prefix of all the paths served by [Page.NewHandler], eg "/live". Optional.
	Prefix string
	// OriginPatterns are additionally allowed WebSocket origins (host
	// patterns). Same origin is always allowed.
	OriginPatterns []string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// DefaultHtmlPage is a minimal HTML document.
func DefaultHtmlPage(body string) string {
	return "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>livepage</title></head><body>" +
		body + "</body></html>"
}

// ///// ///// /////

// ///// PAGE

// ///// ///// /////

// Page binds a [CompoLiveApi] to many browsers.
type Page[M CompoLiveApi] struct {
	compo M
	ctx   context.Context
	renderer h.TestEngine
	opts     Opts
	log      *slog.Logger

	latestHtml atomic.Pointer[string]

	pushMx    sync.Mutex
	pushTimer *time.Timer
	// pushNowMx serializes renders/diffs, keeping latestHtml ordered.
	pushNowMx sync.Mutex

	// TODO slices, not a map
	clientsMx sync.Mutex
	clients   map[*websocket.Conn]struct{}

	handlersMx   sync.RWMutex
	handlers     map[string]h.EventHandler
	prevHandlers map[string]h.EventHandler

	Disposed atomic.Bool
}

// New mounts the model and creates a [Page], which lives as long as ctx.
func New[M CompoLiveApi](ctx context.Context, model M, opts Opts) (*Page[M], error) {
	if opts.HtmlPage == nil {
		opts.HtmlPage = DefaultHtmlPage
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	opts.Prefix = strings.TrimRight(opts.Prefix, "/")
	// TODO check if compo mach implements schema (Prefix.Connected, Prefix.Basic)
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	p := &Page[M]{
		compo:        model,
		ctx:          ctx,
		renderer:     h.NewTestEngine(),
		opts:         opts,
		log:          log,
		clients:      map[*websocket.Conn]struct{}{},
		handlers:     map[string]h.EventHandler{},
		prevHandlers: map[string]h.EventHandler{},
	}

	err := p.renderer.Load(model)
	if err != nil {
		return nil, err
	}
	p.renderer.ConsumeNext()
	if ctx.Err() != nil {
		return nil, ctx.Err() // expired
	}

	// dispose
	model.Mach().OnDispose(func(id string, ctx context.Context) {
		p.Dispose()
	})

	return p, nil
}

func (p *Page[M]) Dispose() {
	p.Disposed.Store(true)
	p.stopPush()
	p.closeClients()
}

// Compo returns the underlying component.
func (p *Page[M]) Compo() M {
	return p.compo
}

// Clients returns the number of connected browsers.
func (p *Page[M]) Clients() int {
	p.clientsMx.Lock()
	defer p.clientsMx.Unlock()
	return len(p.clients)
}

// Push schedules (debounced) pushing of the current HTML to all the clients.
// Nothing is sent when the HTML didn't change.
func (p *Page[M]) Push() {
	// TODO accept html string, dont eval in pushNow
	p.pushMx.Lock()
	defer p.pushMx.Unlock()
	if p.ctx.Err() != nil {
		return // expired
	}
	if p.pushTimer != nil {
		p.pushTimer.Stop()
	}
	p.pushTimer = time.AfterFunc(p.opts.Debounce, p.pushNow)
}

// stopPush cancels a scheduled push.
func (p *Page[M]) stopPush() {
	p.pushMx.Lock()
	defer p.pushMx.Unlock()
	if p.pushTimer != nil {
		p.pushTimer.Stop()
	}
}

// HRefresh re-renders the model and pushes the HTML. It is safe to call from
// state handlers and Eval().
func (p *Page[M]) HRefresh() {
	p.compo.Update()
	p.renderer.ConsumeAll()
	p.Push()
}

// Refresh is threadsafe version of HRefresh and should not be called from state handlers or eval.
func (p *Page[M]) Refresh(ctx context.Context) {
	p.compo.Mach().Eval("livepage-"+p.compo.PageName(), p.HRefresh, ctx)
}

// NewHandler serves the page, scripts and the WebSocket endpoint.
func (p *Page[M]) NewHandler() http.Handler {
	pre := p.opts.Prefix
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pre+"/{$}", p.servePage)
	mux.HandleFunc("GET "+pre+PathWs, p.serveWs)

	sub, _ := fs.Sub(assetsFs, "assets")
	mux.Handle("GET "+pre+PathAssets,
		http.StripPrefix(pre+PathAssets, http.FileServerFS(sub)))

	return mux
}

// HTML returns the full HTML document with the current state, as sent to the
// browsers. It returns false when rendering timed out.
func (p *Page[M]) HTML() (string, bool) {
	var body string
	ok := p.compo.Mach().Eval(evalSource, func() {
		p.handlersMx.Lock()
		p.prevHandlers = p.handlers
		p.handlers = map[string]h.EventHandler{}
		p.handlersMx.Unlock()

		body = h.HTMLStringWithEvents(p.compo, p.registerHandler)
	}, p.ctx)
	if !ok {
		return "", false
	}

	return injectHead(p.opts.HtmlPage(body), p.headTags()), true
}

// currentHtml returns the last rendered HTML, rendering on demand.
func (p *Page[M]) currentHtml() (string, bool) {
	p.pushNowMx.Lock()
	defer p.pushNowMx.Unlock()

	if html := p.latestHtml.Load(); html != nil {
		return *html, true
	}
	html, ok := p.HTML()
	if ok {
		p.latestHtml.Store(&html)
	}
	return html, ok
}

// registerHandler binds a go-app event handler and tags the element with its ID.
func (p *Page[M]) registerHandler(
	element h.HTML, event string, handler h.EventHandler,
) bool {
	if handler == nil {
		return false
	}

	id := amhelp.RandId(8)
	p.handlersMx.Lock()
	p.handlers[id] = handler
	p.handlersMx.Unlock()

	element.SetAttr("data-livepage-"+event, id)
	return true
}

func (p *Page[M]) headTags() string {
	pre := p.opts.Prefix + PathAssets
	return `<script src="` + pre + `jquery.js"></script>` +
		`<script src="` + pre + `idiomorph.min.js"></script>` +
		`<script src="` + pre + `livepage.js" data-ws="` + p.opts.Prefix + PathWs + `"></script>`
}

func (p *Page[M]) pushNow() {
	p.pushNowMx.Lock()
	defer p.pushNowMx.Unlock()

	html, ok := p.HTML()
	if !ok {
		p.log.Warn("livepage: render timeout")
		return
	}
	prev := p.latestHtml.Load()
	if prev != nil && *prev == html {
		return
	}
	p.latestHtml.Store(&html)

	if p.opts.DiffHtml && prev != nil {
		p.broadcastDiff(*prev, html)
		return
	}
	p.broadcast(html)
}

func (p *Page[M]) broadcast(html string) {
	p.clientsMx.Lock()
	conns := slices.Collect(maps.Keys(p.clients))
	p.clientsMx.Unlock()

	for _, c := range conns {
		p.sendHtml(c, html)
	}
}

func (p *Page[M]) broadcastDiff(prev, html string) {
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = 100 * time.Millisecond
	diffs := dmp.DiffMain(prev, html, true)

	ops := make([]diffOp, 0, len(diffs))
	pos := 0
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			pos += utf16Len(d.Text)
		case diffmatchpatch.DiffDelete:
			n := utf16Len(d.Text)
			ops = append(ops, diffOp{Op: -1, Pos: pos, Len: n})
			pos += n
		case diffmatchpatch.DiffInsert:
			ops = append(ops, diffOp{Op: 1, Pos: pos, Text: d.Text})
		}
	}
	msg := diffMsg{
		Type: "diff",
		Ops:  ops,
		Hash: htmlHash(html),
	}

	p.clientsMx.Lock()
	conns := slices.Collect(maps.Keys(p.clients))
	p.clientsMx.Unlock()
	for _, c := range conns {
		p.send(c, msg)
	}
}

func (p *Page[M]) sendHtml(c *websocket.Conn, html string) {
	p.send(c, htmlMsg{Type: "html", Html: html})
}

func (p *Page[M]) send(c *websocket.Conn, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		p.log.Warn("livepage: marshal", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		p.log.Debug("livepage: write", "err", err)
		_ = c.CloseNow()
	}
}

func (p *Page[M]) closeClients() {
	p.clientsMx.Lock()
	defer p.clientsMx.Unlock()
	for c := range p.clients {
		_ = c.Close(websocket.StatusGoingAway, "page disposed")
		delete(p.clients, c)
	}
}

func (p *Page[M]) servePage(w http.ResponseWriter, _ *http.Request) {
	if p.Disposed.Load() {
		http.Error(w, "page disposed", http.StatusServiceUnavailable)
		return
	}

	p.pushNowMx.Lock()
	html, ok := p.HTML()
	if ok {
		p.latestHtml.Store(&html)
	}
	p.pushNowMx.Unlock()
	if !ok {
		http.Error(w, "render timeout", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

func (p *Page[M]) serveWs(w http.ResponseWriter, r *http.Request) {
	if p.Disposed.Load() {
		http.Error(w, "page disposed", http.StatusServiceUnavailable)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: p.opts.OriginPatterns,
	})
	if err != nil {
		return // Accept already responded
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMsgSize)

	p.clientsMx.Lock()
	p.clients[c] = struct{}{}
	p.clientsMx.Unlock()
	defer func() {
		p.clientsMx.Lock()
		delete(p.clients, c)
		p.clientsMx.Unlock()
	}()

	// sync the current state
	if html, ok := p.currentHtml(); ok {
		p.sendHtml(c, html)
	}

	// wait for DOM events
	for {
		_, data, err := c.Read(p.ctx)
		if p.ctx.Err() != nil {
			return // expired
		}
		if err != nil {
			if websocket.CloseStatus(err) == -1 {
				p.log.Debug("livepage: read", "err", err)
			}
			return
		}

		var ev DomEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Type == "redraw" {
			if html, ok := p.currentHtml(); ok {
				p.sendHtml(c, html)
			}
			continue
		}
		ev.Event = am.CtxToEv(p.ctx)
		p.dispatch(&ev)
	}
}

// dispatch runs the DOM handlers and re-renders on the machine's queue, then
// pushes the new HTML.
func (p *Page[M]) dispatch(ev *DomEvent) {
	// TODO state
	ok := p.compo.Mach().Eval(evalSource, func() {
		if ev.Handler != "" {
			p.handlersMx.RLock()
			handler := p.handlers[ev.Handler]
			if handler == nil {
				handler = p.prevHandlers[ev.Handler]
			}
			p.handlersMx.RUnlock()
			if handler != nil {
				ctx := h.Context{}
				if live, ok := any(p.compo).(interface{ LiveContext() h.Context }); ok {
					ctx = live.LiveContext()
				}
				data := map[string]string{
					"type":       ev.Type,
					"id":         ev.Id,
					"name":       ev.Name,
					"value":      ev.Value,
					"data":       ev.Data,
					"path":       ev.Path,
					"key":        ev.Key,
					"code":       ev.Code,
					"button":     strconv.Itoa(ev.Button),
					"clientX":    strconv.FormatFloat(ev.ClientX, 'f', -1, 64),
					"clientY":    strconv.FormatFloat(ev.ClientY, 'f', -1, 64),
					"deltaX":     strconv.FormatFloat(ev.DeltaX, 'f', -1, 64),
					"deltaY":     strconv.FormatFloat(ev.DeltaY, 'f', -1, 64),
					"scrollTop":  strconv.FormatFloat(ev.ScrollTop, 'f', -1, 64),
					"scrollLeft": strconv.FormatFloat(ev.ScrollLeft, 'f', -1, 64),
				}
				handler(ctx, h.Event{Data: data})
			}
		}

		// render
		p.compo.Update()
		p.renderer.ConsumeAll()
	}, p.ctx)
	if !ok {
		return
	}

	p.Push()
}

// ///// ///// /////

// ///// UTILS

// ///// ///// /////

// ToRef dumps a reference for a node within a DOM chain for further processing inside deferred funcs. It prevents the
// chain from being broken up.
func ToRef[T h.UI](target *T, elem T) T {
	*target = elem
	return elem
}

// injectHead adds tags at the end of <head> (or at the start of the document).
func injectHead(doc, tags string) string {
	i := strings.Index(strings.ToLower(doc), "</head>")
	if i < 0 {
		return tags + doc
	}

	return doc[:i] + tags + doc[i:]
}

func htmlHash(html string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(html))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
