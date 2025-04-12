package client

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Client struct {
	jsonClient     *JsonClient
	extraGenerator ExtraGenerator
	responses      chan *Response
	listenerStore  *listenerStore
	catchersStore  *sync.Map
	updatesTimeout time.Duration
	catchTimeout   time.Duration
}

type Option func(*Client)

func WithExtraGenerator(extraGenerator ExtraGenerator) Option {
	return func(client *Client) {
		client.extraGenerator = extraGenerator
	}
}

func WithCatchTimeout(timeout time.Duration) Option {
	return func(client *Client) {
		client.catchTimeout = timeout
	}
}

func WithProxy(req *AddProxyRequest) Option {
	return func(client *Client) {
		client.AddProxy(req)
	}
}

func WithLogVerbosity(req *SetLogVerbosityLevelRequest) Option {
	return func(client *Client) {
		client.SetLogVerbosityLevel(req)
	}
}

func NewClient(authorizationStateHandler AuthorizationStateHandler, options ...Option) (*Client, error) {
	client := &Client{
		jsonClient:    NewJsonClient(),
		responses:     make(chan *Response, 1000),
		listenerStore: newListenerStore(),
		catchersStore: &sync.Map{},
	}

	client.extraGenerator = UuidV4Generator()
	client.catchTimeout = 60 * time.Second

	tdlibInstance.addClient(client)
	go client.receiver()

	for _, option := range options {
		go option(client)
	}

	err := Authorize(client, authorizationStateHandler)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (client *Client) receiver() {
	for response := range client.responses {
		if response.Extra != "" {
			value, ok := client.catchersStore.Load(response.Extra)
			if ok {
				value.(chan *Response) <- response
			}
		}

		typ, err := UnmarshalType(response.Data)
		if err != nil {
			continue
		}

		needGc := false
		for _, listener := range client.listenerStore.Listeners() {
			if listener.IsActive() {
				listener.Updates <- typ
			} else {
				needGc = true
			}
		}
		if needGc {
			client.listenerStore.gc()
		}

		if typ.GetType() == TypeUpdateAuthorizationState && typ.(*UpdateAuthorizationState).AuthorizationState.AuthorizationStateType() == TypeAuthorizationStateClosed {
			//close(client.responses)
			return
		}
	}
}

func (client *Client) Send(req Request) (*Response, error) {
	req.Extra = client.extraGenerator()

	catcher := make(chan *Response, 1)

	client.catchersStore.Store(req.Extra, catcher)

	defer func() {
		client.catchersStore.Delete(req.Extra)
		close(catcher)
	}()

	client.jsonClient.Send(req)

	ctx, cancel := context.WithTimeout(context.Background(), client.catchTimeout)
	defer cancel()

	select {
	case response := <-catcher:
		return response, nil

	case <-ctx.Done():
		return nil, errors.New("response catching timeout")
	}
}

func (client *Client) GetListener() *Listener {
	listener := &Listener{
		isActive: true,
		Updates:  make(chan Type, 1000),
	}
	client.listenerStore.Add(listener)

	return listener
}

// CloseAndCleanup closes the TDLib instance and cleans up resources
func (client *Client) CloseAndCleanup() error {
	// 首先调用 TDLib 的 Close API
	_, err := client.Close()
	if err != nil {
		return err
	}

	// 关闭 JsonClient
	if client.jsonClient != nil {
		client.jsonClient.Close()
	}

	// 从全局实例中移除客户端
	tdlibInstance.removeClient(client)

	// 清理监听器和捕获器
	client.listenerStore.clear()
	client.catchersStore = &sync.Map{}

	// 关闭响应通道
	close(client.responses)

	return nil
}
