package livepage_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/coder/websocket"
	am "github.com/pancsta/asyncmachine-go/pkg/machine"
	. "github.com/pancsta/go-app/pkg/app"
	"github.com/stretchr/testify/require"

	"github.com/pancsta/livepage"
)

const (
	idBtn   = "btn"
	idCount = "count"
)

// counter is a minimal model: a button increasing a counter.
type counter struct {
	livepage.Compo

	mach *am.Machine
	n    int
}

func (c *counter) Mach() *am.Machine { return c.mach }

func (c *counter) Render() UI {
	return Div().Body(
		Span().ID(idCount).Text(c.n),
		Button().ID(idBtn).Text("+").
			OnClick(func(app Context, e Event) {
				c.n += 1
			}),
	)
}

func newServer(t *testing.T) (*livepage.Page[*counter], *httptest.Server) {
	t.Helper()
	return newServerOpts(t, nil)
}

func newServerOpts(
	t *testing.T, configure func(*livepage.Opts),
) (*livepage.Page[*counter], *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := livepage.Opts{Debounce: 10 * time.Millisecond}
	if configure != nil {
		configure(&opts)
	}

	mach := am.New(ctx, am.Schema{am.StateReady: {}}, nil)
	page, err := livepage.New(ctx, &counter{mach: mach}, opts)
	require.NoError(t, err)

	srv := httptest.NewServer(page.NewHandler())
	t.Cleanup(srv.Close)

	return page, srv
}

func TestIndexHasScripts(t *testing.T) {
	_, srv := newServer(t)

	res, err := srv.Client().Get(srv.URL + "/")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	html := string(body)
	require.Contains(t, html, livepage.PathAssets+"jquery.js")
	require.Contains(t, html, livepage.PathAssets+"idiomorph.min.js")
	require.Contains(t, html, livepage.PathAssets+"livepage.js")
	require.Contains(t, html, `id="btn"`)
	require.Contains(t, html, "data-livepage-click=")

	for _, name := range []string{"jquery.js", "idiomorph.min.js", "livepage.js"} {
		res, err := srv.Client().Get(srv.URL + livepage.PathAssets + name)
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, 200, res.StatusCode, name)
	}
}

func TestExampleHtmlPage(t *testing.T) {
	tpl, err := os.ReadFile("testdata/example.html")
	require.NoError(t, err)
	require.Contains(t, string(tpl), "{{body}}")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mach := am.New(ctx, am.Schema{"Ready": {}}, nil)
	page, err := livepage.New(ctx, &counter{mach: mach}, livepage.Opts{
		HtmlPage: func(body string) string {
			return strings.Replace(string(tpl), "{{body}}", body, 1)
		},
	})
	require.NoError(t, err)
	t.Cleanup(mach.Dispose)

	srv := httptest.NewServer(page.NewHandler())
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/")
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	html := string(body)
	require.Contains(t, html, "<title>livepage example</title>")
	require.Contains(t, html, `id="btn"`)
	require.Contains(t, html, livepage.PathAssets+"livepage.js")
	require.NotContains(t, html, "{{body}}")
}

func TestWebsocket(t *testing.T) {
	page, srv := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+livepage.PathWs, nil)
	require.NoError(t, err)
	defer c.CloseNow()

	read := func() string {
		_, data, err := c.Read(ctx)
		require.NoError(t, err)
		var msg struct{ Type, Html string }
		require.NoError(t, json.Unmarshal(data, &msg))
		require.Equal(t, "html", msg.Type)
		return msg.Html
	}

	// initial state
	initial := read()
	require.Contains(t, initial, `id="count">0</span>`)
	handler := regexp.MustCompile(`data-livepage-click="([^"]+)"`).FindStringSubmatch(initial)
	require.Len(t, handler, 2)

	// click
	ev, _ := json.Marshal(map[string]string{"type": "click", "handler": handler[1]})
	require.NoError(t, c.Write(ctx, websocket.MessageText, ev))
	require.Contains(t, read(), `id="count">1</span>`)
	require.Equal(t, 1, page.Compo().n)
	require.Equal(t, 1, page.Clients())
}

func TestDiffHTML(t *testing.T) {
	page, srv := newServerOpts(t, func(opts *livepage.Opts) {
		opts.DiffHtml = true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+livepage.PathWs, nil)
	require.NoError(t, err)
	defer c.CloseNow()

	read := func() map[string]any {
		_, data, err := c.Read(ctx)
		require.NoError(t, err)
		var msg map[string]any
		require.NoError(t, json.Unmarshal(data, &msg))
		return msg
	}

	initial := read()
	require.Equal(t, "html", initial["type"])
	html, ok := initial["html"].(string)
	require.True(t, ok)

	page.Compo().n += 1
	page.HRefresh()

	msg := read()
	require.Equal(t, "diff", msg["type"])
	ops, ok := msg["ops"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, ops)

	next := ""
	consumed := 0
	for _, item := range ops {
		op := item.(map[string]any)
		pos := int(op["pos"].(float64))
		if pos > consumed {
			next += html[consumed:pos]
			consumed = pos
		}
		switch op["op"].(float64) {
		case -1:
			consumed = pos + int(op["len"].(float64))
		case 1:
			next += op["text"].(string)
		}
	}
	if consumed < len(html) {
		next += html[consumed:]
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(next))
	require.Equal(t, strconv.FormatUint(uint64(h.Sum32()), 16), msg["hash"])
	require.Contains(t, next, `id="count">1</span>`)
	require.NotEqual(t, html, next)

	// full redraw on request
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"redraw"}`)))
	redraw := read()
	require.Equal(t, "html", redraw["type"])
}

func TestBrowser(t *testing.T) {
	var chrome string
	for _, bin := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(bin); err == nil {
			chrome = p
			break
		}
	}
	if chrome == "" {
		t.Skip("no local browser found")
	}

	_, srv := newServerOpts(t, func(opts *livepage.Opts) {
		opts.DiffHtml = true
	})

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.NoSandbox,
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	// select {}

	err := chromedp.Do(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("#"+idBtn),
		// wait for the socket, otherwise the first click would be queued
		chromedp.Poll[chromedp.Void](`window.livepage && true`),
		chromedp.Click("#"+idBtn),
		chromedp.Click("#"+idBtn),
		// the DOM is morphed by the server push
		chromedp.Poll[chromedp.Void](`document.getElementById("count").textContent === "2"`),
	)
	require.NoError(t, err)

	libs, err := chromedp.Run(ctx,
		chromedp.Evaluate[string](`typeof $ + "," + typeof Idiomorph + "," + typeof window.livepage`),
	)
	require.NoError(t, err)

	count, err := chromedp.Run(ctx, chromedp.Text("#"+idCount))
	require.NoError(t, err)

	require.Equal(t, "function,object,object", libs)
	require.Equal(t, "2", count)
}
