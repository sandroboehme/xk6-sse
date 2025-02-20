package sse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/grafana/sobek"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/metrics"
)

type (
	sse struct {
		vu      modules.VU
		obj     *sobek.Object
		metrics *sseMetrics
	}
)

var ErrSSEInInitContext = common.NewInitContextError("using sse in the init context is not supported")

type Client struct {
	rt            *sobek.Runtime
	ctx           context.Context
	url           string
	resp          *http.Response
	eventHandlers map[string][]sobek.Callable
	done          chan struct{}
	shutdownOnce  sync.Once

	tagsAndMeta    *metrics.TagsAndMeta
	samplesOutput  chan<- metrics.SampleContainer
	builtinMetrics *metrics.BuiltinMetrics
	sseMetrics     *sseMetrics
	cancelRequest  context.CancelFunc
}

type HTTPResponse struct {
	URL     string            `json:"url"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Error   string            `json:"error"`
}

type Event struct {
	ID      string
	Comment string
	Name    string
	Data    string
}

type sseOpenArgs struct {
	setupFn     sobek.Callable
	headers     http.Header
	method      string
	body        string
	cookieJar   *cookiejar.Jar
	tagsAndMeta *metrics.TagsAndMeta
}

func (mi *sse) Exports() modules.Exports {
	return modules.Exports{Default: mi.obj}
}

func (mi *sse) Open(url string, args ...sobek.Value) (*HTTPResponse, error) {
	ctx := mi.vu.Context()
	rt := mi.vu.Runtime()
	state := mi.vu.State()
	if state == nil {
		return nil, ErrSSEInInitContext
	}

	parsedArgs, err := parseConnectArgs(state, rt, args...)
	if err != nil {
		return nil, err
	}

	parsedArgs.tagsAndMeta.SetSystemTagOrMetaIfEnabled(state.Options.SystemTags, metrics.TagURL, url)

	client, connEndHook, err := mi.open(ctx, state, rt, url, parsedArgs)
	defer connEndHook()
	if err != nil {
		client.handleEvent("error", rt.ToValue(err))
		if state.Options.Throw.Bool {
			return nil, err
		}
		return client.wrapHTTPResponse(err.Error()), nil
	}

	if _, err := parsedArgs.setupFn(sobek.Undefined(), rt.ToValue(&client)); err != nil {
		_ = client.closeResponseBody()
		return nil, err
	}

	client.handleEvent("open")

	readEventChan := make(chan Event, 1)  // Buffered channel to avoid blocking
	readErrChan := make(chan error, 1)
	readCloseChan := make(chan int, 1)

	go client.readEvents(readEventChan, readErrChan, readCloseChan)

	for {
		select {
		case event := <-readEventChan:
			fmt.Println("Event received:", event) // Ensure event is received
			metrics.PushIfNotDone(ctx, client.samplesOutput, metrics.Sample{
				TimeSeries: metrics.TimeSeries{
					Metric: client.sseMetrics.SSEEventReceived,
					Tags:   client.tagsAndMeta.Tags,
				},
				Time:     time.Now(),
				Metadata: client.tagsAndMeta.Metadata,
				Value:    1,
			})

			client.handleEvent("event", rt.ToValue(event))

		case readErr := <-readErrChan:
			client.handleEvent("error", rt.ToValue(readErr))

		case <-ctx.Done():
			_ = client.closeResponseBody()

		case <-readCloseChan:
			_ = client.closeResponseBody()

		case <-client.done:
			return client.wrapHTTPResponse(""), nil
		}
	}
}

func (c *Client) On(event string, handler sobek.Value) {
	if handler, ok := sobek.AssertFunction(handler); ok {
		c.eventHandlers[event] = append(c.eventHandlers[event], handler)
		fmt.Printf("Registered handler for event: %s\n", event) // Log registration
	} else {
		fmt.Printf("Failed to register handler for event: %s\n", event)
	}
}

func (c *Client) Close() error {
	err := c.closeResponseBody()
	c.cancelRequest()
	return err
}

func (c *Client) handleEvent(event string, args ...sobek.Value) {
	fmt.Printf("Handling event: %s\n", event) // Log event handling
	if handlers, ok := c.eventHandlers[event]; ok {
		for _, handler := range handlers {
			fmt.Printf("Invoking handler for event: %s\n", event) // Log handler invocation
			if _, err := handler(sobek.Undefined(), args...); err != nil {
				fmt.Printf("Error in handler for event: %s, error: %v\n", event, err)
				common.Throw(c.rt, err)
			}
		}
	} else {
		fmt.Printf("No handlers registered for event: %s\n", event)
	}
}

func (c *Client) readEvents(readChan chan Event, errorChan chan error, closeChan chan int) {
	reader := bufio.NewReader(c.resp.Body)
	ev := Event{}
	var buf bytes.Buffer

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("EOF reached; closing connection.")
				select {
				case closeChan <- -1:
					return
				case <-c.done:
					return
				}
			} else {
				fmt.Printf("Error reading line: %v\n", err)
				select {
				case errorChan <- err:
					return
				case <-c.done:
					return
				}
			}
		}

		fmt.Println("Received line:", string(line)) // Log received line

		switch {
		case hasPrefix(line, "id: "):
			ev.ID = stripPrefix(line, 4)
			fmt.Println("Parsed ID:", ev.ID)
		case hasPrefix(line, "event: "):
			ev.Name = stripPrefix(line, 7)
			fmt.Println("Parsed Event Name:", ev.Name)
		case hasPrefix(line, "data: "):
			buf.Write(line[6:])
			fmt.Println("Appending Data:", string(line[6:]))
		case bytes.Equal(line, []byte("\n")):
			ev.Data = strings.TrimRightFunc(buf.String(), func(r rune) bool {
				return r == '\r' || r == '\n'
			})

			fmt.Printf("Complete Event: ID=%s, Name=%s, Data=%s\n", ev.ID, ev.Name, ev.Data)

			select {
			case readChan <- ev:
				fmt.Println("Event sent to channel")
				buf.Reset()
				ev = Event{}
			case <-c.done:
				return
			}
		default:
			fmt.Println("Unknown line format:", string(line))
			select {
			case errorChan <- errors.New("unknown event: " + string(line)):
			case <-c.done:
				return
			}
		}
	}
}
