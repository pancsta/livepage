# livepage

**livepage** is a streaming HTML UI layer for [go-app](https://github.com/maxence-charriere/go-app) components
synchronized by [asyncmachine-go](https://github.com/pancsta/asyncmachine-go). It allows for unified WASM and
server-streamed HTML UIs. It's like **datastar**, but not for web pages.

Example component:

```go
// counter has a value and a button increasing the value.
type counter struct {
  livepage.Compo

  mach *am.Machine
  n    int
}

func (c *counter) Mach() *am.Machine { return c.mach }

func (c *counter) Render() UI {
  return Div().Body(
    Span().Text(c.n),
    Button().Text("+").
      OnClick(func(app Context, e Event) {
        c.n += 1
      }),
  )
}
```

Page initialization:

```go
import (
  am "github.com/pancsta/asyncmachine-go/pkg/machine"
  "github.com/pancsta/livepage"
)

// ...
  
mach := am.New(ctx, am.Schema{am.StateReady: {}}, nil)
page, err := livepage.New(ctx, &counter{mach: mach}, livepage.Opts{
  Debounce: 10 * time.Millisecond,
  DiffHtml: true,
})
srv := httptest.NewServer(page.NewHandler())
```

## How It Works

**livepage** does NOT have:

- URLs or links
- API endpoints or REST
- thick JS client
- magic attrs like `data-on:my-event="$foo = evt.detail"`
- JSON
- markup pre-processing / generation step

**livepage** DOES have:

- Golang only
- thin JS client
- DOM diff updates from [idiomorph](https://github.com/bigskysoftware/idiomorph)
- network diffs from [go-diff](https://github.com/sergi/go-diff)
- inline DOM handlers from [go-app](https://github.com/maxence-charriere/go-app)

The rendering happens on the event loop provided by [asyncmachine-go](https://github.com/pancsta/asyncmachine-go) and 
does not require locking. Multiple browsers connecting to a single livepage will see the same mirrored HTML projection
with realtime updates. Plain and simple.

## Why?

The Web is made to optimize the load on the server. This requirement goes away when developing desktop / active / live /
persistent / single-user apps. Instead of streaming raster data, this UI layer streams HTML. It can also be used to gain
access to the Delve debugger for go-app WASM apps which can now be fully deployed on the server using a regular
compilation target, while still rendering the UI in the browser.

## Status

This is an early version extracted from a prototype. Both the prototype and this version have been shipped to
production. Further work is expected, especially for deeper state machine integration via schemas and to support
additional transports (CDP, aRPC, IPC). ATM it depends on a thin fork of go-app.

## Related Work

- https://github.com/arturoeanton/go-echo-live-view
- https://github.com/starfederation/datastar
