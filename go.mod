module github.com/pancsta/livepage

go 1.26.5

//replace github.com/pancsta/asyncmachine-go => ../asyncmachine-go
//
//replace github.com/pancsta/go-app => ../go-app

require (
	github.com/chromedp/chromedp v0.21.0
	github.com/coder/websocket v1.8.15
	github.com/pancsta/asyncmachine-go v0.19.5
	github.com/pancsta/go-app v0.0.0-20261030132146-e4194936713d
	github.com/sergi/go-diff v1.4.0
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/alitto/pond/v2 v2.7.1 // indirect
	github.com/chromedp/cdproto v0.157.8 // indirect
	github.com/failsafe-go/failsafe-go v0.6.8 // indirect
	github.com/go-json-experiment/json v0.0.0-20260213210345-44df1a37e875 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/lithammer/dedent v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
